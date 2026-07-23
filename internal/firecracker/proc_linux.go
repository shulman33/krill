//go:build linux

package firecracker

import (
	"os/exec"
	"syscall"
)

// setProcAttr ties each VMM's lifetime to krilld's: if the daemon dies, the
// kernel SIGKILLs the guests. Orphaned VMMs holding taps and disk images are
// worse than killed ones — a killed FROZEN-bound guest just cold-boots later.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
