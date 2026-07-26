// Package builder turns an app directory into a bootable golden ext4 image:
//
//	context dir → docker build → docker export → staging tree
//	            → inject generated /krill-init.sh → mkfs.ext4 -d
//
// This is wake-bench/lib.sh:image_to_ext4 ported to Go, with two upgrades:
// no loop mount (mkfs.ext4 -d packs a directory directly, so a crashed build
// can never leak a mount), and an injected init that satisfies the M1
// network contract (krill_ip=/krill_gw= kernel args) and then execs the
// image's own CMD — the app author ships a plain Dockerfile and never learns
// what a microVM is.
//
// Trust model (M2): the admin API is a loopback, operator-only plane, and
// build contexts come from the operator's own tooling. Extraction of the
// *uploaded context* is still path-traversal-safe (cheap to do right); the
// docker-export tree is extracted by tar as root and is only as trustworthy
// as the Dockerfile that produced it. The doorman milestone (M4) is where
// other people's apps arrive, and with them real sandboxing of builds.
package builder

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// BootArgs is what deployed apps boot with. Differences from the daemon
// default: console=ttyS0 feeds the serial log (the logs tool is the M2
// feedback loop — worth the small boot cost), loglevel=3 keeps kernel chatter
// out of app output while still printing errors and panics, and init is the
// injected script rather than a hand-written /init.sh.
const BootArgs = "reboot=k panic=1 pci=off random.trust_cpu=on console=ttyS0 loglevel=3 init=/krill-init.sh"

// RunFunc executes a command and returns its combined output. Tests
// substitute a fake; production uses exec.CommandContext.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Builder struct {
	Docker  string // docker binary, e.g. "docker"
	WorkDir string // scratch space for staging trees and tars
	Run     RunFunc
}

func New(docker, workDir string) *Builder {
	return &Builder{Docker: docker, WorkDir: workDir, Run: ExecRunner}
}

type Result struct {
	// GoldenPath is the built ext4 image, inside a Build-owned temp dir.
	// The caller must use or copy it, then call Cleanup.
	GoldenPath string
	// GuestPort is the image's first EXPOSEd tcp port, 0 if none.
	GuestPort int
	// BuildLog is docker build's combined output.
	BuildLog string
	SizeMB   int
	// Warnings are non-fatal findings ("no iproute2 in image").
	Warnings []string

	tmpDir string
}

// NewResult returns an empty Result that owns tmpDir. It exists so an
// alternative builder — internal/buildvm, which runs the build inside a
// microVM — can hand back exactly the value the deploy path already consumes,
// making the two interchangeable at the call site.
func NewResult(tmpDir string) *Result { return &Result{tmpDir: tmpDir} }

func (r *Result) Cleanup() {
	if r.tmpDir != "" {
		os.RemoveAll(r.tmpDir)
	}
}

// BuildError carries the failing stage's output back to the caller — the
// whole point of the deploy loop is that errors arrive structured.
type BuildError struct {
	Stage string // "docker build", "image config", "export", "mkfs"
	Log   string
	Err   error
}

func (e *BuildError) Error() string {
	return fmt.Sprintf("%s: %v", e.Stage, e.Err)
}
func (e *BuildError) Unwrap() error { return e.Err }

// imageConfig is the slice of `docker image inspect .Config` we consume.
type imageConfig struct {
	Env          []string            `json:"Env"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	WorkingDir   string              `json:"WorkingDir"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
}

