// Copyright (c) 2023 Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package ocisif

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ocitypes "github.com/containers/image/v5/types"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	ggcrmutate "github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sylabs/oci-tools/pkg/mutate"
	ocisif "github.com/sylabs/oci-tools/pkg/sif"
	"github.com/sylabs/sif/v2/pkg/sif"
	"github.com/sylabs/singularity/v4/internal/pkg/cache"
	"github.com/sylabs/singularity/v4/internal/pkg/client/progress"
	"github.com/sylabs/singularity/v4/internal/pkg/ociimage"
	"github.com/sylabs/singularity/v4/internal/pkg/ociplatform"
	"github.com/sylabs/singularity/v4/internal/pkg/remote/credential/ociauth"
	"github.com/sylabs/singularity/v4/internal/pkg/util/fs"
	"github.com/sylabs/singularity/v4/pkg/sylog"
	useragent "github.com/sylabs/singularity/v4/pkg/util/user-agent"
	"golang.org/x/term"
)

const (
	// TODO - Replace when exported from SIF / oci-tools
	SquashfsLayerMediaType types.MediaType = "application/vnd.sylabs.image.layer.v1.squashfs"

	// cacheSuffixMultiLayer is appended to the cached filename of OCI-SIF
	// images that have multiple layers. Single layer images have no suffix.
	cacheSuffixMultiLayer = ".ml"
)

var ErrFailedSquashfsConversion = errors.New("could not convert layer to squashfs")

type PullOptions struct {
	TmpDir      string
	OciAuth     *ocitypes.DockerAuthConfig
	DockerHost  string
	NoHTTPS     bool
	NoCleanUp   bool
	Platform    ggcrv1.Platform
	ReqAuthFile string
	KeepLayers  bool
}

// sysCtx provides authentication and tempDir config for containers/image OCI operations
//
//nolint:unparam
func sysCtx(opts PullOptions) (*ocitypes.SystemContext, error) {
	// DockerInsecureSkipTLSVerify is set only if --no-https is specified to honor
	// configuration from /etc/containers/registries.conf because DockerInsecureSkipTLSVerify
	// can have three possible values true/false and undefined, so we left it as undefined instead
	// of forcing it to false in order to delegate decision to /etc/containers/registries.conf:
	// https://github.com/sylabs/singularity/issues/5172
	sysCtx := &ocitypes.SystemContext{
		OCIInsecureSkipTLSVerify: opts.NoHTTPS,
		DockerAuthConfig:         opts.OciAuth,
		AuthFilePath:             ociauth.ChooseAuthFile(opts.ReqAuthFile),
		DockerRegistryUserAgent:  useragent.Value(),
		BigFilesTemporaryDir:     opts.TmpDir,
		DockerDaemonHost:         opts.DockerHost,
		OSChoice:                 opts.Platform.OS,
		ArchitectureChoice:       opts.Platform.Architecture,
		VariantChoice:            opts.Platform.Variant,
	}
	if opts.NoHTTPS {
		sysCtx.DockerInsecureSkipTLSVerify = ocitypes.NewOptionalBool(true)
	}

	return sysCtx, nil
}

