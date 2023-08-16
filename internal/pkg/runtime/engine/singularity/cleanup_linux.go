// Copyright (c) 2018-2021, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/sylabs/singularity/v4/internal/pkg/instance"
	fakerootConfig "github.com/sylabs/singularity/v4/internal/pkg/runtime/engine/fakeroot/config"
	"github.com/sylabs/singularity/v4/internal/pkg/util/bin"
	"github.com/sylabs/singularity/v4/internal/pkg/util/crypt"
	"github.com/sylabs/singularity/v4/internal/pkg/util/priv"
	"github.com/sylabs/singularity/v4/internal/pkg/util/starter"
	"github.com/sylabs/singularity/v4/pkg/runtime/engine/config"
	"github.com/sylabs/singularity/v4/pkg/sylog"
	"github.com/sylabs/singularity/v4/pkg/util/capabilities"
)

// CleanupContainer is called from master after the MonitorContainer returns.
// It is responsible for ensuring that the container has been properly torn down.
//
// Additional privileges may be gained when running
// in suid flow. However, when a user namespace is requested and it is not
// a hybrid workflow (e.g. fakeroot), then there is no privileged saved uid
// and thus no additional privileges can be gained.
//
// For better understanding of runtime flow in general refer to
// https://github.com/opencontainers/runtime-spec/blob/master/runtime.md#lifecycle.
// CleanupContainer is performing step 8/9 here.
func (e *EngineOperations) CleanupContainer(ctx context.Context, _ error, _ syscall.WaitStatus) error {
	// firstly stop all fuse drivers before any image removal
	// by image driver interruption or image cleanup for hybrid
	// fakeroot workflow
	e.stopFuseDrivers()

	if e.EngineConfig.GetImageFuse() {
		// Lazy unmount so any underlay mount points don't block umount call against rootfs,
		// which must be unmounted from master to proceed with the host cleanup.
		// Everything is tidied up as the container mount namespace disappears.
		if err := umount(true); err != nil {
			sylog.Errorf("%s", err)
		}
	}

	if tempDir := e.EngineConfig.GetDeleteTempDir(); tempDir != "" && !e.EngineConfig.GetImageFuse() {
		sylog.Verbosef("Removing image tempDir %s", tempDir)
		sylog.Infof("Cleaning up image...")

		var err error

		if e.EngineConfig.GetFakeroot() && os.Getuid() != 0 {
			// this is required when we are using SUID workflow
			// because master process is not in the fakeroot
			// context and can get permission denied error during
			// image removal, so we execute "rm -rf /tmp/image" via
			// the fakeroot engine
			err = fakerootCleanup(tempDir)
		} else {
			err = os.RemoveAll(tempDir)
		}
		if err != nil {
			sylog.Errorf("failed to delete container image tempDir %s: %s", tempDir, err)
		}
	}

	if networkSetup != nil {
		net := e.EngineConfig.GetNetwork()
		privileged := false
		// If a CNI configuration was allowed as non-root (or fakeroot)
		if net != "none" && os.Geteuid() != 0 {
			priv.Escalate()
			privileged = true
		}
		sylog.Debugf("Cleaning up CNI network config %s", net)
		if err := networkSetup.DelNetworks(ctx); err != nil {
			sylog.Errorf("could not delete networks: %v", err)
		}
		if privileged {
			priv.Drop()
		}
	}

	if cgroupsManager != nil {
		if err := cgroupsManager.Destroy(); err != nil {
			sylog.Warningf("failed to remove cgroup configuration: %v", err)
		}
	}

	if cryptDev != "" {
		if err := cleanupCrypt(cryptDev); err != nil {
			sylog.Errorf("could not cleanup crypt: %v", err)
		}
	}

	if e.EngineConfig.GetInstance() {
		file, err := instance.Get(e.CommonConfig.ContainerID, instance.SingSubDir)
		if err != nil {
			return err
		}
		return file.Delete()
	}

	return nil
}

func umount(lazy bool) (err error) {
	var oldEffective uint64

	caps := uint64(0)
	caps |= uint64(1 << capabilities.Map["CAP_SYS_ADMIN"].Value)

	umountFlags := 0
	if lazy {
		umountFlags = syscall.MNT_DETACH
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	oldEffective, err = capabilities.SetProcessEffective(caps)
	if err != nil {
		return err
	}
	defer func() {
		_, e := capabilities.SetProcessEffective(oldEffective)
		if err == nil {
			err = e
		}
	}()

	for i := len(umountPoints) - 1; i >= 0; i-- {
		p := umountPoints[i]
		sylog.Debugf("Umount %s", p)
		retries := 0
	retry:
		err = syscall.Unmount(p, umountFlags)
		// ignore EINVAL meaning it's not a mount point
		//nolint:forcetypeassert
		if err != nil && err.(syscall.Errno) != syscall.EINVAL {
			// when rootfs mount point is a sandbox, the unmount
			// fail more often with EBUSY, but it's just a matter of
			// time before resources are released by the kernel so we
			// retry until the unmount operation succeed (retries 10 times)
			//nolint:forcetypeassert
			if err.(syscall.Errno) == syscall.EBUSY && retries < 10 {
				retries++
				goto retry
			}
			return fmt.Errorf("while unmounting %s directory: %s", p, err)
		}
	}

	return err
}

func cleanupCrypt(path string) error {
	// Strict umount to be sure crypt device not in use
	if err := umount(false); err != nil {
		return err
	}

	devName := filepath.Base(path)

	cryptDev := &crypt.Device{}
	if err := cryptDev.CloseCryptDevice(devName); err != nil {
		return fmt.Errorf("unable to delete crypt device: %s", devName)
	}

	return nil
}

func fakerootCleanup(path string) error {
	rm, err := bin.FindBin("rm")
	if err != nil {
		return err
	}

	command := []string{rm, "-rf", path}

	sylog.Debugf("Calling fakeroot engine to execute %q", strings.Join(command, " "))

	cfg := &config.Common{
		EngineName:   fakerootConfig.Name,
		ContainerID:  "fakeroot",
		EngineConfig: &fakerootConfig.EngineConfig{Args: command},
	}

	return starter.Run(
		"Singularity fakeroot",
		cfg,
		starter.UseSuid(true),
	)
}
