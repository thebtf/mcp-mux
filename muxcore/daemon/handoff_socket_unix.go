//go:build unix

package daemon

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// handoffSocketPath returns the Unix domain socket path for the handoff channel.
// The socket is created in baseDir (typically os.TempDir()) so it is accessible
// to both old and new daemon processes running as the same user.
func handoffSocketPath(baseDir string) string {
	return filepath.Join(baseDir, "mcp-mux-handoff.sock")
}

// listenHandoff accepts one connection on the handoff Unix domain socket.
// Delegates to listenHandoffUnix which enforces 0600 permissions (FR-29 / NFR-5).
func listenHandoff(socketPath string, timeout time.Duration) (fdConn, error) {
	return listenHandoffUnix(socketPath, timeout)
}

// setSuccessorDetached configures cmd to run in a new session so the successor
// daemon survives the old daemon's exit without receiving SIGHUP or inheriting
// the old daemon's process group. Mirrors engine.setDetached for the daemon package
// (importing engine from daemon would create an import cycle).
func setSuccessorDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// dialHandoff connects to a handoff socket previously bound by listenHandoff.
// Caller owns the returned conn's lifetime.
func dialHandoff(socketPath string, timeout time.Duration) (fdConn, error) {
	return dialHandoffUnix(socketPath, timeout)
}
