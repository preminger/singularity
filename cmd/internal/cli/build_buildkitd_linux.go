// Copyright 2015 The Linux Foundation.
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

// Copyright (c) 2023, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.
//
// This file contains modified code originally taken from:
// github.com/moby/buildkit/tree/v0.12.3/cmd/buildkitd

package cli

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/pkg/dialer"
	"github.com/containerd/containerd/pkg/userns"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/remotes/docker"
	ctdsnapshot "github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay"
	"github.com/containerd/containerd/snapshots/overlay/overlayutils"
	snproxy "github.com/containerd/containerd/snapshots/proxy"
	"github.com/containerd/containerd/sys"
	fuseoverlayfs "github.com/containerd/fuse-overlayfs-snapshotter"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/docker/docker/pkg/idtools"
	"github.com/gofrs/flock"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/cache/remotecache/azblob"
	"github.com/moby/buildkit/cache/remotecache/gha"
	inlineremotecache "github.com/moby/buildkit/cache/remotecache/inline"
	localremotecache "github.com/moby/buildkit/cache/remotecache/local"
	registryremotecache "github.com/moby/buildkit/cache/remotecache/registry"
	s3remotecache "github.com/moby/buildkit/cache/remotecache/s3"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/cmd/buildkitd/config"
	"github.com/moby/buildkit/control"
	bkoci "github.com/moby/buildkit/executor/oci"
	"github.com/moby/buildkit/frontend"
	dockerfile "github.com/moby/buildkit/frontend/dockerfile/builder"
	"github.com/moby/buildkit/frontend/gateway"
	"github.com/moby/buildkit/frontend/gateway/forwarder"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/bboltcachestorage"
	"github.com/moby/buildkit/util/appdefaults"
	"github.com/moby/buildkit/util/archutil"
	"github.com/moby/buildkit/util/network/cniprovider"
	"github.com/moby/buildkit/util/network/netproviders"
	"github.com/moby/buildkit/util/resolver"
	"github.com/moby/buildkit/version"
	"github.com/moby/buildkit/worker"
	"github.com/moby/buildkit/worker/base"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sylabs/singularity/v4/internal/pkg/runtime/launcher/oci"
	"github.com/sylabs/singularity/v4/internal/pkg/util/rootless"
	"github.com/sylabs/singularity/v4/pkg/syfs"
	"github.com/sylabs/singularity/v4/pkg/sylog"
	"go.etcd.io/bbolt"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
)

type workerInitializerOpt struct {
	config         *config.Config
	sessionManager *session.Manager
}

type workerInitializer struct {
	fn func(ctx context.Context, common workerInitializerOpt) ([]worker.Worker, error)
	// less priority number, more preferred
	priority int
}

var workerInitializers []workerInitializer

func registerWorkerInitializer(wi workerInitializer) {
	workerInitializers = append(workerInitializers, wi)
	sort.Slice(workerInitializers,
		func(i, j int) bool {
			return workerInitializers[i].priority < workerInitializers[j].priority
		})
}

func init() {
	registerWorkerInitializer(
		workerInitializer{
			fn:       ociWorkerInitializer,
			priority: 0,
		},
	)
}

// runBuildkitd runs a new buildkitd daemon. Once the server is ready, the path
// of the unix socket will be sent over the provided channel. Make sure this is
// a buffered channel with sufficient room to avoid deadlocks.
func runBuildkitd(ctx context.Context, socketChan chan<- string) error {
	cfg, err := config.LoadFile(defaultConfigPath())
	if err != nil {
		return err
	}

	setDefaultConfig(&cfg)

	server := grpc.NewServer()

	// relative path does not work with nightlyone/lockfile
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return err
	}
	cfg.Root = root

	if err := os.MkdirAll(root, 0o700); err != nil {
		return errors.Wrapf(err, "failed to create %s", root)
	}

	lockPath := filepath.Join(root, "buildkitd.lock")
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return errors.Wrapf(err, "could not lock %s", lockPath)
	}
	if !locked {
		return errors.Errorf("could not lock %s, another instance running?", lockPath)
	}
	defer func() {
		lock.Unlock()
		os.RemoveAll(lockPath)
	}()

	controller, err := newController(ctx, &cfg)
	if err != nil {
		return err
	}
	defer controller.Close()

	controller.Register(server)
	reflection.Register(server)

	errCh := make(chan error, 1)
	if err := serveGRPC(cfg.GRPC, server, errCh); err != nil {
		return err
	}

	// Send the address we're listening on back to our caller over socketChan
	socketChan <- cfg.GRPC.Address[0]

	select {
	case serverErr := <-errCh:
		err = serverErr
	case <-ctx.Done():
		err = ctx.Err()
	}

	sylog.Infof("stopping buildkitd server")
	if os.Getenv("NOTIFY_SOCKET") != "" {
		notified, notifyErr := sddaemon.SdNotify(false, sddaemon.SdNotifyStopping)
		sylog.Debugf("SdNotifyStopping notified=%v, err=%v", notified, notifyErr)
	}
	server.GracefulStop()

	return err
}

