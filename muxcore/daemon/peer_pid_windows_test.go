//go:build windows

package daemon

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestVerifyPIDOwnerWin_OwnProcess(t *testing.T) {
	if err := verifyPIDOwner(uint32(os.Getpid())); err != nil {
		t.Errorf("own process rejected: %v", err)
	}
}

func TestVerifyPIDOwnerWin_InvalidPID(t *testing.T) {
	err := verifyPIDOwner(0)
	if !errors.Is(err, ErrPIDNotFoundWin) {
		t.Errorf("expected ErrPIDNotFoundWin for pid 0, got %v", err)
	}
	// PID 0xFFFFFFFF is guaranteed invalid on Windows.
	err = verifyPIDOwner(0xFFFFFFFF)
	if err == nil {
		t.Errorf("expected error for invalid pid, got nil")
	}
}

// TestVerifyPIDOwnerWin_SystemProcess — the SYSTEM process (pid 4, "System")
// runs as NT AUTHORITY\SYSTEM. A non-admin regular user cannot OpenProcess
// it with PROCESS_QUERY_LIMITED_INFORMATION + read its token → access denied.
// This is the cross-session signal.
func TestVerifyPIDOwnerWin_SystemProcess(t *testing.T) {
	// Skip if running as SYSTEM (CI runners occasionally do).
	token := windows.GetCurrentProcessToken()
	elevated := token.IsElevated()
	if elevated {
		t.Skip("running elevated — SYSTEM check would succeed, not a cross-user signal")
	}
	err := verifyPIDOwner(4) // System process PID
	if err == nil {
		t.Errorf("expected error for SYSTEM process as non-elevated, got nil")
	}
	// Either ForeignOwner (access denied) or NotFound (pid not queryable) is acceptable;
	// both imply "not our process" which is the gate's intent.
	if !errors.Is(err, ErrPIDForeignOwnerWin) && !errors.Is(err, ErrPIDNotFoundWin) {
		t.Errorf("expected typed cross-user error, got generic: %v", err)
	}
}
