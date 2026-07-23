package guestlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pythonCrash = `[    0.612514] Run /krill-init.sh as init process
Traceback (most recent call last):
  File "/usr/local/bin/uvicorn", line 8, in <module>
    sys.exit(main())
  File "/srv/app.py", line 10, in <module>
    app = FastAPI()
NameError: name 'FastAPI' is not defined. Did you mean: 'fastapi'?
[    1.101323] Kernel panic - not syncing: Attempted to kill init! exitcode=0x00000100
`

func TestParsePythonTraceback(t *testing.T) {
	errs := Parse(Clean(pythonCrash))
	if len(errs) != 2 {
		t.Fatalf("want 2 errors (traceback + kernel panic), got %d: %+v", len(errs), errs)
	}
	tb := errs[0]
	if tb.Kind != "python_traceback" {
		t.Fatalf("kind = %q", tb.Kind)
	}
	if !strings.HasPrefix(tb.Message, "NameError: name 'FastAPI' is not defined") {
		t.Fatalf("message = %q", tb.Message)
	}
	if !strings.Contains(tb.Detail, `File "/srv/app.py", line 10`) {
		t.Fatalf("detail lost the offending frame: %q", tb.Detail)
	}
	kp := errs[1]
	if kp.Kind != "kernel_panic" || kp.Hint == "" {
		t.Fatalf("kernel panic not recognized or missing hint: %+v", kp)
	}
}

func TestParseChainedTracebacks(t *testing.T) {
	log := `Traceback (most recent call last):
  File "a.py", line 1, in <module>
    x()
KeyError: 'a'

During handling of the above exception, another exception occurred:

Traceback (most recent call last):
  File "a.py", line 3, in <module>
    y()
RuntimeError: boom
`
	errs := Parse(Clean(log))
	if len(errs) != 2 {
		t.Fatalf("want both tracebacks, got %d: %+v", len(errs), errs)
	}
	if errs[0].Message != "KeyError: 'a'" || errs[1].Message != "RuntimeError: boom" {
		t.Fatalf("messages: %q, %q", errs[0].Message, errs[1].Message)
	}
}

func TestParseNodeError(t *testing.T) {
	log := `> start
Error: Cannot find module 'express'
    at Module._resolveFilename (node:internal/modules/cjs/loader:1145:15)
    at Module._load (node:internal/modules/cjs/loader:986:27)
uvicorn running
TypeError: nope
`
	errs := Parse(Clean(log))
	if len(errs) != 1 {
		t.Fatalf("want 1 node error (bare TypeError line has no frames), got %d: %+v", len(errs), errs)
	}
	if errs[0].Kind != "node_error" || !strings.Contains(errs[0].Detail, "Module._load") {
		t.Fatalf("bad node error: %+v", errs[0])
	}
}

func TestParseGoPanic(t *testing.T) {
	errs := Parse([]string{"panic: runtime error: index out of range [3]"})
	if len(errs) != 1 || errs[0].Kind != "panic" {
		t.Fatalf("got %+v", errs)
	}
}

func TestCleanStripsCRAndANSI(t *testing.T) {
	lines := Clean("a\r\n\x1b[0;32mgreen\x1b[0m\r\n")
	if len(lines) != 2 || lines[0] != "a" || lines[1] != "green" {
		t.Fatalf("got %q", lines)
	}
}

func TestParseIgnoresPlainOutput(t *testing.T) {
	log := `[    0.5] booting
INFO:     Uvicorn running on http://0.0.0.0:8000
INFO:     172.16.0.1:41712 - "GET / HTTP/1.1" 200 OK
`
	if errs := Parse(Clean(log)); len(errs) != 0 {
		t.Fatalf("plain output produced errors: %+v", errs)
	}
}

func TestTailFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "serial.log")
	if _, err := TailFile(p, 10); err != nil {
		t.Fatalf("missing file must be an empty log, got %v", err)
	}
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(strings.Repeat("x", 20) + "\n")
	}
	b.WriteString("last-line\n")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := TailFile(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 5 || lines[4] != "last-line" {
		t.Fatalf("tail wrong: %q", lines)
	}
}