func ociWorkerInitializer(ctx context.Context, common workerInitializerOpt) ([]worker.Worker, error) {
	cfg := common.config.Workers.OCI

	if (cfg.Enabled == nil && !validOCIBinary()) || (cfg.Enabled != nil && !*cfg.Enabled) {
		return nil, nil
	}

	// TODO: this should never change the existing state dir
	idmapping, err := parseIdentityMapping(cfg.UserRemapUnsupported)
	if err != nil {
		return nil, err
	}

	hosts := resolverFunc(common.config)
	snFactory, err := snapshotterFactory(ctx, common.config.Root, cfg)
	if err != nil {
		return nil, err
	}

	if cfg.Rootless {
		sylog.Debugf("running in rootless mode")
		if common.config.Workers.OCI.NetworkConfig.Mode == "auto" {
			common.config.Workers.OCI.NetworkConfig.Mode = "host"
		}
	}

	processMode := bkoci.ProcessSandbox
	if cfg.NoProcessSandbox {
		if !rootless.InNS() {
			sylog.Fatalf("Trying to run with NoProcessSandbox enabled without being in a user namespace; this is insecure, and therefore blocked.")
		}
		if !cfg.Rootless {
			return nil, errors.New("can't enable NoProcessSandbox without Rootless")
		}
		processMode = bkoci.NoProcessSandbox
	}

	dns := getDNSConfig(common.config.DNS)

	nc := netproviders.Opt{
		Mode: common.config.Workers.OCI.NetworkConfig.Mode,
		CNI: cniprovider.Opt{
			Root:       common.config.Root,
			ConfigPath: common.config.Workers.OCI.CNIConfigPath,
			BinaryDir:  common.config.Workers.OCI.CNIBinaryPath,
			PoolSize:   common.config.Workers.OCI.CNIPoolSize,
		},
	}

	var parallelismSem *semaphore.Weighted
	if cfg.MaxParallelism > 0 {
		parallelismSem = semaphore.NewWeighted(int64(cfg.MaxParallelism))
	}

	// Select correct runtime binary
	r, err := oci.Runtime()
	if err != nil {
		return nil, err
	}
	if filepath.Base(r) == "crun" {
		cfg.Binary = r
		sylog.Infof("Using crun runtime for buildkitd daemon.")
	} else {
		sylog.Infof("Using runc runtime for buildkitd daemon.")
	}

	opt, err := NewBkWorkerOpt(ctx, common.config.Root, snFactory, cfg.Rootless, processMode, cfg.Labels, idmapping, nc, dns, cfg.Binary, cfg.ApparmorProfile, cfg.SELinux, parallelismSem, "", cfg.DefaultCgroupParent)
	if err != nil {
		return nil, err
	}
	opt.GCPolicy = getGCPolicy(cfg.GCConfig, common.config.Root)
	opt.BuildkitVersion = getBuildkitVersion()
	opt.RegistryHosts = hosts

	if platformsStr := cfg.Platforms; len(platformsStr) != 0 {
		platforms, err := parsePlatforms(platformsStr)
		if err != nil {
			return nil, errors.Wrap(err, "invalid platforms")
		}
		opt.Platforms = platforms
	}
	w, err := base.NewWorker(ctx, opt)
	if err != nil {
		return nil, err
	}

	return []worker.Worker{w}, nil
}

