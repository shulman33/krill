package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRun scripts the external commands Build shells out to. It materializes
// a plausible rootfs when "tar -x" runs so the host-side checks (shell
// present, ip present) see real files.
type fakeRun struct {
	t          *testing.T
	calls      []string
	config     string // docker image inspect output
	buildFails bool
	withIP     bool
	withShell  bool
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case name == "docker" && args[0] == "build":
		if f.buildFails {
			return []byte("Step 3/5 : RUN pip install nope\nERROR: no matching distribution"), fmt.Errorf("exit status 1")
		}
		return []byte("Successfully built abc123\n"), nil
	case name == "docker" && args[0] == "image":
		return []byte(f.config + "\n"), nil
	case name == "docker" && args[0] == "create":
		return []byte("deadbeefcafe\n"), nil
	case name == "docker" && (args[0] == "export" || args[0] == "rm"):
		return nil, nil
	case name == "tar":
		dst := args[len(args)-1] // -C <staging> is last
		for _, d := range []string{"bin", "sbin", "usr/bin"} {
			os.MkdirAll(filepath.Join(dst, d), 0o755)
		}
		if f.withShell {
			os.WriteFile(filepath.Join(dst, "bin/sh"), []byte("#!"), 0o755)
		}
		if f.withIP {
			os.WriteFile(filepath.Join(dst, "sbin/ip"), []byte{}, 0o755)
		}
		os.WriteFile(filepath.Join(dst, "app.py"), bytes.Repeat([]byte("x"), 4096), 0o644)
		return nil, nil
	case name == "mkfs.ext4":
		return nil, nil
	}
	f.t.Fatalf("unexpected command: %s", call)
	return nil, nil
}

const goodConfig = `{"Env":["PATH=/usr/local/bin:/usr/bin","LANG=C.UTF-8"],"Cmd":["uvicorn","app:app","--host","0.0.0.0","--port","8000"],"Entrypoint":null,"WorkingDir":"/srv","ExposedPorts":{"8000/tcp":{}}}`

func newTestBuilder(t *testing.T, f *fakeRun) *Builder {
	b := New("docker", t.TempDir())
	f.t = t
	b.Run = f.run
	return b
}

func TestBuildHappyPath(t *testing.T) {
	f := &fakeRun{config: goodConfig, withShell: true, withIP: true}
	b := newTestBuilder(t, f)
	res, err := b.Build(context.Background(), "guestbook", "/ctx", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()

	if res.GuestPort != 8000 {
		t.Errorf("guessed port = %d, want 8000", res.GuestPort)
	}
	if res.SizeMB != 1024 {
		t.Errorf("auto size = %d, want the 1024 floor", res.SizeMB)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
	if _, err := os.Stat(res.GoldenPath); err != nil {
		t.Errorf("golden image missing: %v", err)
	}
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"docker build -t krill-app-guestbook /ctx", "docker create", "docker export", "docker rm", "mkfs.ext4 -q -F -d"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing command %q in:\n%s", want, joined)
		}
	}
	// The golden file was truncated to the declared size before mkfs.
	if st, _ := os.Stat(res.GoldenPath); st.Size() != int64(res.SizeMB)<<20 {
		t.Errorf("golden size = %d, want %d MiB", st.Size(), res.SizeMB)
	}
}

func TestBuildFailureCarriesLog(t *testing.T) {
	f := &fakeRun{config: goodConfig, buildFails: true, withShell: true}
	b := newTestBuilder(t, f)
	_, err := b.Build(context.Background(), "x", "/ctx", 0)
	var be *BuildError
	if !asBuildError(err, &be) || be.Stage != "docker build" {
		t.Fatalf("want BuildError{docker build}, got %v", err)
	}
	if !strings.Contains(be.Log, "no matching distribution") {
		t.Errorf("build log lost: %q", be.Log)
	}
}

func TestBuildRejectsShelllessImage(t *testing.T) {
	f := &fakeRun{config: goodConfig, withShell: false}
	b := newTestBuilder(t, f)
	_, err := b.Build(context.Background(), "x", "/ctx", 0)
	if err == nil || !strings.Contains(err.Error(), "/bin/sh") {
		t.Fatalf("want no-shell error, got %v", err)
	}
}

func TestBuildWarnsWithoutIPTool(t *testing.T) {
	f := &fakeRun{config: goodConfig, withShell: true, withIP: false}
	b := newTestBuilder(t, f)
	res, err := b.Build(context.Background(), "x", "/ctx", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "ip") {
		t.Fatalf("want iproute2 warning, got %v", res.Warnings)
	}
}

