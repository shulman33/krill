// Package config holds krilld's daemon-wide settings.
package config

import (
	"flag"
	"time"
)

type Config struct {
	// DataDir is krilld's root: registry DB, per-app dirs (golden rootfs,
	// active disk, snapshots). Should live on local-SSD-class storage —
	// network disks invalidate every latency number this project cares about.
	DataDir string

	// ListenAddr serves app traffic (the wake-on-request router).
	ListenAddr string
	// AdminAddr serves the control API. Loopback by default: it can register
	// and delete apps and must not be reachable from guests or the network.
	AdminAddr string

	// KernelPath is the default uncompressed kernel for apps that don't
	// specify their own. Snapshots are only valid on the exact
	// Firecracker+kernel pair they were taken with.
	KernelPath string
	// FirecrackerBin is the firecracker binary to spawn per microVM.
	FirecrackerBin string

	// IdleTimeout demotes ACTIVE apps to FROZEN after this much time with no
	// requests. The whole economic argument lives in this number.
	IdleTimeout time.Duration
	// WakeTimeout bounds a single cold boot or restore, including guest
	// readiness probing.
	WakeTimeout time.Duration

	// SnapshotBalloon toggles the balloon inflate/deflate cycle during
	// freeze. Inflating reclaims guest free pages, shrinking the stored mem
	// file — but the reclaim also evicts the guest's page cache, which the
	// first post-restore request pays back as a major-fault storm (~200 ms
	// measured for a python guest). Density vs first-wake latency, one knob.
	SnapshotBalloon bool
	// BalloonSettle is how long the guest gets to reclaim pages after the
	// balloon inflates during freeze (lib.sh used 5s). DeflateSettle is the
	// pause after deflating — never pause a VM mid-inflation.
	BalloonSettle time.Duration
	DeflateSettle time.Duration
}

func Default() Config {
	return Config{
		DataDir:        "/srv/krill",
		ListenAddr:     ":8080",
		AdminAddr:      "127.0.0.1:9091",
		KernelPath:     "/srv/fc/vmlinux",
		FirecrackerBin: "firecracker",
		IdleTimeout:     60 * time.Second,
		WakeTimeout:     30 * time.Second,
		SnapshotBalloon: true,
		BalloonSettle:   5 * time.Second,
		DeflateSettle:   1 * time.Second,
	}
}

// RegisterFlags wires every field to the standard flag set.
func (c *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "krilld state root (registry, rootfs images, snapshots)")
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "address for app traffic (wake-on-request router)")
	fs.StringVar(&c.AdminAddr, "admin", c.AdminAddr, "address for the control API (keep on loopback)")
	fs.StringVar(&c.KernelPath, "kernel", c.KernelPath, "default guest kernel (uncompressed vmlinux)")
	fs.StringVar(&c.FirecrackerBin, "firecracker", c.FirecrackerBin, "firecracker binary")
	fs.DurationVar(&c.IdleTimeout, "idle-timeout", c.IdleTimeout, "idle time before an ACTIVE app is frozen")
	fs.DurationVar(&c.WakeTimeout, "wake-timeout", c.WakeTimeout, "max time for one boot/restore incl. readiness")
	fs.BoolVar(&c.SnapshotBalloon, "snapshot-balloon", c.SnapshotBalloon, "balloon-reclaim guest RAM before snapshotting (smaller snapshots, slower first wake)")
	fs.DurationVar(&c.BalloonSettle, "balloon-settle", c.BalloonSettle, "guest reclaim time after balloon inflate")
	fs.DurationVar(&c.DeflateSettle, "deflate-settle", c.DeflateSettle, "wait after balloon deflate before pausing")
}