func snapshotterFactory(_ context.Context, commonRoot string, cfg config.OCIConfig) (BkSnapshotterFactory, error) {
	var (
		name    = cfg.Snapshotter
		address = cfg.ProxySnapshotterPath
	)
	if address != "" {
		snFactory := BkSnapshotterFactory{
			Name: name,
		}
		if _, err := os.Stat(address); os.IsNotExist(err) {
			return snFactory, errors.Wrapf(err, "snapshotter doesn't exist on %q (Do not include 'unix://' prefix)", address)
		}
		snFactory.New = func(root string) (ctdsnapshot.Snapshotter, error) {
			backoffConfig := backoff.DefaultConfig
			backoffConfig.MaxDelay = 3 * time.Second
			connParams := grpc.ConnectParams{
				Backoff: backoffConfig,
			}
			gopts := []grpc.DialOption{
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithConnectParams(connParams),
				grpc.WithContextDialer(dialer.ContextDialer),
				grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize)),
				grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)),
			}
			conn, err := grpc.Dial(dialer.DialAddress(address), gopts...)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to dial %q", address)
			}
			return snproxy.NewSnapshotter(snapshotsapi.NewSnapshotsClient(conn), name), nil
		}
		return snFactory, nil
	}

	if name == "auto" {
		if err := overlayutils.Supported(commonRoot); err == nil {
			name = "overlayfs"
		} else {
			sylog.Debugf("auto snapshotter: overlayfs is not available for %s, trying fuse-overlayfs: %v", commonRoot, err)
			if err2 := fuseoverlayfs.Supported(commonRoot); err2 == nil {
				name = "fuse-overlayfs"
			} else {
				sylog.Debugf("auto snapshotter: fuse-overlayfs is not available for %s, falling back to native: %v", commonRoot, err2)
				name = "native"
			}
		}
		sylog.Infof("auto snapshotter: using %s", name)
	}

	snFactory := BkSnapshotterFactory{
		Name: name,
	}
	switch name {
	case "overlayfs": // not "overlay", for consistency with containerd snapshotter plugin ID.
		snFactory.New = func(root string) (ctdsnapshot.Snapshotter, error) {
			return overlay.NewSnapshotter(root, overlay.AsynchronousRemove)
		}
	default:
		return snFactory, errors.Errorf("unknown snapshotter name: %q", name)
	}
	return snFactory, nil
}

func validOCIBinary() bool {
	_, err := exec.LookPath("runc")
	_, err1 := exec.LookPath("buildkit-runc")
	if err != nil && err1 != nil {
		sylog.Warningf("skipping oci worker, as runc does not exist")
		return false
	}
	return true
}

func parseIdentityMapping(str string) (*idtools.IdentityMapping, error) {
	if str == "" {
		return nil, nil
	}

	idparts := strings.SplitN(str, ":", 3)
	if len(idparts) > 2 {
		return nil, errors.Errorf("invalid userns remap specification in %q", str)
	}

	username := idparts[0]

	sylog.Debugf("user namespaces: ID ranges will be mapped to subuid ranges of: %s", username)

	mappings, err := idtools.LoadIdentityMapping(username)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create ID mappings")
	}
	return &mappings, nil
}

func serveGRPC(cfg config.GRPCConfig, server *grpc.Server, errCh chan error) error {
	addrs := cfg.Address
	if len(addrs) == 0 {
		return errors.New("--addr cannot be empty")
	}
	eg, _ := errgroup.WithContext(context.Background())
	listeners := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		l, err := getListener(addr, *cfg.UID, *cfg.GID, nil)
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return err
		}
		listeners = append(listeners, l)
	}

	if os.Getenv("NOTIFY_SOCKET") != "" {
		notified, notifyErr := sddaemon.SdNotify(false, sddaemon.SdNotifyReady)
		sylog.Debugf("SdNotifyReady notified=%v, err=%v", notified, notifyErr)
	}
	for _, l := range listeners {
		func(l net.Listener) {
			eg.Go(func() error {
				defer l.Close()
				sylog.Infof("running buildkitd server on %s", l.Addr())
				return server.Serve(l)
			})
		}(l)
	}
	go func() {
		errCh <- eg.Wait()
	}()
	return nil
}

func defaultConfigPath() string {
	return filepath.Join(syfs.ConfigDir(), "buildkitd.toml")
}

func setDefaultNetworkConfig(nc config.NetworkConfig) config.NetworkConfig {
	if nc.Mode == "" {
		nc.Mode = "auto"
	}
	if nc.CNIConfigPath == "" {
		nc.CNIConfigPath = appdefaults.DefaultCNIConfigPath
	}
	if nc.CNIBinaryPath == "" {
		nc.CNIBinaryPath = appdefaults.DefaultCNIBinDir
	}
	return nc
}

