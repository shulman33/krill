// krilld — the Krill host agent. One daemon on one KVM host: registers apps,
// boots Firecracker microVMs, snapshots them when idle, wakes them on request.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shulman33/krill/internal/admin"
	"github.com/shulman33/krill/internal/builder"
	"github.com/shulman33/krill/internal/config"
	"github.com/shulman33/krill/internal/host"
	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/network"
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

	for _, d := range []string{"apps", "build"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o755); err != nil {
			return err
		}
	}
	reg, err := registry.Open(filepath.Join(cfg.DataDir, "krill.db"))
	if err != nil {
		return err
	}
	defer reg.Close()

	be := host.New(cfg, network.NewManager(nil), rootfs.NewManager(cfg.DataDir), log)
	sup := lifecycle.New(reg, be, lifecycle.Config{
		WakeTimeout: cfg.WakeTimeout,
		// Freeze = balloon settle + deflate settle + writing guest RAM to
		// disk; 90s of margin covers the largest M1 guests.
		FreezeTimeout: cfg.BalloonSettle + cfg.DeflateSettle + 90*time.Second,
		IdleTimeout:   cfg.IdleTimeout,
	}, log)
	if err := sup.Reconcile(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sup.RunJanitor(ctx)

	bld := builder.New(cfg.DockerBin, filepath.Join(cfg.DataDir, "build"))
	appSrv := &http.Server{Addr: cfg.ListenAddr, Handler: router.New(sup, log)}
	admSrv := &http.Server{Addr: cfg.AdminAddr, Handler: admin.New(sup, bld, admin.DeployConfig{
		WorkDir:      filepath.Join(cfg.DataDir, "build"),
		BaseHost:     cfg.BaseHost,
		RouterAddr:   cfg.ListenAddr,
		BuildTimeout: cfg.BuildTimeout,
	}, log)}
	errCh := make(chan error, 2)
	go func() { errCh <- serve(appSrv, "router", log) }()
	go func() { errCh <- serve(admSrv, "admin", log) }()
	log.Info("krilld up", "router", cfg.ListenAddr, "admin", cfg.AdminAddr,
		"data", cfg.DataDir, "idle_timeout", cfg.IdleTimeout.String())

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

func serve(s *http.Server, name string, log *slog.Logger) error {
	if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("server died", "server", name, "err", err)
		return err
	}
	return nil
}
