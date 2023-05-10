// Copyright (c) 2018-2023, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package oci

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sylabs/singularity/pkg/ocibundle/tools"
	"github.com/sylabs/singularity/pkg/sylog"
	"github.com/sylabs/singularity/pkg/util/singularityconf"
)

// WrapWithWritableTmpFs runs a function wrapped with prep / cleanup steps for a writable tmpfs.
func WrapWithWritableTmpFs(f func() error, bundleDir string) error {
	// TODO: --oci mode always emulating --compat, which uses --writable-tmpfs.
	//       Provide a way of disabling this, for a read only rootfs.
	overlayDir, err := prepareWritableTmpfs(bundleDir)
	if err != nil {
		return err
	}

	err = f()

	// Cleanup actions log errors, but don't return - so we get as much cleanup done as possible.
	if cleanupErr := cleanupWritableTmpfs(bundleDir, overlayDir); cleanupErr != nil {
		sylog.Errorf("While cleaning up writable tmpfs: %v", cleanupErr)
	}

	// Return any error from the actual container payload - preserve exit code.
	return err
}

// WrapWithOverlays runs a function wrapped with prep / cleanup steps for overlays.
func WrapWithOverlays(f func() error, bundleDir string, overlayPaths []string) error {
	writableOverlayFound := false
	ovs := tools.OverlaySet{}
	for _, p := range overlayPaths {
		writable := true
		splitted := strings.SplitN(p, ":", 2)
		barePath := splitted[0]
		if len(splitted) > 1 {
			if splitted[1] == "ro" {
				writable = false
			}
		}

		if writable && writableOverlayFound {
			return fmt.Errorf("you can't specify more than one writable overlay; %#v has already been specified as a writable overlay; use '--overlay %s:ro' instead", ovs.WritableLoc, barePath)
		}
		if writable {
			writableOverlayFound = true
			ovs.WritableLoc = barePath
		} else {
			ovs.ReadonlyLocs = append(ovs.ReadonlyLocs, barePath)
		}
	}

	rootFsDir := tools.RootFs(bundleDir).Path()
	err := tools.ApplyOverlay(rootFsDir, ovs)
	if err != nil {
		return err
	}

	err = f()

	// Cleanup actions log errors, but don't return - so we get as much cleanup done as possible.
	if cleanupErr := tools.UnmountOverlay(rootFsDir); cleanupErr != nil {
		sylog.Errorf("While unmounting rootfs overlay: %v", cleanupErr)
	}

	// Return any error from the actual container payload - preserve exit code.
	return err
}

func prepareWritableTmpfs(bundleDir string) (string, error) {
	sylog.Debugf("Configuring writable tmpfs overlay for %s", bundleDir)
	c := singularityconf.GetCurrentConfig()
	if c == nil {
		return "", fmt.Errorf("singularity configuration is not initialized")
	}
	return tools.CreateOverlayTmpfs(bundleDir, int(c.SessiondirMaxSize))
}

func cleanupWritableTmpfs(bundleDir, overlayDir string) error {
	sylog.Debugf("Cleaning up writable tmpfs overlay for %s", bundleDir)
	return tools.DeleteOverlayTmpfs(bundleDir, overlayDir)
}

// absOverlay takes an overlay description string (a path, optionally followed by a colon with an option string, like ":ro" or ":rw"), and replaces any relative path in the description string with an absolute one.
func absOverlay(desc string) (string, error) {
	splitted := strings.SplitN(desc, ":", 2)
	barePath := splitted[0]
	absBarePath, err := filepath.Abs(barePath)
	if err != nil {
		return "", err
	}
	absDesc := absBarePath
	if len(splitted) > 1 {
		absDesc += ":" + splitted[1]
	}

	return absDesc, nil
}