func setDefaultConfig(cfg *config.Config) {
	orig := *cfg

	// If we need to, enter a new cgroup now, to workaround an issue with crun container cgroup creation (#1538).
	if err := oci.CrunNestCgroup(); err != nil {
		sylog.Fatalf("while applying crun cgroup workaround: %v", err)
	}

	rlUID, err := rootless.Getuid()
	if err != nil {
		sylog.Fatalf("While trying to determine uid: %v", err)
	}
	if rlUID != 0 {
		cfg.Workers.OCI.Rootless = true
		cfg.Workers.OCI.NoProcessSandbox = true
	}

	if cfg.GRPC.UID == nil {
		uid := os.Getuid()
		cfg.GRPC.UID = &uid
	}

	if cfg.GRPC.GID == nil {
		gid := os.Getgid()
		cfg.GRPC.GID = &gid
	}

	enabled := true
	cfg.Workers.OCI.Enabled = &enabled

	if cfg.Root == "" {
		cfg.Root = appdefaults.Root
	}

	cfg.Workers.OCI.Snapshotter = "overlayfs"

	if len(cfg.GRPC.Address) == 0 {
		cfg.GRPC.Address = []string{appdefaults.Address}
	}

	if cfg.Workers.OCI.Platforms == nil {
		cfg.Workers.OCI.Platforms = formatPlatforms(archutil.SupportedPlatforms(false))
	}
	if cfg.Workers.Containerd.Platforms == nil {
		cfg.Workers.Containerd.Platforms = formatPlatforms(archutil.SupportedPlatforms(false))
	}

	cfg.Workers.OCI.NetworkConfig = setDefaultNetworkConfig(cfg.Workers.OCI.NetworkConfig)
	cfg.Workers.Containerd.NetworkConfig = setDefaultNetworkConfig(cfg.Workers.Containerd.NetworkConfig)

	if userns.RunningInUserNS() {
		// if buildkitd is being executed as the mapped-root (not only EUID==0 but also $USER==root)
		// in a user namespace, we need to enable the rootless mode but
		// we don't want to honor $HOME for setting up default paths.
		if u := os.Getenv("USER"); u != "" && u != "root" {
			if orig.Root == "" {
				cfg.Root = appdefaults.UserRoot()
			}
			if len(orig.GRPC.Address) == 0 {
				cfg.GRPC.Address = []string{appdefaults.UserAddress()}
			}
			appdefaults.EnsureUserAddressDir()
		}
	}
}

func getListener(addr string, uid, gid int, tlsConfig *tls.Config) (net.Listener, error) {
	addrSlice := strings.SplitN(addr, "://", 2)
	if len(addrSlice) < 2 {
		return nil, errors.Errorf("address %s does not contain proto, you meant unix://%s ?",
			addr, addr)
	}
	proto := addrSlice[0]
	listenAddr := addrSlice[1]
	switch proto {
	case "unix":
		if tlsConfig != nil {
			sylog.Warningf("TLS is disabled for %s", addr)
		}
		return sys.GetLocalListener(listenAddr, uid, gid)
	default:
		return nil, errors.Errorf("we do not support protocol %q addresses (%q)", proto, addr)
	}
}