func TestBuildRejectsNoCommand(t *testing.T) {
	f := &fakeRun{config: `{"Env":null,"Cmd":null,"Entrypoint":null,"WorkingDir":""}`, withShell: true}
	b := newTestBuilder(t, f)
	_, err := b.Build(context.Background(), "x", "/ctx", 0)
	if err == nil || !strings.Contains(err.Error(), "CMD") {
		t.Fatalf("want no-CMD error, got %v", err)
	}
}

func asBuildError(err error, target **BuildError) bool {
	for err != nil {
		if be, ok := err.(*BuildError); ok {
			*target = be
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestInitScript(t *testing.T) {
	cfg := imageConfig{
		Env:        []string{"PATH=/usr/local/bin", "WEIRD=it's got 'quotes'", "3BAD=skip", "NOEQ"},
		Entrypoint: []string{"/entry.sh"},
		Cmd:        []string{"serve", "--flag", "a b"},
		WorkingDir: "/srv",
	}
	s := InitScript("demo", cfg)
	for _, want := range []string{
		"#!/bin/sh",
		"krill_ip=",
		`export PATH='/usr/local/bin'`,
		`export WEIRD='it'\''s got '\''quotes'\'''`,
		"cd '/srv'",
		`exec '/entry.sh' 'serve' '--flag' 'a b'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("init script missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "3BAD") || strings.Contains(s, "NOEQ") {
		t.Errorf("invalid env names must be skipped:\n%s", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("script must end with a newline")
	}
}

func TestInitScriptDefaultWorkdirAndCmdOnly(t *testing.T) {
	s := InitScript("d", imageConfig{Cmd: []string{"python3", "-m", "http.server"}})
	if !strings.Contains(s, "cd '/'") || !strings.Contains(s, "exec 'python3' '-m' 'http.server'") {
		t.Errorf("bad script:\n%s", s)
	}
}

func TestGuessPort(t *testing.T) {
	cases := []struct {
		in   map[string]struct{}
		want int
	}{
		{nil, 0},
		{map[string]struct{}{"8000/tcp": {}}, 8000},
		{map[string]struct{}{"9000/tcp": {}, "3000/tcp": {}}, 3000},
		{map[string]struct{}{"53/udp": {}}, 0},
		{map[string]struct{}{"8080": {}}, 8080},
	}
	for _, c := range cases {
		if got := guessPort(c.in); got != c.want {
			t.Errorf("guessPort(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func makeTarGz(t *testing.T, entries map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if strings.HasPrefix(content, "->") { // symlink
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: content[2:]}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractTarGzRoundTrip(t *testing.T) {
	data := makeTarGz(t, map[string]string{
		"Dockerfile": "FROM x",
		"src/app.py": "print(1)",
		"link.py":    "->src/app.py",
	})
	dst := t.TempDir()
	if err := ExtractTarGz(bytes.NewReader(data), dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "src/app.py"))
	if err != nil || string(got) != "print(1)" {
		t.Fatalf("file content: %q, %v", got, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link.py")); err != nil || target != "src/app.py" {
		t.Fatalf("symlink: %q, %v", target, err)
	}
}

func TestPackExtractRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "src"), 0o755)
	os.MkdirAll(filepath.Join(src, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM x"), 0o644)
	os.WriteFile(filepath.Join(src, "src/app.py"), []byte("print(1)"), 0o644)
	os.WriteFile(filepath.Join(src, ".git/objects/junk"), []byte("z"), 0o644)
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh"), 0o755)
	os.Symlink("src/app.py", filepath.Join(src, "main.py"))

	var buf bytes.Buffer
	if err := PackTarGz(src, &buf); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := ExtractTarGz(&buf, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "src/app.py")); string(got) != "print(1)" {
		t.Fatalf("content: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git must not travel")
	}
	if st, err := os.Stat(filepath.Join(dst, "run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec bit lost: %v %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "main.py")); err != nil || target != "src/app.py" {
		t.Fatalf("symlink: %q %v", target, err)
	}
}

func TestExtractTarGzRejectsEscapes(t *testing.T) {
	for name, entries := range map[string]map[string]string{
		"dotdot path":      {"../evil": "x"},
		"absolute symlink": {"l": "->/etc/passwd"},
		"escaping symlink": {"a/l": "->../../outside"},
	} {
		data := makeTarGz(t, entries)
		if err := ExtractTarGz(bytes.NewReader(data), t.TempDir()); err == nil {
			t.Errorf("%s: extraction must fail", name)
		}
	}
}
