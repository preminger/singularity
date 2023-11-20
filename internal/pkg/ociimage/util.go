// Copyright (c) 2019-2023, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package ociimage

import (
	"fmt"
	"strings"

	"github.com/containers/image/v5/docker"
	dockerarchive "github.com/containers/image/v5/docker/archive"
	dockerdaemon "github.com/containers/image/v5/docker/daemon"
	ociarchive "github.com/containers/image/v5/oci/archive"
	ocilayout "github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/types"
	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// TransportOptions provides authentication, platform etc. configuration for
// interactions with image transports.
type TransportOptions struct {
	// AuthConfig provides optional credentials to be used when interacting with
	// an image transport.
	AuthConfig *authn.AuthConfig
	// AuthFilePath provides an optional path to a file containing credentials
	// to be used when interacting with an image transport.
	AuthFilePath string
	// Insecure should be set to true in order to interact with a registry via
	// http, or without TLS certificate verification.
	Insecure bool
	// DockerDaemonHost provides the URI to use when interacting with a Docker
	// daemon.
	DockerDaemonHost string
	// Platform specifies the OS / Architeture / Variant
	Platform  v1.Platform
	UserAgent string
	TmpDir    string
}

// SystemContext returns a containers/image/v5 types.SystemContext struct for
// compatibility with operations that still use container/image.
//
// Deprecated: for containers/image compatibility only. To be removes in
// SingularityCE v5
func (t *TransportOptions) SystemContext() *types.SystemContext {
	sc := types.SystemContext{
		AuthFilePath:            t.AuthFilePath,
		BigFilesTemporaryDir:    t.TmpDir,
		DockerRegistryUserAgent: t.UserAgent,
		OSChoice:                t.Platform.OS,
		ArchitectureChoice:      t.Platform.Architecture,
		VariantChoice:           t.Platform.Variant,
	}

	if t.AuthConfig != nil {
		sc.DockerAuthConfig = &types.DockerAuthConfig{
			Username:      t.AuthConfig.Username,
			Password:      t.AuthConfig.Password,
			IdentityToken: t.AuthConfig.IdentityToken,
		}
	}

	if t.Insecure {
		sc.DockerInsecureSkipTLSVerify = types.NewOptionalBool(true)
		sc.DockerDaemonInsecureSkipTLSVerify = true
		sc.OCIInsecureSkipTLSVerify = true
	}

	return &sc
}

// TransportOptionsFromSystemContext returns a TransportOptions struct
// initialized from a containers/image SystemContext.
//
// Deprecated: for containers/image compatibility only. To be removed in
// SingularityCE v5
func TransportOptionsFromSystemContext(sc types.SystemContext) TransportOptions {
	to := TransportOptions{
		AuthFilePath: sc.AuthFilePath,
		TmpDir:       sc.BigFilesTemporaryDir,
		UserAgent:    sc.DockerRegistryUserAgent,
		Platform: v1.Platform{
			OS:           sc.OSChoice,
			Architecture: sc.ArchitectureChoice,
			Variant:      sc.VariantChoice,
		},
		Insecure: sc.DockerInsecureSkipTLSVerify == types.OptionalBoolTrue || sc.DockerDaemonInsecureSkipTLSVerify || sc.OCIInsecureSkipTLSVerify,
	}

	if sc.DockerAuthConfig != nil {
		to.AuthConfig = &authn.AuthConfig{
			Username:      sc.DockerAuthConfig.Username,
			Password:      sc.DockerAuthConfig.Password,
			IdentityToken: sc.DockerAuthConfig.IdentityToken,
		}
	}

	return to
}

// defaultPolicy is Singularity's default containers/image OCI signature verification policy - accept anything.
func defaultPolicy() (*signature.PolicyContext, error) {
	policy := &signature.Policy{Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()}}
	return signature.NewPolicyContext(policy)
}

// parseImageRef parses a uri-like OCI image reference into a containers/image types.ImageReference.
func ParseImageRef(imageRef string) (types.ImageReference, error) {
	parts := strings.SplitN(imageRef, ":", 2)
	if len(parts) < 2 {
		return nil, fmt.Errorf("could not parse image ref: %s", imageRef)
	}

	var srcRef types.ImageReference
	var err error

	switch parts[0] {
	case "docker":
		srcRef, err = docker.ParseReference(parts[1])
	case "docker-archive":
		srcRef, err = dockerarchive.ParseReference(parts[1])
	case "docker-daemon":
		srcRef, err = dockerdaemon.ParseReference(parts[1])
	case "oci":
		srcRef, err = ocilayout.ParseReference(parts[1])
	case "oci-archive":
		srcRef, err = ociarchive.ParseReference(parts[1])
	default:
		return nil, fmt.Errorf("cannot create an OCI container from %s source", parts[0])
	}
	if err != nil {
		return nil, fmt.Errorf("invalid image source: %v", err)
	}

	return srcRef, nil
}
