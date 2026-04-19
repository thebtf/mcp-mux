//go:build windows

package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// handoffSocketPath returns a random named-pipe suffix for the handoff channel.
// listenHandoffWindows prepends the \\.\pipe\mcp-mux-handoff- prefix.
// Each graceful restart uses a fresh suffix to avoid stale-pipe conflicts.
// Falls back to time+PID if crypto/rand is unavailable (extremely rare).
func handoffSocketPath(_ string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Crypto/rand unavailable — use time+PID as a deterministic fallback.
		// Not cryptographically strong but still unique enough to avoid stale-pipe conflicts.
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
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

// dialHandoff connects to a handoff named pipe previously bound by listenHandoff.
// Caller owns the returned conn's lifetime.
func dialHandoff(socketPath string, timeout time.Duration) (fdConn, error) {
	return dialHandoffWindows(socketPath, timeout)
}
