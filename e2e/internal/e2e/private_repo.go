// Copyright (c) 2023, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package e2e

import (
	"testing"
)

// PrivateRepoLogin logs in to the private repo at env.TestRegistryPrivURI. In
// all cases, the global default e2e repo username & password are used. If
// reqAuthFile is empty, the credentials will be stored in the default location
// ($HOME/.singularity/docker-config.json); if it is non-empty, the credentials
// will be stored as an authfile at the specified path.
func PrivateRepoLogin(t *testing.T, env TestEnv, profile Profile, reqAuthFile string) {
	args := []string{}
	if reqAuthFile != "" {
		args = append(args, "--authfile", reqAuthFile)
	}
	args = append(args, "-u", DefaultUsername, "-p", DefaultPassword, env.TestRegistryPrivURI)
	env.RunSingularity(
		t,
		WithProfile(profile),
		WithCommand("registry login"),
		WithArgs(args...),
		ExpectExit(0),
	)
}

// PrivateRepoLogout logs out of the private repo at env.TestRegistryPrivURI. If
// reqAuthFile is empty, login credentials will be removed from the default
// location ($HOME/.singularity/docker-config.json); if it is non-empty, login
// credentials will be removed from the authfile at the specified path.
func PrivateRepoLogout(t *testing.T, env TestEnv, profile Profile, reqAuthFile string) {
	args := []string{}
	if reqAuthFile != "" {
		args = append(args, "--authfile", reqAuthFile)
	}
	args = append(args, env.TestRegistryPrivURI)
	env.RunSingularity(
		t,
		WithProfile(profile),
		WithCommand("registry logout"),
		WithArgs(args...),
		ExpectExit(0),
	)
}
