// krilld — the Krill host agent. One daemon on one KVM host: registers apps,
// boots Firecracker microVMs, snapshots them when idle, wakes them on request.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shulman33/krill/internal/admin"
	"github.com/shulman33/krill/internal/builder"
	"github.com/shulman33/krill/internal/buildvm"
	"github.com/shulman33/krill/internal/config"
	"github.com/shulman33/krill/internal/dataplane"
	"github.com/shulman33/krill/internal/egress"
	"github.com/shulman33/krill/internal/host"
	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/network"
	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/regbackup"
	"github.com/shulman33/krill/internal/registry"
	"github.com/shulman33/krill/internal/rootfs"
	"github.com/shulman33/krill/internal/router"
)

func main() {
	if err := run(); err != nil {
		slog.Error("krilld exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Default()
	cfg.RegisterFlags(flag.CommandLine)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	for _, d := range []string{"apps", "build", "backup"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o755); err != nil {
			return err
		}
	}
	reg, err := registry.Open(filepath.Join(cfg.DataDir, "krill.db"))
	if err != nil {
		return err
	}
	defer reg.Close()

	fs := rootfs.NewManager(cfg.DataDir)
	netm := network.NewManager(nil)
	be := host.New(cfg, netm, fs, log)
	sup := lifecycle.New(reg, be, lifecycle.Config{
		WakeTimeout: cfg.WakeTimeout,
		// Freeze = balloon settle + deflate settle + writing guest RAM to
		// disk; 90s of margin covers the largest M1 guests.
		FreezeTimeout: cfg.BalloonSettle + cfg.DeflateSettle + 90*time.Second,
		IdleTimeout:   cfg.IdleTimeout,
	}, log)
	var store objstore.Store
	spec := cfg.Objstore
	if cfg.DataPlane {
		if spec == "" {
			spec = "file://" + filepath.Join(cfg.DataDir, "objstore")
		}
		store, err = objstore.Open(spec)
		if err != nil {
			return err
		}
		// Preflight the store before serving anything. This is not a liveness
		// ping: it exercises the conditional PUT the fencing protocol rests
		// on, so a store that silently ignores preconditions is caught here
		// rather than during a wake. Non-fatal by design — krilld runs under
		// Restart=always and must not crash-loop on a transient outage — but
		// loud, because until it passes every wake and every acked write will
		// fail.
		pctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = objstore.Check(pctx, store)
		cancel()
		if err != nil {
			log.Error("OBJECT STORE PREFLIGHT FAILED — the data plane cannot commit until this is fixed",
				"objstore", spec, "err", err)
		}
		coord := dataplane.NewCoordinator(dataplane.New(store), reg, fs,
			uint32(cfg.CellGen), cfg.DataDiskMB, log)
		sup.SetDataPlane(coord)
		log.Info("data plane on", "objstore", describe(store, spec),
			"preflight_ok", err == nil, "cell_gen", cfg.CellGen, "sync_ack", cfg.SyncAck)
	}
	if err := sup.Reconcile(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sup.RunJanitor(ctx)

	// The F6 baseline. It goes in before the router accepts anything, and
	// before any builder VM could exist, because the first masquerade rule
	// and the first drop rule have to arrive together — guests have no
	// outbound today only by accident, and an accident stops protecting you
	// the moment something needs the feature it was withholding.
	if cfg.Egress {
		eg := egress.New(egress.Config{
			AppEgress:     cfg.AppEgress,
			Registries:    splitList(cfg.EgressRegistries),
			BuildAllow:    splitList(cfg.EgressBuildAllow),
			Resolvers:     splitList(cfg.EgressResolvers),
			RatePerSecond: cfg.EgressRate,
		}, nil, nil)
		if err := eg.Apply(ctx); err != nil {
			// Loud, not fatal, for the same reason as the objstore preflight:
			// krilld runs under Restart=always and must not crash-loop. The
			// failure direction is safe — no masquerade means builds fail,
			// not that guests get out.
			log.Error("EGRESS BASELINE NOT INSTALLED — builder VMs cannot pull images, "+
				"and any pre-existing rules are whatever was there before", "err", err)
		} else {
			log.Info("egress baseline on", "app_egress", cfg.AppEgress,
				"registries", cfg.EgressRegistries, "build_allow", cfg.EgressBuildAllow,
				"rate_pps", cfg.EgressRate)
		}
		// Registry addresses move; a build that starts after they do would
		// otherwise fail against a stale set.
		go eg.RunRefresh(ctx, 15*time.Minute)
	} else {
		log.Warn("egress baseline DISABLED (--egress=false): if anything has granted " +
			"guests outbound, nothing here is stopping them")
	}

	// Registry backups: the epoch mint and the app catalog are the only state
	// on this box that nothing else can reconstruct (E1). RAID1 does not
	// survive the server.
	backups, backupSpec, err := setupBackups(cfg, reg, store, spec, log)
	if err != nil {
		return err
	}
	if backups != nil {
		go backups.Run(ctx)
	}

	bld := builder.New(cfg.DockerBin, filepath.Join(cfg.DataDir, "build"))
	rt := router.New(sup, log)
	rt.SyncAck = cfg.DataPlane && cfg.SyncAck
	rt.RouteSuffixes = splitList(cfg.RouteSuffixes)
	if len(rt.RouteSuffixes) > 0 {
		log.Info("router host-suffix pin on", "suffixes", rt.RouteSuffixes)
	}
	appSrv := &http.Server{Addr: cfg.ListenAddr, Handler: rt}
	adm := admin.New(sup, bld, admin.DeployConfig{
		WorkDir:      filepath.Join(cfg.DataDir, "build"),
		BaseHost:     cfg.BaseHost,
		RouterAddr:   cfg.ListenAddr,
		BuildTimeout: cfg.BuildTimeout,
	}, log)
	adm.SetDurability(admin.Durability{
		Spec: spec, Store: store, BackupSpec: backupSpec, Backups: backups,
	})
	setupIsolatedBuilder(cfg, adm, netm, log)
	admSrv := &http.Server{Addr: cfg.AdminAddr, Handler: adm}
	errCh := make(chan error, 2)
	go func() { errCh <- serve(appSrv, "router", log) }()
	go func() { errCh <- serve(admSrv, "admin", log) }()
	log.Info("krilld up", "router", cfg.ListenAddr, "admin", cfg.AdminAddr,
		"data", cfg.DataDir, "idle_timeout", cfg.IdleTimeout.String(),
		"identity_key", cfg.IdentityPubFile != "")

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	// Graceful shutdown: freeze every running app so acked writes sitting in
	// guest RAM land in a snapshot instead of dying with the daemon.
	log.Info("shutting down: freezing active apps")
	sup.FreezeAll()
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = appSrv.Shutdown(shCtx)
	_ = admSrv.Shutdown(shCtx)
	return nil
}

// setupIsolatedBuilder wires F5's builder VM into the deploy path.
//
// The important line here is the one that does nothing: when no builder image
// is configured, we do NOT fall back to host docker for untrusted deploys —
// admin refuses them. A server-side `docker build` of someone else's
// Dockerfile runs their instructions as root on the host, so the fallback
// would be the exact vulnerability the milestone exists to close.
func setupIsolatedBuilder(cfg config.Config, adm *admin.Server, netm *network.Manager, log *slog.Logger) {
	mode := admin.Isolation(cfg.BuildIsolation)
	switch mode {
	case admin.IsolationOff, admin.IsolationUntrusted, admin.IsolationAll:
	default:
		log.Error("unknown --build-isolation, falling back to untrusted", "value", cfg.BuildIsolation)
		mode = admin.IsolationUntrusted
	}
	if mode == admin.IsolationOff || cfg.BuildVMImage == "" {
		adm.SetIsolatedBuilder(nil, mode)
		log.Warn("NO ISOLATED BUILDER (--build-vm-image): deploys arriving through the doorman " +
			"will be refused. Build one per m4-gates/builder-image/README.md.")
		return
	}
	kernel := cfg.BuildVMKernel
	if kernel == "" {
		kernel = cfg.KernelPath
	}
	bvm := buildvm.New(buildvm.Config{
		Image: cfg.BuildVMImage, Kernel: kernel,
		FirecrackerBin: cfg.FirecrackerBin,
		WorkDir:        filepath.Join(cfg.DataDir, "build"),
		VCPUs:          cfg.BuildVMVCPUs, MemMiB: cfg.BuildVMMemMiB,
		Timeout: cfg.BuildTimeout,
	}, netm, log)
	if err := bvm.Available(); err != nil {
		adm.SetIsolatedBuilder(nil, mode)
		log.Error("ISOLATED BUILDER MISCONFIGURED: untrusted deploys will be refused", "err", err)
		return
	}
	adm.SetIsolatedBuilder(bvm, mode)
	log.Info("isolated builder on", "mode", mode, "image", cfg.BuildVMImage, "kernel", kernel)
}

// setupBackups resolves where registry snapshots go and builds the manager.
// Returns (nil, "", nil) when backups are disabled or have nowhere to go.
func setupBackups(cfg config.Config, reg *registry.Registry, dataStore objstore.Store, dataSpec string, log *slog.Logger) (*regbackup.Manager, string, error) {
	if cfg.RegistryBackupInterval <= 0 {
		log.Warn("registry backups DISABLED (--registry-backup-interval 0): " +
			"the epoch mint and app catalog exist only on this host")
		return nil, "", nil
	}
	store, spec := dataStore, dataSpec
	if cfg.RegistryBackupStore != "" {
		s, err := objstore.Open(cfg.RegistryBackupStore)
		if err != nil {
			return nil, "", fmt.Errorf("--registry-backup-store: %w", err)
		}
		store, spec = s, cfg.RegistryBackupStore
	}
	if store == nil {
		log.Warn("registry backups have nowhere to go: set --registry-backup-store " +
			"(the data plane is off, so there is no object store to inherit)")
		return nil, "", nil
	}
	if strings.HasPrefix(spec, "file://") {
		// Worth saying out loud: a backup on the same disk as the original
		// protects against `rm krill.db`, and against nothing else.
		log.Warn("registry backups are staying on this host — a dead server takes them with it",
			"backup_store", spec)
	}
	m := regbackup.New(regbackup.Config{
		Store:    store,
		Source:   reg,
		WorkDir:  filepath.Join(cfg.DataDir, "backup"),
		CellGen:  uint32(cfg.CellGen),
		Interval: cfg.RegistryBackupInterval,
		Keep:     cfg.RegistryBackupKeep,
		Log:      log,
	})
	log.Info("registry backups on", "store", spec,
		"interval", cfg.RegistryBackupInterval.String(), "keep", cfg.RegistryBackupKeep)
	return m, spec, nil
}

// describe prefers a backend's own description (which names the credential
// identity for GCS) over the raw spec.
func describe(store objstore.Store, spec string) string {
	if d, ok := store.(interface{ Describe() string }); ok {
		return d.Describe()
	}
	return spec
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func serve(s *http.Server, name string, log *slog.Logger) error {
	if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("server died", "server", name, "err", err)
		return err
	}
	return nil
}
