//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
	"time"
)

// platformState holds Unix-specific process state (none needed beyond the pgid,
// which is derived from pid).
type platformState struct{}

// configurePlatform sets the process group so the entire tree can be signalled
// as a unit. Setpgid=true causes the child to become the leader of a new process
// group (pgid == child pid), isolating it from the parent's process group.
func configurePlatform(cmd *exec.Cmd, _ *Process) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

// postStart is a no-op on Unix; the process group is already set via Setpgid.
func (p *Process) postStart() error { return nil }

// gracefulKillPlatform sends SIGTERM to the process group, waits for timeout,
// then sends SIGKILL to the process group.
func (p *Process) gracefulKillPlatform(timeout time.Duration) error {
	pgid := -p.pid // negative PID targets the entire process group

	// Best-effort SIGTERM; ignore error (process may have already exited).
	_ = syscall.Kill(pgid, syscall.SIGTERM)

	select {
	case <-p.done:
		return nil
	case <-time.After(timeout):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-p.done
		return nil
	}
}

// killPlatform sends SIGKILL to the entire process group immediately.
func (p *Process) killPlatform() error {
	pgid := -p.pid
	return syscall.Kill(pgid, syscall.SIGKILL)
}
