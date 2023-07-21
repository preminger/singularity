// Copyright (c) 2019-2023, Sylabs Inc. All rights reserved.
// Copyright (c) 2020, Control Command Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"fmt"
	"io"
	"os"

	"github.com/sylabs/singularity/internal/pkg/remote"
)

// RegistryLogout logs out from an OCI/Docker registry.
func RegistryLogout(usrConfigFile, name string) (err error) {
	// opening config file
	file, err := os.OpenFile(usrConfigFile, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("while opening configuration file: %s", err)
	}
	defer file.Close()

	// read file contents to config struct
	c, err := remote.ReadFrom(file)
	if err != nil {
		return fmt.Errorf("while parsing configuration data: %s", err)
	}

	if err := syncSysConfig(c); err != nil {
		return err
	}

	// services
	if err := c.Logout(name); err != nil {
		return fmt.Errorf("while verifying token: %v", err)
	}

	// truncating file before writing new contents and syncing to commit file
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("while truncating configuration file: %s", err)
	}

	if n, err := file.Seek(0, io.SeekStart); err != nil || n != 0 {
		return fmt.Errorf("failed to reset %s cursor: %s", file.Name(), err)
	}

	if _, err := c.WriteTo(file); err != nil {
		return fmt.Errorf("while writing configuration to file: %s", err)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to flush configuration file %s: %s", file.Name(), err)
	}

	return nil
}
