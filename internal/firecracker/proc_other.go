//go:build !linux

package firecracker

import "os/exec"

// Firecracker only runs on Linux/KVM; this stub exists so the package (and
// its tests) build on development machines.
func setProcAttr(cmd *exec.Cmd) {}
