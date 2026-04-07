//go:build !windows

package upstream

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr isolates the upstream process into its own process group.
// Prevents signals sent to the daemon from propagating to upstream children.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
