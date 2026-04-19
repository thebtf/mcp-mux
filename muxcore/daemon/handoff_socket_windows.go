//go:build windows

package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"syscall"
	"time"
)

// handoffSocketPath returns a random named-pipe suffix for the handoff channel.
// listenHandoffWindows prepends the \\.\pipe\mcp-mux-handoff- prefix.
// Each graceful restart uses a fresh suffix to avoid stale-pipe conflicts.
func handoffSocketPath(_ string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// listenHandoff accepts one connection on the handoff named pipe.
// Delegates to listenHandoffWindows which enforces current-user DACL (NFR-5).
func listenHandoff(socketPath string, timeout time.Duration) (fdConn, error) {
	return listenHandoffWindows(socketPath, timeout)
}

// setSuccessorDetached configures cmd to run as a detached background process
// on Windows. Mirrors engine.setDetached for the daemon package (importing
// engine from daemon would create an import cycle).
func setSuccessorDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
