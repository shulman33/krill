// Package guestlog turns a guest's serial console log into something an
// agent can act on: a raw tail plus structured runtime errors (Python
// tracebacks, Node errors, panics). This is the feedback half of the M2
// deploy loop — a broken deploy must explain itself through the API, never
// through "ssh in and read a file".
//
// The serial log is untrusted guest output. Parsing is heuristic by design:
// extract the error shapes agents actually hit, pass everything else through
// as plain lines.
package guestlog

import (
	"os"
	"regexp"
	"strings"
)

type Error struct {
	// Kind is one of python_traceback, node_error, panic, kernel_panic.
	Kind string `json:"kind"`
	// Message is the one-line summary (e.g. the Python exception line).
	Message string `json:"message"`
	// Detail is the full multi-line block the message was extracted from.
	Detail string `json:"detail"`
	// Hint is the parser's interpretation, when it has one worth stating.
	Hint string `json:"hint,omitempty"`
}

// maxTailBytes bounds how much of the log is read: serial logs grow without
// rotation for an app's whole ACTIVE life.
const maxTailBytes = 512 << 10

// TailFile reads up to the last n lines of the serial log. A missing file is
// an empty log, not an error — the app may simply never have booted.
func TailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := st.Size() - maxTailBytes
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	lines := Clean(string(buf))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is almost certainly truncated
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// Clean splits raw serial output into lines, stripping CR and ANSI escapes.
func Clean(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r", "")
	raw = ansiRE.ReplaceAllString(raw, "")
	lines := strings.Split(raw, "\n")
	// Drop a single trailing empty line from the final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

var (
	pyExcRE     = regexp.MustCompile(`^[A-Za-z_][\w.]*(Error|Exception|Interrupt|Exit|Warning)?\s*:`)
	nodeErrRE   = regexp.MustCompile(`^(?:[A-Za-z_$][\w$]*)?Error: `)
	nodeFrameRE = regexp.MustCompile(`^\s+at `)
	goPanicRE   = regexp.MustCompile(`^panic: `)
	kernelRE    = regexp.MustCompile(`Kernel panic - not syncing: (.*)`)
)

// Parse extracts structured errors from cleaned log lines, in order of
// appearance.
func Parse(lines []string) []Error {
	var out []Error
	consumed := make([]bool, len(lines))

	// Pass 1: Python tracebacks. Block = the Traceback header, every indented
	// line after it, ended by the first non-indented line (the exception).
	for i := 0; i < len(lines); i++ {
		if !strings.Contains(lines[i], "Traceback (most recent call last):") {
			continue
		}
		start := i
		j := i + 1
		for j < len(lines) && (lines[j] == "" || strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t")) {
			j++
		}
		msg := "unknown Python error"
		if j < len(lines) && pyExcRE.MatchString(lines[j]) {
			msg = lines[j]
		} else if j < len(lines) && lines[j] != "" {
			msg = lines[j]
		}
		for k := start; k <= j && k < len(lines); k++ {
			consumed[k] = true
		}
		out = append(out, Error{
			Kind:    "python_traceback",
			Message: msg,
			Detail:  strings.Join(lines[start:min(j+1, len(lines))], "\n"),
		})
		i = j
	}

	// Pass 2: Node errors — an XxxError: line followed by `at` stack frames.
	for i := 0; i < len(lines); i++ {
		if consumed[i] || !nodeErrRE.MatchString(lines[i]) {
			continue
		}
		j := i + 1
		frames := 0
		for j < len(lines) && (nodeFrameRE.MatchString(lines[j]) || lines[j] == "") {
			if nodeFrameRE.MatchString(lines[j]) {
				frames++
			}
			j++
		}
		if frames == 0 {
			continue // a bare "SomethingError:" line without a stack is just output
		}
		for k := i; k < j; k++ {
			consumed[k] = true
		}
		out = append(out, Error{
			Kind:    "node_error",
			Message: lines[i],
			Detail:  strings.Join(lines[i:j], "\n"),
		})
		i = j - 1
	}

	// Pass 3: Go panics and kernel panics.
	for i, l := range lines {
		if consumed[i] {
			continue
		}
		if m := kernelRE.FindStringSubmatch(l); m != nil {
			e := Error{Kind: "kernel_panic", Message: strings.TrimSpace(m[1]), Detail: l}
			if strings.Contains(m[1], "Attempted to kill init") {
				e.Hint = "the app's main process exited; the error above it in the log says why"
			}
			out = append(out, e)
			continue
		}
		if goPanicRE.MatchString(l) {
			out = append(out, Error{Kind: "panic", Message: l, Detail: l})
		}
	}
	return out
}