// PullOCISIF will create an OCI-SIF image in the cache if directTo="", or a specific file if directTo is set.
func PullOCISIF(ctx context.Context, imgCache *cache.Handle, directTo, pullFrom string, opts PullOptions) (imagePath string, err error) {
	sys, err := sysCtx(opts)
	if err != nil {
		return "", err
	}

	ref, err := ociimage.ParseImageRef(pullFrom)
	if err != nil {
		return "", err
	}

	hash, err := ociimage.ImageDigest(ctx, sys, imgCache, ref)
	if err != nil {
		return "", fmt.Errorf("failed to get digest for %s: %s", pullFrom, err)
	}

	if directTo != "" {
		if err := createOciSif(ctx, sys, imgCache, pullFrom, directTo, opts); err != nil {
			return "", fmt.Errorf("while creating OCI-SIF: %w", err)
		}
		imagePath = directTo
	} else {
		// We must distinguish between multi-layer and single-layer OCI-SIF in
		// the cache so that the caller gets what they asked for.
		cacheSuffix := ""
		if opts.KeepLayers {
			cacheSuffix = cacheSuffixMultiLayer
		}
		cacheEntry, err := imgCache.GetEntry(cache.OciSifCacheType, hash.String()+cacheSuffix)
		if err != nil {
			return "", fmt.Errorf("unable to check if %v exists in cache: %v", hash, err)
		}
		defer cacheEntry.CleanTmp()
		if !cacheEntry.Exists {
			if err := createOciSif(ctx, sys, imgCache, pullFrom, cacheEntry.TmpPath, opts); err != nil {
				return "", fmt.Errorf("while creating OCI-SIF: %w", err)
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
func createOciSif(ctx context.Context, sysCtx *ocitypes.SystemContext, imgCache *cache.Handle, imageSrc, imageDest string, opts PullOptions) error {
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
	layoutRef, _, err := ociimage.FetchLayout(ctx, sysCtx, imgCache, imageSrc, layoutDir)
	if err != nil {
		return fmt.Errorf("while fetching OCI image: %w", err)
	}
	if err := ociplatform.CheckImageRefPlatform(ctx, sysCtx, layoutRef); err != nil {
		return fmt.Errorf("while checking OCI image: %w", err)
	}

	// Step 2 - Work from containers/image ImageReference -> gocontainerregistry digest & manifest
	layoutSrc, err := layoutRef.NewImageSource(ctx, sysCtx)
	if err != nil {
		return err
	}
	defer layoutSrc.Close()
	rawManifest, _, err := layoutSrc.GetManifest(ctx, nil)
	if err != nil {
		return err
	}
	digest, _, err := ggcrv1.SHA256(bytes.NewBuffer(rawManifest))
	if err != nil {
		return err
	}
	mf, err := ggcrv1.ParseManifest(bytes.NewBuffer(rawManifest))
	if err != nil {
		return err
	}

	// If the image has a single squashfs layer, then we can always write it
	// directly to OCI-SIF.
	if (len(mf.Layers)) == 1 && (mf.Layers[0].MediaType == SquashfsLayerMediaType) {
		sylog.Infof("Writing OCI-SIF image")
		return writeLayoutToOciSif(layoutDir, digest, imageDest)
	}

	// If the image multiple layers, all are squashfs, and KeepLayers is in effect,
	// then we can write it directly to OCI-SIF.
	if opts.KeepLayers {
		allSquash := true
		for _, l := range mf.Layers {
			if l.MediaType != SquashfsLayerMediaType {
				allSquash = false
				break
			}
		}
		if allSquash {
			sylog.Infof("Writing OCI-SIF image")
			return writeLayoutToOciSif(layoutDir, digest, imageDest)
		}
	}

	// Otherwise, conversion and optional squashing are required.
	sylog.Infof("Converting OCI image to OCI-SIF format")
	return convertLayoutToOciSif(layoutDir, digest, imageDest, workDir, opts.KeepLayers)
}

// writeLayoutToOciSif will write an image from an OCI layout to an oci-sif without applying any mutations.
func writeLayoutToOciSif(layoutDir string, digest ggcrv1.Hash, imageDest string) error {
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

// convertLayoutToOciSif will convert an image in an OCI layout to an oci-sif with squashfs layer format.
// The OCI layout can contain only a single image.
func convertLayoutToOciSif(layoutDir string, digest ggcrv1.Hash, imageDest, workDir string, keepLayers bool) error {
	lp, err := layout.FromPath(layoutDir)
	if err != nil {
		return fmt.Errorf("while opening layout: %w", err)
	}
	img, err := lp.Image(digest)
	if err != nil {
		return fmt.Errorf("while retrieving image: %w", err)
	}

	if !keepLayers {
		sylog.Infof("Squashing image to single layer")
		img, err = mutate.Squash(img)
		if err != nil {
			return fmt.Errorf("while squashing image: %w", err)
		}
	}

	img, err = imgLayersToSquashfs(img, digest, workDir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFailedSquashfsConversion, err)
	}

	sylog.Infof("Writing OCI-SIF image")
	ii := ggcrmutate.AppendManifests(empty.Index, ggcrmutate.IndexAddendum{
		Add: img,
	})
	return ocisif.Write(imageDest, ii)
}

func imgLayersToSquashfs(img ggcrv1.Image, digest ggcrv1.Hash, workDir string) (sqfsImage ggcrv1.Image, err error) {
	ms := []mutate.Mutation{}

	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("while retrieving layers: %w", err)
	}

	var sqOpts []mutate.SquashfsConverterOpt
	if len(layers) == 1 {
		sqOpts = []mutate.SquashfsConverterOpt{
			mutate.OptSquashfsSkipWhiteoutConversion(true),
		}
	}

	for i, l := range layers {
		squashfsLayer, err := mutate.SquashfsLayer(l, workDir, sqOpts...)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrFailedSquashfsConversion, err)
		}
		ms = append(ms, mutate.SetLayer(i, squashfsLayer))
	}

	ms = append(ms,
		mutate.SetHistory(ggcrv1.History{
			Created:    ggcrv1.Time{Time: time.Now()},
			CreatedBy:  useragent.Value(),
			Comment:    "oci-sif created from " + digest.Hex,
			EmptyLayer: false,
		}))

	sqfsImage, err = mutate.Apply(img, ms...)
	if err != nil {
		return nil, fmt.Errorf("while replacing layers: %w", err)
	}

	return sqfsImage, nil
}

// PushOCISIF pushes a single image from sourceFile to the OCI registry destRef.
func PushOCISIF(ctx context.Context, sourceFile, destRef string, ociAuth *ocitypes.DockerAuthConfig, reqAuthFile string) error {
	destRef = strings.TrimPrefix(destRef, "docker://")
	destRef = strings.TrimPrefix(destRef, "//")
	ir, err := name.ParseReference(destRef)
	if err != nil {
		return fmt.Errorf("invalid reference %q: %w", destRef, err)
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

	remoteOpts := []remote.Option{
		ociauth.AuthOptn(ociAuth, reqAuthFile),
		remote.WithUserAgent(useragent.Value()),
		remote.WithContext(ctx),
	}
	if term.IsTerminal(2) {
		pb := &progress.DownloadBar{}
		progChan := make(chan ggcrv1.Update, 1)
		go func() {
			var total int64
			soFar := int64(0)
			for {
				// The following is concurrency-safe because this is the only
				// goroutine that's going to be reading progChan updates.
				update := <-progChan
				if update.Error != nil {
					pb.Abort(false)
					return
				}
				if update.Total != total {
					pb.Init(update.Total)
					total = update.Total
				}
				pb.IncrBy(int(update.Complete - soFar))
				soFar = update.Complete
				if soFar >= total {
					pb.Wait()
					return
				}
			}
		}()
		remoteOpts = append(remoteOpts, remote.WithProgress(progChan))
	}

	return remote.Write(ir, image, remoteOpts...)
}