// Build runs the full pipeline. name must already be a valid app name (the
// registry enforces DNS labels); sizeMB <= 0 picks a size automatically.
func (b *Builder) Build(ctx context.Context, name, contextDir string, sizeMB int) (*Result, error) {
	tag := "krill-app-" + name

	out, err := b.Run(ctx, b.Docker, "build", "-t", tag, contextDir)
	buildLog := string(out)
	if err != nil {
		return nil, &BuildError{Stage: "docker build", Log: buildLog, Err: err}
	}

	cfgOut, err := b.Run(ctx, b.Docker, "image", "inspect", "-f", "{{json .Config}}", tag)
	if err != nil {
		return nil, &BuildError{Stage: "image config", Log: string(cfgOut), Err: err}
	}
	var cfg imageConfig
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(cfgOut))), &cfg); err != nil {
		return nil, &BuildError{Stage: "image config", Log: string(cfgOut), Err: err}
	}
	argv := append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	if len(argv) == 0 {
		return nil, &BuildError{Stage: "image config", Log: buildLog,
			Err: fmt.Errorf("image has no ENTRYPOINT or CMD — add a CMD to the Dockerfile")}
	}

	tmp, err := os.MkdirTemp(b.WorkDir, "build-"+name+"-")
	if err != nil {
		return nil, err
	}
	res := &Result{BuildLog: buildLog, GuestPort: guessPort(cfg.ExposedPorts), tmpDir: tmp}
	ok := false
	defer func() {
		if !ok {
			res.Cleanup()
		}
	}()

	staging := filepath.Join(tmp, "rootfs")
	if err := os.Mkdir(staging, 0o755); err != nil {
		return nil, err
	}
	cidOut, err := b.Run(ctx, b.Docker, "create", tag)
	if err != nil {
		return nil, &BuildError{Stage: "export", Log: string(cidOut), Err: err}
	}
	cid := strings.TrimSpace(string(cidOut))
	exportTar := filepath.Join(tmp, "export.tar")
	if out, err := b.Run(ctx, b.Docker, "export", "-o", exportTar, cid); err != nil {
		b.Run(ctx, b.Docker, "rm", cid)
		return nil, &BuildError{Stage: "export", Log: string(out), Err: err}
	}
	b.Run(ctx, b.Docker, "rm", cid)
	// --numeric-owner: container uids must land as-is, not remapped through
	// the host's passwd. Run as root, so ownership and modes are preserved.
	if out, err := b.Run(ctx, "tar", "-x", "--numeric-owner", "-f", exportTar, "-C", staging); err != nil {
		return nil, &BuildError{Stage: "export", Log: string(out), Err: err}
	}
	os.Remove(exportTar)

	if !hasShell(staging) {
		return nil, &BuildError{Stage: "export", Log: buildLog,
			Err: fmt.Errorf("image has no /bin/sh — the generated init needs a shell; use a base image with one (debian-slim, alpine, ...)")}
	}
	if !hasIPTool(staging) {
		res.Warnings = append(res.Warnings,
			"image has no `ip` binary (iproute2); guest networking will rely on kernel-level ip= autoconfig")
	}
	if !hasMountTool(staging) {
		res.Warnings = append(res.Warnings,
			"image has no `mount` binary; /data (the durable data disk) cannot be mounted — app data will not survive redeploys")
	}

	initPath := filepath.Join(staging, "krill-init.sh")
	if err := os.WriteFile(initPath, []byte(InitScript(name, cfg)), 0o755); err != nil {
		return nil, err
	}

	if sizeMB <= 0 {
		sizeMB = autoSizeMB(staging)
	}
	res.SizeMB = sizeMB
	golden := filepath.Join(tmp, "golden.ext4")
	f, err := os.Create(golden)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	if out, err := b.Run(ctx, "mkfs.ext4", "-q", "-F", "-d", staging, golden); err != nil {
		return nil, &BuildError{Stage: "mkfs", Log: string(out), Err: err}
	}
	os.RemoveAll(staging)

	res.GoldenPath = golden
	ok = true
	return res, nil
}

// InitScript renders /krill-init.sh: PID 1 for a deployed app. Mount kernel
// filesystems, bring up eth0 per the network contract, restore the image's
// env/workdir, exec its command.
func InitScript(name string, cfg imageConfig) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&sb, "# Generated by krill deploy for app %q. PID 1 inside the microVM.\n", name)
	sb.WriteString(`mount -t proc proc /proc 2>/dev/null
mount -t sysfs sys /sys 2>/dev/null
mount -t devtmpfs dev /dev 2>/dev/null
IP="$(sed -n 's/.*krill_ip=\([^ ]*\).*/\1/p' /proc/cmdline)"
GW="$(sed -n 's/.*krill_gw=\([^ ]*\).*/\1/p' /proc/cmdline)"
if command -v ip >/dev/null 2>&1 && [ -n "$IP" ]; then
  ip addr add "$IP" dev eth0 2>/dev/null
  ip link set eth0 up 2>/dev/null
  ip route add default via "$GW" 2>/dev/null
fi
# (no ip binary: krilld also passes kernel ip= autoconfig, which already ran)
# The M3 data contract: /dev/vdb is the app's durable data disk. Anything
# the app wants to outlive redeploys — above all its SQLite database at
# /data/app.db — lives here. No vdb attached (data plane off) is fine.
if [ -b /dev/vdb ]; then
  mkdir -p /data
  mount /dev/vdb /data 2>/dev/null || echo "krill-init: mounting /data failed" >&2
fi
# The M4 identity contract: the doorman signs a short-lived token naming the
# caller, and hands the guest the ed25519 public key on the kernel command
# line. Verify X-Krill-Token against this key and you may believe X-App-User;
# skip it and you are trusting a header. Apps have no outbound network (F6),
# which is exactly why the key arrives here rather than from a JWKS URL.
KRILL_IDENTITY_PUBKEY="$(sed -n 's/.*krill_idkey=\([^ ]*\).*/\1/p' /proc/cmdline)"
export KRILL_IDENTITY_PUBKEY
export HOME="${HOME:-/root}"
`)
	for _, e := range cfg.Env {
		k, v, found := strings.Cut(e, "=")
		if !found || !validEnvName(k) {
			continue
		}
		fmt.Fprintf(&sb, "export %s=%s\n", k, shQuote(v))
	}
	wd := cfg.WorkingDir
	if wd == "" {
		wd = "/"
	}
	fmt.Fprintf(&sb, "cd %s\n", shQuote(wd))
	argv := append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shQuote(a)
	}
	fmt.Fprintf(&sb, "exec %s\n", strings.Join(quoted, " "))
	return sb.String()
}

