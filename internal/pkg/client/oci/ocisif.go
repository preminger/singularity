// Copyright (c) 2023 Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package oci

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ocitypes "github.com/containers/image/v5/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	ggcrmutate "github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sylabs/oci-tools/pkg/mutate"
	ocisif "github.com/sylabs/oci-tools/pkg/sif"
	"github.com/sylabs/sif/v2/pkg/sif"
	buildoci "github.com/sylabs/singularity/internal/pkg/build/oci"
	"github.com/sylabs/singularity/internal/pkg/cache"
	"github.com/sylabs/singularity/internal/pkg/util/bin"
	"github.com/sylabs/singularity/internal/pkg/util/fs"
	"github.com/sylabs/singularity/pkg/sylog"
	useragent "github.com/sylabs/singularity/pkg/util/user-agent"
)

// TODO - Replace when exported from SIF / oci-tools
const SquashfsLayerMediaType types.MediaType = "application/vnd.sylabs.image.layer.v1.squashfs"

// pull will create an OCI-SIF image in the cache if directTo="", or a specific file if directTo is set.
//
//nolint:dupl
func pullOciSif(ctx context.Context, imgCache *cache.Handle, directTo, pullFrom string, opts PullOptions) (imagePath string, err error) {
	sys := sysCtx(opts)
	hash, err := buildoci.ImageDigest(ctx, pullFrom, sys)
	if err != nil {
		return "", fmt.Errorf("failed to get checksum for %s: %s", pullFrom, err)
	}

	if directTo != "" {
		if err := createOciSif(ctx, imgCache, pullFrom, directTo, opts); err != nil {
			return "", fmt.Errorf("while creating OCI-SIF: %v", err)
		}
		imagePath = directTo
	} else {

		cacheEntry, err := imgCache.GetEntry(cache.OciSifCacheType, hash)
		if err != nil {
			return "", fmt.Errorf("unable to check if %v exists in cache: %v", hash, err)
		}
		defer cacheEntry.CleanTmp()
		if !cacheEntry.Exists {
			if err := createOciSif(ctx, imgCache, pullFrom, cacheEntry.TmpPath, opts); err != nil {
				return "", fmt.Errorf("while creating OCI-SIF: %v", err)
			}

			err = cacheEntry.Finalize()
			if err != nil {
				return "", err
			}

		} else {
			sylog.Infof("Using cached OCI-SIF image")
		}
		imagePath = cacheEntry.Path
	}

	return imagePath, nil
}

// createOciSif will convert an OCI source into an OCI-SIF using sylabs/oci-tools
func createOciSif(ctx context.Context, imgCache *cache.Handle, imageSrc, imageDest string, opts PullOptions) error {
	// Step 1 - Pull the OCI config and blobs to a standalone oci layout directory, through the cache if necessary.
	sys := sysCtx(opts)
	tmpDir, err := os.MkdirTemp(opts.TmpDir, "oci-sif-tmp-")
	if err != nil {
		return err
	}
	defer func() {
		sylog.Infof("Cleaning up.")
		if err := fs.ForceRemoveAll(tmpDir); err != nil {
			sylog.Warningf("Couldn't remove oci-sif temporary directory %q: %v", tmpDir, err)
		}
	}()

	layoutDir := filepath.Join(tmpDir, "layout")
	if err := os.Mkdir(layoutDir, 0o755); err != nil {
		return err
	}
	workDir := filepath.Join(tmpDir, "work")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		return err
	}

	sylog.Debugf("Fetching image to temporary layout %q", layoutDir)
	layoutRef, err := buildoci.FetchLayout(ctx, sys, imgCache, imageSrc, layoutDir)
	if err != nil {
		return fmt.Errorf("while fetching OCI image: %w", err)
	}

	// Step 2 - Work from containers/image ImageReference -> gocontainerregistry digest & manifest
	layoutSrc, err := layoutRef.NewImageSource(ctx, sys)
	if err != nil {
		return err
	}
	defer layoutSrc.Close()
	rawManifest, _, err := layoutSrc.GetManifest(ctx, nil)
	if err != nil {
		return err
	}
	digest, _, err := v1.SHA256(bytes.NewBuffer(rawManifest))
	if err != nil {
		return err
	}
	mf, err := v1.ParseManifest(bytes.NewBuffer(rawManifest))
	if err != nil {
		return err
	}

	// If the image has a single squashfs layer, then we can write it directly to oci-sif.
	if (len(mf.Layers)) == 1 && (mf.Layers[0].MediaType == SquashfsLayerMediaType) {
		sylog.Infof("Writing OCI-SIF image")
		return writeLayoutToOciSif(layoutDir, digest, imageDest, workDir)
	}

	// Otherwise, squashing and converting layers to squashfs is required.
	sylog.Infof("Converting OCI image to OCI-SIF format")
	return convertLayoutToOciSif(layoutDir, digest, imageDest, workDir)
}

