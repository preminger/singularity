// Copyright (c) 2023 Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.
//
// The following code is adapted from:
//
//	https://github.com/google/go-containerregistry/blob/v0.15.2/pkg/authn/keychain.go
//
// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ociauth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"

	ocitypes "github.com/containers/image/v5/types"
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/sylabs/singularity/v4/internal/pkg/util/fs"
	"github.com/sylabs/singularity/v4/pkg/syfs"
	"github.com/sylabs/singularity/v4/pkg/sylog"
)

type singularityKeychain struct {
	mu          sync.Mutex
	reqAuthFile string
}

// Resolve implements Keychain.
func (sk *singularityKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	sk.mu.Lock()
	defer sk.mu.Unlock()

	cf, err := getCredsFile(ChooseAuthFile(sk.reqAuthFile))
	if err != nil {
		if sk.reqAuthFile != "" {
			// User specifically requested use of an auth file but relevant
			// credentials could not be read from that file; issue warning, but
			// proceed with anonymous authentication.
			sylog.Warningf("Unable to find matching credentials in specified file (%v); proceeding with anonymous authentication.", err)
		}

		// No credentials found; proceed anonymously.
		return authn.Anonymous, nil
	}

	// See:
	// https://github.com/google/ko/issues/90
	// https://github.com/moby/moby/blob/fc01c2b481097a6057bec3cd1ab2d7b4488c50c4/registry/config.go#L397-L404
	var cfg, empty types.AuthConfig
	for _, key := range []string{
		target.String(),
		target.RegistryStr(),
	} {
		if key == name.DefaultRegistry {
			key = authn.DefaultAuthKey
		}

		cfg, err = cf.GetAuthConfig(key)
		if err != nil {
			return nil, err
		}
		// cf.GetAuthConfig automatically sets the ServerAddress attribute. Since
		// we don't make use of it, clear the value for a proper "is-empty" test.
		// See: https://github.com/google/go-containerregistry/issues/1510
		cfg.ServerAddress = ""
		if cfg != empty {
			break
		}
	}

	if cfg == empty {
		return authn.Anonymous, nil
	}

	return authn.FromConfig(authn.AuthConfig{
		Username:      cfg.Username,
		Password:      cfg.Password,
		Auth:          cfg.Auth,
		IdentityToken: cfg.IdentityToken,
		RegistryToken: cfg.RegistryToken,
	}), nil
}

func AuthOptn(ociAuth *ocitypes.DockerAuthConfig, reqAuthFile string) remote.Option {
	// If explicit credentials in ociAuth were passed in, use those.
	if ociAuth != nil {
		auth := authn.FromConfig(authn.AuthConfig{
			Username:      ociAuth.Username,
			Password:      ociAuth.Password,
			IdentityToken: ociAuth.IdentityToken,
		})
		return remote.WithAuth(auth)
	}

	return remote.WithAuthFromKeychain(&singularityKeychain{reqAuthFile: reqAuthFile})
}

func getCredsFile(reqAuthFile string) (*configfile.ConfigFile, error) {
	authFileToUse := ChooseAuthFile(reqAuthFile)
	cf, err := ConfigFileFromPath(authFileToUse)
	if err != nil {
		return nil, fmt.Errorf("while trying to read OCI credentials from file %q: %w", reqAuthFile, err)
	}

	return cf, nil
}

// ConfigFileFromPath creates a configfile.Configfile object (part of docker/cli
// API) associated with the auth file at path.
func ConfigFileFromPath(path string) (*configfile.ConfigFile, error) {
	cf := configfile.New(path)
	if fs.IsFile(path) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		cf, err = config.LoadFromReader(f)
		if err != nil {
			return nil, err
		}
		cf.Filename = path
	}

	return cf, nil
}

// ChooseAuthFile returns reqAuthFile if it is not empty, or else the default
// location of the OCI registry auth file.
func ChooseAuthFile(reqAuthFile string) string {
	if reqAuthFile != "" {
		return reqAuthFile
	}

	return syfs.DockerConf()
}

func LoginAndStore(registry, username, password string, insecure bool, reqAuthFile string) error {
	if err := checkOCILogin(registry, username, password, insecure); err != nil {
		return err
	}

	cf, err := ConfigFileFromPath(ChooseAuthFile(reqAuthFile))
	if err != nil {
		return fmt.Errorf("while loading existing OCI registry credentials from %q: %w", ChooseAuthFile(reqAuthFile), err)
	}

	creds := cf.GetCredentialsStore(registry)

	// DockerHub requires special logic for historical reasons.
	serverAddress := registry
	if serverAddress == name.DefaultRegistry {
		serverAddress = authn.DefaultAuthKey
	}

	if err := creds.Store(types.AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: serverAddress,
	}); err != nil {
		return fmt.Errorf("while trying to store new credentials: %w", err)
	}

	sylog.Infof("Token stored in %s", cf.Filename)

	return nil
}

func checkOCILogin(regName string, username, password string, insecure bool) error {
	regOpts := []name.Option{}
	if insecure {
		regOpts = []name.Option{name.Insecure}
	}
	reg, err := name.NewRegistry(regName, regOpts...)
	if err != nil {
		return err
	}

	auth := authn.FromConfig(authn.AuthConfig{
		Username: username,
		Password: password,
	})

	// Creating a new transport pings the registry and works through auth flow.
	_, err = transport.NewWithContext(context.TODO(), reg, auth, http.DefaultTransport, nil)
	if err != nil {
		return err
	}

	return nil
}
