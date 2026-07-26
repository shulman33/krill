package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// VMConfig is everything needed to configure a fresh VMM before InstanceStart.
type VMConfig struct {
	VCPUs      int
	MemMiB     int
	KernelPath string
	BootArgs   string
	RootfsPath string // this exact path gets baked into any snapshot taken later
	// DataPath, when non-empty, attaches a second virtio drive (/dev/vdb in
	// the guest): the app's durable data disk, tailed from the host by the
	// M3 data plane. Also baked into snapshots.
	DataPath string
	// Extra are additional drives beyond rootfs and data, attached in order
	// (/dev/vdb, /dev/vdc, ...). Builder VMs use them to take a build context
	// in and hand a finished image out without either ever touching the host
	// filesystem the builder can see.
	Extra    []Drive
	TapDev   string
	GuestMAC string
}

// Drive is one additional virtio block device.
type Drive struct {
	ID       string
	Path     string
	ReadOnly bool
}

// Machine is one firecracker process plus its API socket.
type Machine struct {
	sock   string
	cmd    *exec.Cmd
	client *Client
	waited chan error
}

// Launch spawns a firecracker process and waits for its API socket to appear
// (lib.sh waited ~2s; we keep that). logPath captures the VMM's own output —
// with a console in the boot args this is the guest serial log.
func Launch(ctx context.Context, bin, sock, logPath string) (*Machine, error) {
	_ = os.Remove(sock)
	cmd := exec.Command(bin, "--api-sock", sock)
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		cmd.Stdout = f
		cmd.Stderr = f
	}
	setProcAttr(cmd) // on Linux: SIGKILL the VMM if krilld dies — no orphan guests
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", bin, err)
	}
	m := &Machine{sock: sock, cmd: cmd, client: NewClient(sock), waited: make(chan error, 1)}
	go func() { m.waited <- cmd.Wait() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			return m, nil
		}
		select {
		case <-m.waited:
			return nil, fmt.Errorf("firecracker exited before its API socket appeared (see %s)", logPath)
		case <-ctx.Done():
			m.Kill()
			return nil, ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			m.Kill()
			return nil, fmt.Errorf("firecracker API socket %s never appeared", sock)
		}
		time.Sleep(time.Millisecond)
	}
}

// Configure applies the full pre-boot sequence from lib.sh:configure_vm:
// machine-config, boot-source, root drive, eth0, balloon device.
func (m *Machine) Configure(ctx context.Context, cfg VMConfig) error {
	steps := []struct {
		path string
		body any
	}{
		{"/machine-config", map[string]any{
			"vcpu_count": cfg.VCPUs, "mem_size_mib": cfg.MemMiB, "smt": false}},
		{"/boot-source", map[string]any{
			"kernel_image_path": cfg.KernelPath, "boot_args": cfg.BootArgs}},
		{"/drives/rootfs", map[string]any{
			"drive_id": "rootfs", "path_on_host": cfg.RootfsPath,
			"is_root_device": true, "is_read_only": false}},
	}
	drives := cfg.Extra
	if cfg.DataPath != "" {
		drives = append([]Drive{{ID: "data", Path: cfg.DataPath}}, drives...)
	}
	for _, d := range drives {
		steps = append(steps, struct {
			path string
			body any
		}{"/drives/" + d.ID, map[string]any{
			"drive_id": d.ID, "path_on_host": d.Path,
			"is_root_device": false, "is_read_only": d.ReadOnly}})
	}
	steps = append(steps, []struct {
		path string
		body any
	}{
		{"/network-interfaces/eth0", map[string]any{
			"iface_id": "eth0", "guest_mac": cfg.GuestMAC, "host_dev_name": cfg.TapDev}},
		// The balloon must exist at boot to be inflatable at freeze time.
		{"/balloon", map[string]any{
			"amount_mib": 0, "deflate_on_oom": true, "stats_polling_interval_s": 1}},
	}...)
	for _, s := range steps {
		if err := m.client.put(ctx, s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}

func (m *Machine) Start(ctx context.Context) error {
	return m.client.put(ctx, "/actions", map[string]any{"action_type": "InstanceStart"})
}

func (m *Machine) SetBalloon(ctx context.Context, mib int) error {
	return m.client.patch(ctx, "/balloon", map[string]any{"amount_mib": mib})
}

func (m *Machine) Pause(ctx context.Context) error {
	return m.client.patch(ctx, "/vm", map[string]any{"state": "Paused"})
}

// CreateSnapshot writes a Full snapshot. The VM must already be paused.
func (m *Machine) CreateSnapshot(ctx context.Context, vmstatePath, memPath string) error {
	return m.client.put(ctx, "/snapshot/create", map[string]any{
		"snapshot_type": "Full", "snapshot_path": vmstatePath, "mem_file_path": memPath})
}

// LoadSnapshot restores a snapshot into a fresh, unconfigured VMM and
// resumes it in the same call (resume_vm:true — the wake path's one round
// trip). The drive paths baked into vmstate must exist and be byte-exact.
func (m *Machine) LoadSnapshot(ctx context.Context, vmstatePath, memPath string) error {
	return m.client.put(ctx, "/snapshot/load", map[string]any{
		"snapshot_path": vmstatePath,
		"mem_backend":   map[string]any{"backend_type": "File", "backend_path": memPath},
		"resume_vm":     true})
}

// Kill hard-stops the VMM. For a paused, already-snapshotted VM this is the
// designed exit; there is nothing graceful to do with a microVM.
func (m *Machine) Kill() {
	if m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		<-m.waited
	}
	_ = os.Remove(m.sock)
}

// Exited reports whether the VMM process has already terminated on its own.
func (m *Machine) Exited() bool {
	select {
	case err := <-m.waited:
		// Preserve the signal for any later Kill() call.
		m.waited <- err
		return true
	default:
		return false
	}
}
