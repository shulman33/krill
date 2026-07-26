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

	// IdentityPubFile is the doorman's ed25519 public key (base64url, one
	// line). krilld appends it to every guest's kernel command line as
	// krill_idkey=, and the generated init exports it as
	// KRILL_IDENTITY_PUBKEY — which is how an app with NO outbound network
	// (the F6 baseline) can still verify the X-Krill-Token the doorman
	// minted for its caller. Empty = no injection (M1-M3 behavior).
	IdentityPubFile string

	// RouteSuffixes pins which Host suffixes the router will serve, as a
	// comma-separated list. Empty = accept any suffix, which is M1-M3
	// behavior and is what the gate suites rely on (they send krill.local).
	// The doorman pins the suffix unconditionally; this is defense in depth
	// for the loopback router behind it.
	RouteSuffixes string

	// BaseHost is the domain apps hang off: app "counter" serves at
	// counter.<BaseHost> through the router. Point a wildcard DNS record (or
	// /etc/hosts entries) at the router and printed URLs become clickable.
	BaseHost string
	// DockerBin builds deploy contexts into images (M2 deploy path).
	DockerBin string
	// BuildTimeout bounds one deploy build: docker build + export + mkfs.
	BuildTimeout time.Duration

	// IdleTimeout demotes ACTIVE apps to FROZEN after this much time with no
	// requests. The whole economic argument lives in this number.
	IdleTimeout time.Duration
	// WakeTimeout bounds a single cold boot or restore, including guest
	// readiness probing.
	WakeTimeout time.Duration

	// Objstore is the data plane's object store: "file:///path" (default:
	// <DataDir>/objstore) or "gs://bucket/prefix". The M3 durability story
	// lives behind this URL — put it on storage you trust more than the
	// host's local disk.
	Objstore string
	// DataPlane master switch. Off = M1/M2 behavior: no data disks, no WAL
	// shipping, no epochs (kept for A1-comparable latency benchmarking).
	DataPlane bool
	// SyncAck is D1: hold each proxied response until the app's committed
	// writes are durable at the object store. Costs one precise WAL scan +
	// possible segment PUT per response; sync-in-region is the contract's
	// default, async an opt-out.
	SyncAck bool
	// CellGen is the epoch's cell-generation prefix (E1). Bump it manually
	// if the registry database (the epoch mint) is ever lost or restored
	// from backup — a new generation fences every pre-loss epoch at once.
	CellGen uint
	// DataDiskMB sizes per-app data disks (sparse files: the size is a
	// ceiling, not an allocation).
	DataDiskMB int

	// RegistryBackupStore is where registry snapshots are shipped; empty
	// means the data-plane object store. The registry is the epoch mint and
	// the app catalog — the one thing on the box that nothing else can
	// reconstruct — so this should resolve to storage that survives the host.
	RegistryBackupStore string
	// RegistryBackupInterval is the target age of the newest backup (0 =
	// off). Age-driven, not uptime-driven: a restarting daemon does not
	// re-ship, and a daemon down for a day ships as soon as it is back.
	RegistryBackupInterval time.Duration
	// RegistryBackupKeep is how many snapshots to retain.
	RegistryBackupKeep int

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
		DataDir:         "/srv/krill",
		ListenAddr:      ":8080",
		AdminAddr:       "127.0.0.1:9091",
		KernelPath:      "/srv/fc/vmlinux",
		FirecrackerBin:  "firecracker",
		BaseHost:        "krill.local",
		IdentityPubFile: "",
		RouteSuffixes:   "",
		DockerBin:       "docker",
		BuildTimeout:    10 * time.Minute,
		IdleTimeout:     60 * time.Second,
		WakeTimeout:     30 * time.Second,
		Objstore:        "", // resolved to file://<DataDir>/objstore at startup
		DataPlane:       true,
		SyncAck:         true,
		CellGen:         1,
		DataDiskMB:      256,

		RegistryBackupStore:    "", // resolved to the data-plane objstore
		RegistryBackupInterval: 24 * time.Hour,
		RegistryBackupKeep:     14,
		SnapshotBalloon:        true,
		BalloonSettle:          5 * time.Second,
		DeflateSettle:          1 * time.Second,
	}
}

// RegisterFlags wires every field to the standard flag set.
func (c *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "krilld state root (registry, rootfs images, snapshots)")
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "address for app traffic (wake-on-request router)")
	fs.StringVar(&c.AdminAddr, "admin", c.AdminAddr, "address for the control API (keep on loopback)")
	fs.StringVar(&c.KernelPath, "kernel", c.KernelPath, "default guest kernel (uncompressed vmlinux)")
	fs.StringVar(&c.FirecrackerBin, "firecracker", c.FirecrackerBin, "firecracker binary")
	fs.StringVar(&c.BaseHost, "base-host", c.BaseHost, "domain apps serve under (<app>.<base-host>)")
	fs.StringVar(&c.IdentityPubFile, "identity-pubkey-file", c.IdentityPubFile, "doorman ed25519 public key to hand guests on the kernel command line (empty = none)")
	fs.StringVar(&c.RouteSuffixes, "route-suffixes", c.RouteSuffixes, "comma-separated Host suffixes the router will serve (empty = any)")
	fs.StringVar(&c.DockerBin, "docker", c.DockerBin, "docker binary for deploy builds")
	fs.DurationVar(&c.BuildTimeout, "build-timeout", c.BuildTimeout, "max time for one deploy build")
	fs.DurationVar(&c.IdleTimeout, "idle-timeout", c.IdleTimeout, "idle time before an ACTIVE app is frozen")
	fs.DurationVar(&c.WakeTimeout, "wake-timeout", c.WakeTimeout, "max time for one boot/restore incl. readiness")
	fs.StringVar(&c.Objstore, "objstore", c.Objstore, "data-plane object store: file:///path or gs://bucket/prefix (empty = <data-dir>/objstore)")
	fs.BoolVar(&c.DataPlane, "data-plane", c.DataPlane, "enable the M3 data plane (data disks, WAL shipping, epochs)")
	fs.BoolVar(&c.SyncAck, "sync-ack", c.SyncAck, "hold responses until committed writes are durable at the object store (D1)")
	fs.UintVar(&c.CellGen, "cell-gen", c.CellGen, "epoch cell-generation prefix; bump after losing the registry database")
	fs.IntVar(&c.DataDiskMB, "data-disk-mb", c.DataDiskMB, "per-app data disk size ceiling in MiB")
	fs.StringVar(&c.RegistryBackupStore, "registry-backup-store", c.RegistryBackupStore, "where to ship registry snapshots: file:///path or gs://bucket/prefix (empty = the data-plane objstore)")
	fs.DurationVar(&c.RegistryBackupInterval, "registry-backup-interval", c.RegistryBackupInterval, "target age of the newest registry backup (0 = disable backups)")
	fs.IntVar(&c.RegistryBackupKeep, "registry-backup-keep", c.RegistryBackupKeep, "how many registry backups to retain")
	fs.BoolVar(&c.SnapshotBalloon, "snapshot-balloon", c.SnapshotBalloon, "balloon-reclaim guest RAM before snapshotting (smaller snapshots, slower first wake)")
	fs.DurationVar(&c.BalloonSettle, "balloon-settle", c.BalloonSettle, "guest reclaim time after balloon inflate")
	fs.DurationVar(&c.DeflateSettle, "deflate-settle", c.DeflateSettle, "wait after balloon deflate before pausing")
}
