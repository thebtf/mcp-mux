//go:build !windows

package upstream

import (
	"os"
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

// interruptProcess sends SIGINT to the process for graceful shutdown.
func interruptProcess(p *os.Process) {
	_ = p.Signal(syscall.SIGINT)
}
