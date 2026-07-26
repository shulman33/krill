// Package buildvm is F5: `docker build` of an untrusted context runs inside a
// throwaway Krill microVM, never on the host.
//
// The risk being closed is today-risk #1 from the ROADMAP's posture map, and
// it is worth stating without euphemism: a server-side `docker build`
// executes the submitted Dockerfile's instructions, as root, on the host,
// outside any microVM, against a root daemon. That was acceptable while the
// only person who could reach the admin API was Sam. The edit share plane
// ends that, so the builder moves inside the primitive this platform
// already sells — the platform isolating its own builder by dogfooding.
//
// # The shape
//
//	context dir ─mkfs.ext4─> ctx.ext4 ──┐
//	                                    ├─> throwaway microVM ─> golden.ext4
//	empty formatted golden.ext4 ────────┘        (build runs here)
//
// The output disk IS the app's golden image: the builder mounts it and
// populates it, so nothing large has to be read back out through the host's
// ext4 parser. The build's own log and its structured result arrive on the
// serial console, which krilld already captures — the same channel that made
// M1's guest tracebacks debuggable.
//
// # What the isolation actually rests on
//
//   - The VM sees exactly two things from the host: a disk holding the
//     submitted context, and an empty disk to fill. No credentials, no
//     registry database, no other app's data, no host filesystem.
//   - Its network is a builder tap (krillb*), which the F6 ruleset treats as
//     the one guest class permitted outbound — to a container registry and a
//     resolver, and nothing else. It cannot reach 127.0.0.1:9091 because it
//     cannot reach the host at all.
//   - It is destroyed unconditionally: on success, on failure, on timeout,
//     and on a build that hangs forever. The teardown is a defer, and the
//     timeout is enforced by the host's clock, never the guest's.
package buildvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/builder"
	"github.com/shulman33/krill/internal/firecracker"
	"github.com/shulman33/krill/internal/network"
)

// ResultMarker is what the in-VM build script prints on the serial console
// when it is done. One line, so the host can find it in a log full of kernel
// noise and build output.
const ResultMarker = "KRILL-BUILD-RESULT:"

// MaxSlots bounds concurrent builder VMs. Builders are the heaviest thing
// this host does and the least latency-sensitive; a small pool keeps a burst
// of deploys from evicting resident apps.
const MaxSlots = 4

type Config struct {
	// Image is the builder golden rootfs: a trusted, operator-built image
	// containing the build toolchain. Built once from
	// m4-gates/builder-image/, exactly like the kernel.
	Image string
	// Kernel may differ from the app kernel. It generally has to: a container
	// build needs cgroups, namespaces and (for some snapshotters) overlayfs,
	// which the Firecracker CI kernel does not necessarily carry. Empty falls
	// back to the daemon kernel.
	Kernel         string
	FirecrackerBin string
	// WorkDir holds per-build scratch. Everything under it is removed when
	// the build returns.
	WorkDir string
	VCPUs   int
	MemMiB  int
	// Timeout bounds one build END TO END, measured on the host. A guest
	// clock cannot be trusted to bound anything (the same reason PT-3
	// forbids guest-side lease timers).
	Timeout time.Duration
	// OutputSizeMB is the golden image size when the caller does not say.
	OutputSizeMB int
	// ContextSizeMB caps the disk the submitted context is packed into.
	ContextSizeMB int
}

func Default() Config {
	return Config{
		FirecrackerBin: "firecracker",
		VCPUs:          2,
		MemMiB:         2048,
		Timeout:        10 * time.Minute,
		OutputSizeMB:   0, // auto
		ContextSizeMB:  0, // auto
	}
}

// Builder implements the same interface krilld's admin API already uses for
// host builds, so the two are interchangeable at the call site and the
// choice between them is one decision in one place.
type Builder struct {
	cfg  Config
	net  *network.Manager
	log  *slog.Logger
	slot chan int
}

func New(cfg Config, netm *network.Manager, log *slog.Logger) *Builder {
	if log == nil {
		log = slog.Default()
	}
	if cfg.VCPUs <= 0 {
		cfg.VCPUs = Default().VCPUs
	}
	if cfg.MemMiB <= 0 {
		cfg.MemMiB = Default().MemMiB
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = Default().Timeout
	}
	if cfg.FirecrackerBin == "" {
		cfg.FirecrackerBin = "firecracker"
	}
	slots := make(chan int, MaxSlots)
	for i := 0; i < MaxSlots; i++ {
		slots <- i
	}
	return &Builder{cfg: cfg, net: netm, log: log, slot: slots}
}

