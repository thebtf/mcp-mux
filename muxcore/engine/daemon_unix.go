//go:build !windows

package engine

import (
	"os/exec"
	"syscall"
)

// setDetached configures cmd to run as a detached background process.
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
