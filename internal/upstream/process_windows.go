//go:build windows

package upstream

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr isolates the upstream process from the daemon's console group.
// Without this, CTRL_C_EVENT from CC (sent to shim) propagates through the
// shared console to daemon's children, killing upstream mid-request.
// STATUS_CONTROL_C_EXIT (0xc000013a) was observed killing netcoredbg during
// worktree dotnet build (~46s) while main builds (~11s) completed before
// any signal arrived.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
