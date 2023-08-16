// Copyright (c) 2019-2023, Sylabs Inc. All rights reserved.
// Copyright (c) 2020, Control Command Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/sylabs/singularity/v4/internal/pkg/remote"
	"github.com/sylabs/singularity/v4/internal/pkg/remote/credential"
	"github.com/sylabs/singularity/v4/internal/pkg/remote/endpoint"
)

// KeyserverList prints information about remote configurations
func KeyserverList(remoteName string, usrConfigFile string) (err error) {
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

	keyserverCredentials := make(map[string]*credential.Config)
	for _, cred := range c.Credentials {
		u, err := url.Parse(cred.URI)
		if err != nil {
			return err
		}

		switch u.Scheme {
		case "http", "https":
			keyserverCredentials[cred.URI] = cred
		}
	}

	defaultRemote, err := c.GetDefault()
	if err != nil {
		return fmt.Errorf("error getting default remote-endpoint: %w", err)
	}

	remotes := c.Remotes
	if remoteName != "" {
		ep, ok := c.Remotes[remoteName]
		if !ok {
			return fmt.Errorf("no remote-endpoint with the name %q found", remoteName)
		}
		remotes = map[string]*endpoint.Config{remoteName: ep}
	}

	for epName, ep := range remotes {
		fmt.Println()
		isSystem := ""
		if ep.System {
			isSystem = "*"
		}
		isDefault := ""
		if ep == defaultRemote {
			isDefault = "^"
		}
		fmt.Printf("%s %s%s\n", epName, isSystem, isDefault)

		if err := ep.UpdateKeyserversConfig(); err != nil {
			fmt.Println("(unable to fetch associated keyserver info for this endpoint)")
			continue
		}

		order := 1
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, kc := range ep.Keyservers {
			if kc.Skip {
				continue
			}
			secure := "TLS"
			if kc.Insecure {
				secure = "no TLS"
			}
			loggedInStr := ""
			if _, ok := keyserverCredentials[kc.URI]; ok {
				loggedInStr = "+"
			}
			fmt.Fprintf(tw, " \t#%d\t%s\t%s\t%s\n", order, kc.URI, secure, loggedInStr)
			order++
		}
		tw.Flush()
	}

	fmt.Println()
	fmt.Print(strings.Join([]string{
		"(* = system endpoint, ^ = default endpoint,",
		" + = user is logged in directly to this keyserver)",
	}, "\n"))
	fmt.Println()

	return nil
}
