//go:build linux

package sandbox

import (
	"os/exec"
	"syscall"
)

// tieToParent makes the kernel kill the job when this process dies. A killed
// engine cannot clean up after itself, and a sandbox that outlives the engine
// that started it is the one thing a stateless design must not leave behind.
// The signal survives the exec chain down to the sandbox's own processes,
// which exit when their launcher does.
func tieToParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
