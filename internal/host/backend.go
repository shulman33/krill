// Package host is the production lifecycle.Backend: Firecracker processes,
// tap devices, and disk images composed into the operations the supervisor
// needs. Everything here is a port of sequences proven by wake-bench.
package host

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/shulman33/krill/internal/config"
	"github.com/shulman33/krill/internal/firecracker"
	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/network"
	"github.com/shulman33/krill/internal/registry"
	"github.com/shulman33/krill/internal/rootfs"
)

// DefaultBootArgs mirrors wake-bench/lib.sh. The M1 guest contract: the
// image's /init.sh brings up eth0 from the krill_ip= / krill_gw= kernel
// args appended at boot time (M2's image builder will inject a compliant
// init into every built rootfs).
const DefaultBootArgs = "reboot=k panic=1 pci=off quiet loglevel=1 random.trust_cpu=on init=/init.sh"

type Backend struct {
	cfg config.Config
	net *network.Manager
	fs  *rootfs.Manager
	log *slog.Logger
}

func New(cfg config.Config, netm *network.Manager, fs *rootfs.Manager, log *slog.Logger) *Backend {
	if log == nil {
		log = slog.Default()
	}
	return &Backend{cfg: cfg, net: netm, fs: fs, log: log}
}

func (b *Backend) appNet(app registry.App) network.AppNet {
	n, err := network.Derive(app.SubnetIdx)
	if err != nil {
		// Subnet indexes are allocated 0..255 by the registry; out-of-range
		// here means the database was edited by hand.
		panic(fmt.Sprintf("app %s has impossible subnet index %d", app.Name, app.SubnetIdx))
	}
	return n
}

func (b *Backend) Install(app registry.App, goldenSrc string) error {
	if err := b.fs.Register(app.Name, goldenSrc); err != nil {
		return err
	}
	if err := b.ensureDataDisk(app); err != nil {
		return err
	}
	return b.net.EnsureTap(b.appNet(app))
}

// ensureDataDisk creates the app's (empty) durable data disk if the data
// plane wants one and it doesn't exist — including for apps that predate
// M3, on their first Prepare after upgrade.
func (b *Backend) ensureDataDisk(app registry.App) error {
	if !b.cfg.DataPlane || b.fs.HasDataDisk(app.Name) {
		return nil
	}
	return b.fs.BuildDataDisk(app.Name, nil, b.cfg.DataDiskMB)
}

// Replace swaps in a new golden and resets disk + snapshot files. The
// supervisor has already invalidated the snapshot in the registry and
// guaranteed no instance is running.
func (b *Backend) Replace(app registry.App, goldenSrc string) error {
	if err := b.fs.DropSnapshot(app.Name); err != nil {
		return err
	}
	if err := b.fs.InstallGolden(app.Name, goldenSrc); err != nil {
		return err
	}
	return b.fs.ResetDisk(app.Name)
}

func (b *Backend) SerialLogPath(app registry.App) string {
	return b.fs.SerialLog(app.Name)
}

func (b *Backend) Prepare(app registry.App) error {
	if err := b.fs.EnsureDirs(app.Name); err != nil {
		return err
	}
	if err := b.ensureDataDisk(app); err != nil {
		return err
	}
	return b.net.EnsureTap(b.appNet(app))
}

func (b *Backend) HasSnapshot(app registry.App) bool {
	return b.fs.SnapshotExists(app.Name)
}

func (b *Backend) GuestAddr(app registry.App) string {
	return net.JoinHostPort(b.appNet(app).GuestIP, strconv.Itoa(app.GuestPort))
}

func (b *Backend) kernel(app registry.App) string {
	if app.KernelPath != "" {
		return app.KernelPath
	}
	return b.cfg.KernelPath
}

func (b *Backend) bootArgs(app registry.App, n network.AppNet) string {
	base := app.BootArgs
	if base == "" {
		base = DefaultBootArgs
	}
	// Two ways to satisfy the network contract, guest's choice: krill_ip=/
	// krill_gw= for an init with an `ip` binary, and kernel-level ip=
	// autoconfig (CONFIG_IP_PNP) for images without one. A kernel lacking
	// IP_PNP ignores ip= as an unknown parameter; an init that configured
	// eth0 already just gets EEXIST from its `ip addr add`. Both are inert
	// where they don't apply.
	return fmt.Sprintf("%s krill_ip=%s/30 krill_gw=%s ip=%s::%s:255.255.255.252::eth0:off",
		base, n.GuestIP, n.HostIP, n.GuestIP, n.HostIP)
}