// writeLayoutToOciSif will write an image from an OCI layout to an oci-sif without applying any mutations.
func writeLayoutToOciSif(layoutDir string, digest v1.Hash, imageDest, workDir string) error {
	lp, err := layout.FromPath(layoutDir)
	if err != nil {
		return fmt.Errorf("while opening layout: %w", err)
	}
	img, err := lp.Image(digest)
	if err != nil {
		return fmt.Errorf("while retrieving image: %w", err)
	}
	ii := ggcrmutate.AppendManifests(empty.Index, ggcrmutate.IndexAddendum{
		Add: img,
	})
	return ocisif.Write(imageDest, ii)
}

// convertLayoutToOciSif will convert an image in an OCI layout to a squashed oci-sif with squashfs layer format.
// The OCI layout can contain only a single image.
func convertLayoutToOciSif(layoutDir string, digest v1.Hash, imageDest, workDir string) error {
	lp, err := layout.FromPath(layoutDir)
	if err != nil {
		return fmt.Errorf("while opening layout: %w", err)
	}
	img, err := lp.Image(digest)
	if err != nil {
		return fmt.Errorf("while retrieving image: %w", err)
	}

	sylog.Infof("Squashing image to single layer")
	img, err = mutate.Squash(img)
	if err != nil {
		return fmt.Errorf("while squashing image: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("while retrieving layers: %w", err)
	}
	if len(layers) != 1 {
		return fmt.Errorf("%d > 1 layers remaining after squash operation", len(layers))
	}
	sqfsLayerConv, err := bin.FindBin("sqfstar")
	if err != nil {
		return fmt.Errorf("couldn't find appropriate utility for converting layer into squashfs format: %w", err)
	}
	squashfsLayer, err := mutate.SquashfsLayer(layers[0], workDir, mutate.OptSquashfsLayerConverter(sqfsLayerConv))
	if err != nil {
		return fmt.Errorf("while converting to squashfs format: %w", err)
	}
	img, err = mutate.Apply(img,
		mutate.ReplaceLayers(squashfsLayer),
		mutate.SetHistory(v1.History{
			Created:    v1.Time{time.Now()}, //nolint:govet
			CreatedBy:  useragent.Value(),
			Comment:    "oci-sif created from " + digest.Hex,
			EmptyLayer: false,
		}),
	)
	if err != nil {
		return fmt.Errorf("while replacing layers: %w", err)
	}

	sylog.Infof("Writing OCI-SIF image")
	ii := ggcrmutate.AppendManifests(empty.Index, ggcrmutate.IndexAddendum{
		Add: img,
	})
	return ocisif.Write(imageDest, ii)
}

// pushOCISIF pushes a single image from an OCI SIF to the OCI registry destination ref.
func pushOCISIF(ctx context.Context, sourceFile, dest string, ociAuth *ocitypes.DockerAuthConfig) error {
	dest = strings.TrimPrefix(dest, "//")
	ref, err := name.ParseReference(dest)
	if err != nil {
		return fmt.Errorf("invalid reference %q: %w", dest, err)
	}

	fi, err := sif.LoadContainerFromPath(sourceFile, sif.OptLoadWithFlag(os.O_RDONLY))
	if err != nil {
		return err
	}
	defer fi.UnloadContainer()

	ix, err := ocisif.ImageIndexFromFileImage(fi)
	if err != nil {
		return fmt.Errorf("only OCI-SIF files can be pushed to docker/OCI registries")
	}

	idxManifest, err := ix.IndexManifest()
	if err != nil {
		return fmt.Errorf("while obtaining index manifest: %w", err)
	}

	if len(idxManifest.Manifests) != 1 {
		return fmt.Errorf("only single image oci-sif files are supported")
	}
	image, err := ix.Image(idxManifest.Manifests[0].Digest)
	if err != nil {
		return fmt.Errorf("while obtaining image: %w", err)
	}

	// By default we use auth from ~/.singularity/docker-config.json
	authOptn := remote.WithAuthFromKeychain(&singularityKeychain{})

	// If explicit credentials in ociAuth were passed in, use those instead.
	if ociAuth != nil {
		auth := authn.FromConfig(authn.AuthConfig{
			Username:      ociAuth.Username,
			Password:      ociAuth.Password,
			IdentityToken: ociAuth.IdentityToken,
		})
		authOptn = remote.WithAuth(auth)
	}

	return remote.Write(ref, image, authOptn, remote.WithUserAgent(useragent.Value()))
}