// Available reports whether an isolated build is possible at all. krilld
// refuses untrusted deploys when this is false rather than quietly falling
// back to a host build — the fallback IS the vulnerability.
func (b *Builder) Available() error {
	if b.cfg.Image == "" {
		return errors.New("no builder image configured (--build-vm-image)")
	}
	if _, err := os.Stat(b.cfg.Image); err != nil {
		return fmt.Errorf("builder image %s: %w", b.cfg.Image, err)
	}
	kernel := b.cfg.Kernel
	if kernel == "" {
		return errors.New("no builder kernel configured (--build-vm-kernel)")
	}
	if _, err := os.Stat(kernel); err != nil {
		return fmt.Errorf("builder kernel %s: %w", kernel, err)
	}
	return nil
}

// buildResult is the JSON the in-VM script prints after ResultMarker.
type buildResult struct {
	OK        bool     `json:"ok"`
	Stage     string   `json:"stage"`
	Error     string   `json:"error"`
	GuestPort int      `json:"guest_port"`
	SizeMB    int      `json:"size_mb"`
	Warnings  []string `json:"warnings"`
}

// Build runs one isolated build and returns the golden image.
func (b *Builder) Build(ctx context.Context, name, contextDir string, sizeMB int) (*builder.Result, error) {
	if err := b.Available(); err != nil {
		return nil, fmt.Errorf("isolated builder unavailable: %w", err)
	}
	var slot int
	select {
	case slot = <-b.slot:
		defer func() { b.slot <- slot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tmp, err := os.MkdirTemp(b.cfg.WorkDir, "buildvm-"+name+"-")
	if err != nil {
		return nil, err
	}
	res := builder.NewResult(tmp)
	ok := false
	defer func() {
		if !ok {
			res.Cleanup()
		}
	}()

	// 1. The submitted context becomes a disk. Note what this does NOT do:
	// it never extracts, interprets or executes anything from the context on
	// the host. The bytes go in unread.
	ctxDisk := filepath.Join(tmp, "ctx.ext4")
	ctxMB := b.cfg.ContextSizeMB
	if ctxMB <= 0 {
		ctxMB = autoSizeMB(contextDir, 512)
	}
	if err := mkfsFromDir(ctx, ctxDisk, contextDir, ctxMB); err != nil {
		return nil, &builder.BuildError{Stage: "packing context", Err: err}
	}
	// 2. An empty formatted disk to fill: this becomes the app's golden image.
	golden := filepath.Join(tmp, "golden.ext4")
	outMB := sizeMB
	if outMB <= 0 {
		outMB = b.cfg.OutputSizeMB
	}
	if outMB <= 0 {
		outMB = autoSizeMB(contextDir, 2048)
	}
	if err := mkfsEmpty(ctx, golden, outMB); err != nil {
		return nil, &builder.BuildError{Stage: "preparing output", Err: err}
	}
	// 3. A throwaway copy of the builder rootfs. Never the original: the
	// build is expected to scribble on it, and the next build must not
	// inherit what the last one left.
	rootfs := filepath.Join(tmp, "rootfs.ext4")
	if err := copyFile(b.cfg.Image, rootfs); err != nil {
		return nil, &builder.BuildError{Stage: "preparing builder rootfs", Err: err}
	}

	n, err := network.DeriveBuilder(slot)
	if err != nil {
		return nil, err
	}
	if err := b.net.EnsureTap(n); err != nil {
		return nil, &builder.BuildError{Stage: "builder network", Err: err}
	}
	// The tap goes away with the build. Leaving it would leave a permitted
	// egress path attached to nothing, waiting to be reused by something else.
	defer func() {
		if err := b.net.DeleteTap(n); err != nil {
			b.log.Error("builder tap not removed", "tap", n.TapName, "err", err)
		}
	}()

	serial := filepath.Join(tmp, "build.log")
	kernel := b.cfg.Kernel
	if kernel == "" {
		return nil, errors.New("no builder kernel")
	}

	// The host's clock bounds the build. Everything below inherits it.
	vmCtx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	start := time.Now()
	m, err := firecracker.Launch(vmCtx, b.cfg.FirecrackerBin, filepath.Join(tmp, "fc.sock"), serial)
	if err != nil {
		return nil, &builder.BuildError{Stage: "starting builder VM", Err: err}
	}
	// Unconditional: success, failure, timeout, panic. A builder VM that
	// outlives its deploy is an F5 FAIL on its own.
	defer m.Kill()

	err = m.Configure(vmCtx, firecracker.VMConfig{
		VCPUs: b.cfg.VCPUs, MemMiB: b.cfg.MemMiB,
		KernelPath: kernel,
		BootArgs:   bootArgs(name, n, outMB),
		RootfsPath: rootfs,
		Extra: []firecracker.Drive{
			// Read-only: a build has no business rewriting its own input, and
			// the host reads nothing back from it either way.
			{ID: "ctx", Path: ctxDisk, ReadOnly: true},
			{ID: "out", Path: golden},
		},
		TapDev: n.TapName, GuestMAC: n.GuestMAC,
	})
	if err == nil {
		err = m.Start(vmCtx)
	}
	if err != nil {
		return nil, &builder.BuildError{Stage: "starting builder VM", Log: tailFile(serial), Err: err}
	}

	// 4. Wait for the guest to power itself off. A build that never finishes
	// is killed by the deferred Kill when this returns.
	if err := waitExit(vmCtx, m); err != nil {
		b.log.Warn("builder VM killed", "app", name, "after", time.Since(start).String(), "err", err)
		return nil, &builder.BuildError{
			Stage: "build timed out",
			Log:   tailFile(serial),
			Err:   fmt.Errorf("the build did not finish within %s and was killed", b.cfg.Timeout),
		}
	}

	log := readFile(serial)
	out, found := parseResult(log)
	if !found {
		return nil, &builder.BuildError{Stage: "build", Log: tailOf(log),
			Err: errors.New("the builder VM exited without reporting a result")}
	}
	if !out.OK {
		return nil, &builder.BuildError{Stage: firstNonEmpty(out.Stage, "docker build"),
			Log: tailOf(log), Err: errors.New(firstNonEmpty(out.Error, "the build failed"))}
	}

	res.GoldenPath = golden
	res.GuestPort = out.GuestPort
	res.BuildLog = tailOf(log)
	res.SizeMB = outMB
	res.Warnings = out.Warnings
	b.log.Info("isolated build finished", "app", name, "slot", slot,
		"secs", int(time.Since(start).Seconds()), "size_mb", outMB)
	ok = true
	return res, nil
}

// bootArgs hands the builder VM everything it needs and nothing it doesn't.
// It has no console=ttyS0 problem to worry about — unlike an app, the whole
// point of this guest is that its output is read.
func bootArgs(name string, n network.AppNet, outMB int) string {
	return fmt.Sprintf(
		"reboot=k panic=1 pci=off random.trust_cpu=on console=ttyS0 loglevel=4 init=/krill-build.sh "+
			"krill_app=%s krill_out_mb=%d krill_ip=%s/30 krill_gw=%s ip=%s::%s:255.255.255.252::eth0:off",
		name, outMB, n.GuestIP, n.HostIP, n.GuestIP, n.HostIP)
}

// waitExit blocks until the VMM process is gone or the context expires.
// Polling rather than waiting on the process: Machine deliberately owns its
// own Wait, and a builder that has powered off is indistinguishable from one
// that crashed — both are "the VMM is no longer running", which is all we
// need before reading the log.
func waitExit(ctx context.Context, m *firecracker.Machine) error {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		if m.Exited() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// parseResult finds the last result line. The last, not the first: a hostile
// context can print anything it likes to stdout, including a forged success
// line, so the value that counts is the one the build script prints after
// the build is genuinely over.
func parseResult(log string) (buildResult, bool) {
	var out buildResult
	found := false
	for _, line := range strings.Split(log, "\n") {
		i := strings.Index(line, ResultMarker)
		if i < 0 {
			continue
		}
		var r buildResult
		if json.Unmarshal([]byte(strings.TrimSpace(line[i+len(ResultMarker):])), &r) == nil {
			out, found = r, true
		}
	}
	return out, found
}

func mkfsFromDir(ctx context.Context, image, dir string, sizeMB int) error {
	if err := truncate(image, sizeMB); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-d", dir, image).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 -d %s: %w: %s", dir, err, out)
	}
	return nil
}

func mkfsEmpty(ctx context.Context, image string, sizeMB int) error {
	if err := truncate(image, sizeMB); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", image).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", image, err, out)
	}
	return nil
}

func truncate(path string, sizeMB int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(int64(sizeMB) << 20)
}

func copyFile(src, dst string) error {
	// --sparse=always keeps a mostly-empty builder image cheap to copy per
	// build, the same trick fresh_rootfs uses on the wake path.
	if out, err := exec.Command("cp", "--sparse=always", src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("cp %s: %w: %s", src, err, out)
	}
	return nil
}

func autoSizeMB(dir string, floor int) int {
	var bytes int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
		}
		return nil
	})
	mb := int(bytes>>20)*3 + 256
	if mb < floor {
		mb = floor
	}
	return mb
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func tailFile(path string) string { return tailOf(readFile(path)) }

// tailOf bounds what a hostile build can push into a deploy response.
func tailOf(s string) string {
	const max = 64 << 10
	if len(s) > max {
		return "... (truncated)\n" + s[len(s)-max:]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
