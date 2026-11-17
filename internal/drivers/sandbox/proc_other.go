//go:build !linux

package sandbox

import "os/exec"

// tieToParent has nothing to do where gVisor does not run.
func tieToParent(*exec.Cmd) {}
