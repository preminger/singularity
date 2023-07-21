// Copyright (c) 2019-2023, Sylabs Inc. All rights reserved.
// Copyright (c) 2020, Control Command Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/sylabs/singularity/internal/pkg/remote"
)

const listLine = "%s\t%s\t%s\t%s\t%s\t%s\n"

// RemoteList prints information about remote configurations
func RemoteList(usrConfigFile string) (err error) {
	c := &remote.Config{}

	// opening config file
	file, err := os.OpenFile(usrConfigFile, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no remote configurations")
		}
		return fmt.Errorf("while opening remote config file: %s", err)
	}
	defer file.Close()

	// read file contents to config struct
	c, err = remote.ReadFrom(file)
	if err != nil {
		return fmt.Errorf("while parsing remote config data: %s", err)
	}

	if err := syncSysConfig(c); err != nil {
		return err
	}

	// list in alphanumeric order
	names := make([]string, 0, len(c.Remotes))
	for n := range c.Remotes {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		iName, jName := names[i], names[j]

		if c.Remotes[iName].System && !c.Remotes[jName].System {
			return true
		} else if !c.Remotes[iName].System && c.Remotes[jName].System {
			return false
		}

		return names[i] < names[j]
	})
	sort.Strings(names)

	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, listLine, "NAME", "URI", "DEFAULT?", "GLOBAL?", "EXCLUSIVE?", "SECURE?")
	for _, n := range names {
		sys := ""
		if c.Remotes[n].System {
			sys = "✓"
		}
		excl := ""
		if c.Remotes[n].Exclusive {
			excl = "✓"
		}
		secure := "✓"
		if c.Remotes[n].Insecure {
			secure = "✗!"
		}
		isDefault := ""
		if c.DefaultRemote != "" && c.DefaultRemote == n {
			isDefault = "✓"
		}

		fmt.Fprintf(tw, listLine, n, c.Remotes[n].URI, isDefault, sys, excl, secure)
	}
	tw.Flush()

	return nil
}