func newController(ctx context.Context, cfg *config.Config) (*control.Controller, error) {
	sessionManager, err := session.NewManager()
	if err != nil {
		return nil, err
	}

	wc, err := newWorkerController(ctx, workerInitializerOpt{
		config:         cfg,
		sessionManager: sessionManager,
	})
	if err != nil {
		return nil, err
	}
	frontends := map[string]frontend.Frontend{}
	frontends["dockerfile.v0"] = forwarder.NewGatewayForwarder(wc, dockerfile.Build)
	frontends["gateway.v0"] = gateway.NewGatewayFrontend(wc)

	cacheStorage, err := bboltcachestorage.NewStore(filepath.Join(cfg.Root, "cache.db"))
	if err != nil {
		return nil, err
	}

	historyDB, err := bbolt.Open(filepath.Join(cfg.Root, "history.db"), 0o600, nil)
	if err != nil {
		return nil, err
	}

	resolverFn := resolverFunc(cfg)

	w, err := wc.GetDefault()
	if err != nil {
		return nil, err
	}

	remoteCacheExporterFuncs := map[string]remotecache.ResolveCacheExporterFunc{
		"registry": registryremotecache.ResolveCacheExporterFunc(sessionManager, resolverFn),
		"local":    localremotecache.ResolveCacheExporterFunc(sessionManager),
		"inline":   inlineremotecache.ResolveCacheExporterFunc(),
		"gha":      gha.ResolveCacheExporterFunc(),
		"s3":       s3remotecache.ResolveCacheExporterFunc(),
		"azblob":   azblob.ResolveCacheExporterFunc(),
	}
	remoteCacheImporterFuncs := map[string]remotecache.ResolveCacheImporterFunc{
		"registry": registryremotecache.ResolveCacheImporterFunc(sessionManager, w.ContentStore(), resolverFn),
		"local":    localremotecache.ResolveCacheImporterFunc(sessionManager),
		"gha":      gha.ResolveCacheImporterFunc(),
		"s3":       s3remotecache.ResolveCacheImporterFunc(),
		"azblob":   azblob.ResolveCacheImporterFunc(),
	}
	return control.NewController(control.Opt{
		SessionManager:            sessionManager,
		WorkerController:          wc,
		Frontends:                 frontends,
		ResolveCacheExporterFuncs: remoteCacheExporterFuncs,
		ResolveCacheImporterFuncs: remoteCacheImporterFuncs,
		CacheManager:              solver.NewCacheManager(ctx, "local", cacheStorage, worker.NewCacheResultStorage(wc)),
		Entitlements:              cfg.Entitlements,
		HistoryDB:                 historyDB,
		CacheStore:                cacheStorage,
		LeaseManager:              w.LeaseManager(),
		ContentStore:              w.ContentStore(),
		HistoryConfig:             cfg.History,
	})
}

func resolverFunc(cfg *config.Config) docker.RegistryHosts {
	return resolver.NewRegistryConfig(cfg.Registries)
}

func newWorkerController(ctx context.Context, wiOpt workerInitializerOpt) (*worker.Controller, error) {
	wc := &worker.Controller{}
	nWorkers := 0
	for _, wi := range workerInitializers {
		ws, err := wi.fn(ctx, wiOpt)
		if err != nil {
			return nil, err
		}
		for _, w := range ws {
			p := w.Platforms(false)
			archutil.WarnIfUnsupported(p)
			if err = wc.Add(w); err != nil {
				return nil, err
			}
			nWorkers++
		}
	}
	if nWorkers == 0 {
		return nil, errors.New("no worker found, rebuild the buildkit daemon?")
	}
	_, err := wc.GetDefault()
	if err != nil {
		return nil, err
	}
	return wc, nil
}

func formatPlatforms(p []ocispecs.Platform) []string {
	str := make([]string, 0, len(p))
	for _, pp := range p {
		str = append(str, platforms.Format(platforms.Normalize(pp)))
	}
	return str
}

func parsePlatforms(platformsStr []string) ([]ocispecs.Platform, error) {
	out := make([]ocispecs.Platform, 0, len(platformsStr))
	for _, s := range platformsStr {
		p, err := platforms.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, platforms.Normalize(p))
	}
	return out, nil
}

func getGCPolicy(cfg config.GCConfig, root string) []client.PruneInfo {
	if cfg.GC != nil && !*cfg.GC {
		return nil
	}
	if len(cfg.GCPolicy) == 0 {
		cfg.GCPolicy = config.DefaultGCPolicy(cfg.GCKeepStorage)
	}
	out := make([]client.PruneInfo, 0, len(cfg.GCPolicy))
	for _, rule := range cfg.GCPolicy {
		out = append(out, client.PruneInfo{
			Filter:       rule.Filters,
			All:          rule.All,
			KeepBytes:    rule.KeepBytes.AsBytes(root),
			KeepDuration: rule.KeepDuration.Duration,
		})
	}
	return out
}

func getBuildkitVersion() client.BuildkitVersion {
	return client.BuildkitVersion{
		Package:  version.Package,
		Version:  version.Version,
		Revision: version.Revision,
	}
}

func getDNSConfig(cfg *config.DNSConfig) *bkoci.DNSConfig {
	var dns *bkoci.DNSConfig
	if cfg != nil {
		dns = &bkoci.DNSConfig{
			Nameservers:   cfg.Nameservers,
			Options:       cfg.Options,
			SearchDomains: cfg.SearchDomains,
		}
	}
	return dns
}