func (b *Backend) launch(ctx context.Context) func(app registry.App) (*firecracker.Machine, error) {
	return func(app registry.App) (*firecracker.Machine, error) {
		return firecracker.Launch(ctx, b.cfg.FirecrackerBin,
			b.fs.SockPath(app.Name), b.fs.SerialLog(app.Name))
	}
}

// ColdBoot: fresh VMM, full configure, InstanceStart. The disk is created
// from golden on first boot and persists afterwards — an app's data lives on
// its disk until M3 exists.
func (b *Backend) ColdBoot(ctx context.Context, app registry.App) (lifecycle.Instance, error) {
	if err := b.fs.EnsureDisk(app.Name); err != nil {
		return nil, err
	}
	n := b.appNet(app)
	m, err := b.launch(ctx)(app)
	if err != nil {
		return nil, err
	}
	vmc := firecracker.VMConfig{
		VCPUs:      app.VCPUs,
		MemMiB:     app.MemMiB,
		KernelPath: b.kernel(app),
		BootArgs:   b.bootArgs(app, n),
		RootfsPath: b.fs.DiskPath(app.Name),
		TapDev:     n.TapName,
		GuestMAC:   n.GuestMAC,
	}
	if b.cfg.DataPlane && b.fs.HasDataDisk(app.Name) {
		vmc.DataPath = b.fs.DataDiskPath(app.Name)
	}
	err = m.Configure(ctx, vmc)
	if err == nil {
		err = m.Start(ctx)
	}
	if err != nil {
		m.Kill()
		return nil, err
	}
	return m, nil
}

// Restore: fresh VMM, snapshot/load with resume_vm:true — the warm path.
// The snapshot's baked-in drive path is the app's disk.ext4, exactly as the
// last pause left it; the supervisor guarantees nothing mutated it since.
func (b *Backend) Restore(ctx context.Context, app registry.App) (lifecycle.Instance, error) {
	m, err := b.launch(ctx)(app)
	if err != nil {
		return nil, err
	}
	if err := m.LoadSnapshot(ctx, b.fs.VMStatePath(app.Name), b.fs.MemPath(app.Name)); err != nil {
		m.Kill()
		return nil, err
	}
	return m, nil
}

// Snapshot ports lib.sh:snapshot_vm — inflate the balloon so the guest
// returns free pages, deflate, settle, pause, write the snapshot, then
// hole-punch the mem file. The supervisor kills the VMM afterwards.
func (b *Backend) Snapshot(ctx context.Context, app registry.App, inst lifecycle.Instance) error {
	m, ok := inst.(*firecracker.Machine)
	if !ok {
		return fmt.Errorf("instance for %s is not a firecracker machine", app.Name)
	}
	if b.cfg.SnapshotBalloon {
		// Reclaim target: 3/4 of guest RAM, the benchmark's proven ratio
		// (384 MiB of 512). The balloon deflates on OOM, so an overshoot
		// degrades to a smaller reclaim rather than a dead guest. The cost:
		// reclaim evicts guest page cache, and the first post-restore
		// request pays it back as re-read faults.
		if err := m.SetBalloon(ctx, app.MemMiB*3/4); err != nil {
			return err
		}
		if err := sleepCtx(ctx, b.cfg.BalloonSettle); err != nil {
			return err
		}
		if err := m.SetBalloon(ctx, 0); err != nil {
			return err
		}
		// Never pause a VM mid-inflation.
		if err := sleepCtx(ctx, b.cfg.DeflateSettle); err != nil {
			return err
		}
	}
	if err := m.Pause(ctx); err != nil {
		return err
	}
	// The supervisor flipped snapshot_valid=false before we got here, so
	// deleting the old files is safe even if we crash mid-write.
	if err := b.fs.DropSnapshot(app.Name); err != nil {
		return err
	}
	if err := m.CreateSnapshot(ctx, b.fs.VMStatePath(app.Name), b.fs.MemPath(app.Name)); err != nil {
		return err
	}
	b.fs.PunchHoles(app.Name)
	return nil
}

func (b *Backend) Purge(app registry.App) error {
	if err := b.net.DeleteTap(b.appNet(app)); err != nil {
		return err
	}
	return b.fs.Purge(app.Name)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
