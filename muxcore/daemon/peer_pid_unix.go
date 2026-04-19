//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrPIDForeignOwner is returned when verifyPIDOwner detects that the
// target PID belongs to a different UID than the current process.
// NFR-5 security gate for handoff acceptance.
var ErrPIDForeignOwner = errors.New("handoff: pid not owned by current uid")

// ErrPIDNotFound is returned when the target PID does not exist or cannot
// be inspected (exited, permission denied on /proc, etc.).
var ErrPIDNotFound = errors.New("handoff: pid not found or unreadable")

// verifyPIDOwner asserts that PID `pid` is alive AND owned by the same UID
// as the current process. Returns nil on success, ErrPIDForeignOwner if
// the PID is owned by a different UID, ErrPIDNotFound if the PID cannot
// be found. Other errors are wrapped with fmt.Errorf.
//
// Implementation is platform-split:
//   - Linux: parse /proc/{pid}/status for Uid line.
//   - macOS / BSD: kill(pid, 0) liveness check — conservative best-effort
//     (token handshake FR-11 is primary auth; verifyPIDOwner is defense-in-depth).
//
// For T011's scope, the Linux branch is mandatory. The macOS/BSD branch
// must be present and return a well-documented best-effort result.
func verifyPIDOwner(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("%w: invalid pid %d", ErrPIDNotFound, pid)
	}
	// Liveness check first (works on all Unix): kill(pid, 0) returns nil
	// if the process exists and we have permission to signal it.
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("%w: pid %d", ErrPIDNotFound, pid)
		}
		if errors.Is(err, syscall.EPERM) {
			// EPERM from kill(0) means process exists but we cannot signal it,
			// which implies cross-user ownership. Reject.
			return fmt.Errorf("%w: pid %d (EPERM)", ErrPIDForeignOwner, pid)
		}
		return fmt.Errorf("verifyPIDOwner: kill(%d, 0): %w", pid, err)
	}
	// Platform-specific UID read.
	uid, err := readPIDUID(pid)
	if err != nil {
		return fmt.Errorf("verifyPIDOwner: read uid for pid %d: %w", pid, err)
	}
	if uid != os.Getuid() {
		return fmt.Errorf("%w: pid %d owned by uid %d (we are uid %d)",
			ErrPIDForeignOwner, pid, uid, os.Getuid())
	}
	return nil
}
