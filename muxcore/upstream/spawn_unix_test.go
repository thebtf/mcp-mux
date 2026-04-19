//go:build unix

package upstream

import (
	"os"
	"syscall"
	"testing"
)

// TestStart_Setpgid verifies that an upstream process started via
// upstream.Start() is in its own process group, not the daemon's.
func TestStart_Setpgid(t *testing.T) {
	proc, err := Start("sleep", []string{"30"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer proc.Close()

	childPID := proc.PID()
	if childPID == 0 {
		t.Fatal("child PID is 0")
	}

	// syscall.Getpgid returns the PGID of the given PID.
	childPGID, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatalf("Getpgid(child): %v", err)
	}

	daemonPGID, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("Getpgid(daemon): %v", err)
	}

	if childPGID == daemonPGID {
		t.Errorf("child PGID=%d equals daemon PGID=%d — setpgid did NOT take effect",
			childPGID, daemonPGID)
	}
	if childPGID != childPID {
		t.Errorf("child PGID=%d does not equal child PID=%d (setpgid should set PGID=PID)",
			childPGID, childPID)
	}
}
