//go:build darwin || freebsd || openbsd || netbsd

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// readPIDUID uses a best-effort strategy on BSD variants: the combination of
// kill(pid, 0) succeeding + returning the current process's UID is a weak proxy
// for same-owner, because kill(0) returns EPERM when the target belongs to a
// different UID (unless we're root, which can signal anyone). This is NOT
// cryptographically strong — the handoff token handshake (FR-11) remains the
// primary auth. verifyPIDOwner's role here is defense-in-depth.
//
// A proper implementation would use libproc's proc_pidinfo + KERN_PROC_PID
// sysctl. T013 (macOS CI) will validate the defense-in-depth value empirically.
func readPIDUID(pid int) (int, error) {
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, fmt.Errorf("kill(%d, 0): %w", pid, err)
	}
	// Weak: we claim the PID is ours because kill(0) didn't return EPERM.
	// Upstream caller must cross-check token (NFR-5 + FR-11).
	return os.Getuid(), nil
}