// shQuote single-quotes s for POSIX sh, the only quoting that never
// interpolates.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func validEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		alpha := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !alpha && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// guessPort picks the numerically smallest EXPOSEd tcp port, so a Dockerfile
// with EXPOSE needs no --port flag at deploy time.
func guessPort(exposed map[string]struct{}) int {
	best := 0
	for k := range exposed {
		numStr, proto, _ := strings.Cut(k, "/")
		if proto != "" && proto != "tcp" {
			continue
		}
		n, err := strconv.Atoi(numStr)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		if best == 0 || n < best {
			best = n
		}
	}
	return best
}

func hasShell(staging string) bool {
	// Lstat, not Stat: /bin/sh is usually a symlink (dash, busybox) whose
	// target is a path inside the image, unresolvable from the host.
	for _, p := range []string{"bin/sh", "usr/bin/sh"} {
		if _, err := os.Lstat(filepath.Join(staging, p)); err == nil {
			return true
		}
	}
	return false
}

func hasIPTool(staging string) bool {
	for _, p := range []string{"sbin/ip", "usr/sbin/ip", "bin/ip", "usr/bin/ip"} {
		if _, err := os.Lstat(filepath.Join(staging, p)); err == nil {
			return true
		}
	}
	return false
}

func hasMountTool(staging string) bool {
	for _, p := range []string{"bin/mount", "usr/bin/mount", "sbin/mount", "usr/sbin/mount"} {
		if _, err := os.Lstat(filepath.Join(staging, p)); err == nil {
			return true
		}
	}
	return false
}

// autoSizeMB sizes the image: tree size with room for ext4 overhead plus the
// app's own writes (its SQLite data lives on this disk until M3), floored at
// 1 GiB — the benchmark's proven size, and sparse files make the floor cheap.
func autoSizeMB(staging string) int {
	var bytes int64
	filepath.WalkDir(staging, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
		}
		return nil
	})
	mb := int(bytes>>20)*2 + 128
	if mb < 1024 {
		mb = 1024
	}
	return mb
}

// PackTarGz writes dir as a gzipped tar — the client half of the deploy
// upload, mirrored by ExtractTarGz on the daemon side. .git is skipped
// (never part of a build context); .dockerignore filtering is docker's job
// at build time.
func PackTarGz(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{
				Name: rel + "/", Typeflag: tar.TypeDir, Mode: int64(info.Mode().Perm()),
			})
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{
				Name: rel, Typeflag: tar.TypeSymlink, Linkname: target,
			})
		case info.Mode().IsRegular():
			hdr := &tar.Header{Name: rel, Mode: int64(info.Mode().Perm()), Size: info.Size()}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		default:
			return nil // sockets, devices: not build-context material
		}
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// ExtractTarGz unpacks an uploaded build context into dst, refusing paths
// that escape it. Supports files, dirs, and symlinks (relative, in-tree
// targets only) — everything a docker build context legitimately contains.
func ExtractTarGz(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("build context is not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
			return fmt.Errorf("build context entry escapes the context: %q", hdr.Name)
		}
		path := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, fs.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fs.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The link target must stay inside the context once resolved.
			target := hdr.Linkname
			if filepath.IsAbs(target) {
				return fmt.Errorf("absolute symlink in build context: %q -> %q", hdr.Name, target)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), target))
			if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return fmt.Errorf("symlink escapes the build context: %q -> %q", hdr.Name, target)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(target, path); err != nil {
				return err
			}
		default:
			// Hardlinks, devices, fifos: nothing a build context needs.
			return fmt.Errorf("unsupported entry type %d in build context: %q", hdr.Typeflag, hdr.Name)
		}
	}
}
