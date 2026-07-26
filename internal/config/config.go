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

	// BuildVMImage and BuildVMKernel enable F5's isolated builder: a
	// throwaway microVM that runs `docker build` on a submitted context.
	// Build both once per host from m4-gates/builder-image/. With no image
	// configured, deploys arriving over the network are REFUSED rather than
	// built on the host — the fallback is the vulnerability.
	BuildVMImage  string
	BuildVMKernel string
	// BuildIsolation: "off" | "untrusted" (default) | "all".
	BuildIsolation string
	// BuildVMMemMiB and BuildVMVCPUs size a builder VM. Builds want more of
	// both than the apps they produce.
	BuildVMMemMiB int
	BuildVMVCPUs  int

	// Egress installs the F6 nftables baseline at startup: app guests reach
	// nothing outbound, builder VMs reach a registry and a resolver, nobody
	// reaches another guest, the host, link-local or SMTP. On by default —
	// the baseline must never be something a deployment forgets to turn on.
	Egress bool
	// AppEgress additionally lets ordinary apps out (still no SMTP, no local
	// networks, rate-limited). Off, and expected to stay off.
	AppEgress bool
	// EgressRegistries are the container registries a builder VM may reach on
	// 443; EgressResolvers the DNS servers it may query.
	EgressRegistries string
	EgressResolvers  string
	// EgressBuildAllow widens the builder allowlist to package sources
	// (distro mirrors, PyPI, npm). Empty by default: almost every real
	// Dockerfile wants it, and it is a bigger surface than the registry
	// alone, so it should be a decision rather than a discovery.
	EgressBuildAllow string
	// EgressRate bounds permitted guest egress in packets/second.
	EgressRate int

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
		BuildVMImage:           "",
		BuildVMKernel:          "",
		BuildIsolation:         "untrusted",
		BuildVMMemMiB:          2048,
		BuildVMVCPUs:           2,
		Egress:                 true,
		AppEgress:              false,
		EgressRegistries:       "registry-1.docker.io,auth.docker.io,production.cloudflare.docker.com",
		EgressResolvers:        "1.1.1.1,8.8.8.8",
		EgressBuildAllow:       "",
		EgressRate:             200,
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
	fs.StringVar(&c.BuildVMImage, "build-vm-image", c.BuildVMImage, "builder microVM golden rootfs (F5); empty = untrusted deploys are refused")
	fs.StringVar(&c.BuildVMKernel, "build-vm-kernel", c.BuildVMKernel, "kernel for builder microVMs (needs cgroups/namespaces; empty = --kernel)")
	fs.StringVar(&c.BuildIsolation, "build-isolation", c.BuildIsolation, "which deploys build in a microVM: off | untrusted | all")
	fs.IntVar(&c.BuildVMMemMiB, "build-vm-mem", c.BuildVMMemMiB, "MiB of RAM for a builder microVM")
	fs.IntVar(&c.BuildVMVCPUs, "build-vm-vcpus", c.BuildVMVCPUs, "vCPUs for a builder microVM")
	fs.BoolVar(&c.Egress, "egress", c.Egress, "install the F6 nftables baseline (apps silent, builders reach a registry only)")
	fs.BoolVar(&c.AppEgress, "app-egress", c.AppEgress, "let app guests reach the internet (still no SMTP, no local networks, rate-limited)")
	fs.StringVar(&c.EgressRegistries, "egress-registries", c.EgressRegistries, "comma-separated registry hostnames builder VMs may reach on 443")
	fs.StringVar(&c.EgressBuildAllow, "egress-build-allow", c.EgressBuildAllow, "extra hostnames builder VMs may reach on 443 (deb.debian.org,pypi.org,...) — a deliberate widening")
	fs.StringVar(&c.EgressResolvers, "egress-resolvers", c.EgressResolvers, "comma-separated DNS servers builder VMs may query")
	fs.IntVar(&c.EgressRate, "egress-rate", c.EgressRate, "packets/second ceiling on any permitted guest egress")
	fs.BoolVar(&c.SnapshotBalloon, "snapshot-balloon", c.SnapshotBalloon, "balloon-reclaim guest RAM before snapshotting (smaller snapshots, slower first wake)")
	fs.DurationVar(&c.BalloonSettle, "balloon-settle", c.BalloonSettle, "guest reclaim time after balloon inflate")
	fs.DurationVar(&c.DeflateSettle, "deflate-settle", c.DeflateSettle, "wait after balloon deflate before pausing")
}
