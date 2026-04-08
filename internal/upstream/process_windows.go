//go:build windows

package upstream

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr configures the upstream process for headless operation.
// CREATE_NO_WINDOW (0x08000000) creates the process without a visible window
// but WITH a valid console — required by dotnet build, MSBuild, and other
// tools that write to console output. Without a console, these tools hang
// indefinitely on progress output.
//
// We do NOT use CREATE_NEW_PROCESS_GROUP here because it, combined with the
// daemon's HideWindow=true, creates an environment without a valid console
// handle. dotnet build detected this and hung on console write.
func setSysProcAttr(cmd *exec.Cmd) {
	const CREATE_NO_WINDOW = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: CREATE_NO_WINDOW,
	}
}
