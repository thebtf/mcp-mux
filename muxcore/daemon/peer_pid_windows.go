//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrPIDForeignOwnerWin is the Windows counterpart to ErrPIDForeignOwner.
// Same semantic: target PID belongs to a different user account (TokenUser SID
// mismatch). Session isolation beyond user-level is not enforced here; a process
// running as the same user in a different logon session will pass this check.
// NFR-5 requires only user-level isolation, which TokenUser provides.
var ErrPIDForeignOwnerWin = errors.New("handoff: pid not owned by current user")

// ErrPIDNotFoundWin — target PID does not exist or can't be opened.
var ErrPIDNotFoundWin = errors.New("handoff: pid not found")

// verifyPIDOwner (Windows) asserts that the target PID runs under the same
// user SID as the current process. Implements NFR-5 on Windows using the
// Token ownership SID (TokenUser class).
//
// Flow:
//  1. OpenProcess with PROCESS_QUERY_LIMITED_INFORMATION (minimum required
//     to open a token). Many cross-user PIDs fail at this step with
//     ERROR_ACCESS_DENIED — that's a cross-user signal → reject.
//  2. OpenProcessToken with TOKEN_QUERY.
//  3. GetTokenInformation with TokenUser → SID of target.
//  4. Compare against current process's token SID. Reject if different.
func verifyPIDOwner(pid uint32) error {
	if pid == 0 {
		return fmt.Errorf("%w: invalid pid 0", ErrPIDNotFoundWin)
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		// ERROR_INVALID_PARAMETER / ERROR_ACCESS_DENIED both mean either
		// pid doesn't exist (former) or exists but we can't query it
		// (latter — commonly cross-user). Fold both into NotFound/foreign.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("%w: pid %d (access denied — likely cross-user)",
				ErrPIDForeignOwnerWin, pid)
		}
		return fmt.Errorf("%w: pid %d: %v", ErrPIDNotFoundWin, pid, err)
	}
	defer windows.CloseHandle(h)

	targetSID, err := tokenSIDFromProcess(h)
	if err != nil {
		return fmt.Errorf("verifyPIDOwner(win): target token: %w", err)
	}

	selfHandle := windows.CurrentProcess()
	selfSID, err := tokenSIDFromProcess(selfHandle)
	if err != nil {
		return fmt.Errorf("verifyPIDOwner(win): self token: %w", err)
	}

	if !windows.EqualSid(targetSID, selfSID) {
		return fmt.Errorf("%w: pid %d token sid differs from current process",
			ErrPIDForeignOwnerWin, pid)
	}
	return nil
}

// tokenSIDFromProcess opens the process token and returns the user's SID.
// The returned SID is a copy allocated via windows.CopySid — safe for the
// caller to use after the token is closed; GC-managed.
func tokenSIDFromProcess(processHandle windows.Handle) (*windows.SID, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(processHandle, windows.TOKEN_QUERY, &token); err != nil {
		return nil, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer token.Close()

	// GetTokenInformation with TokenUser class.
	// First call with 0 buffer learns the required size.
	var bufSize uint32
	err := windows.GetTokenInformation(token, windows.TokenUser, nil, 0, &bufSize)
	if err != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, fmt.Errorf("GetTokenInformation(sizeprobe): %w", err)
	}
	buf := make([]byte, bufSize)
	if err := windows.GetTokenInformation(
		token, windows.TokenUser, &buf[0], bufSize, &bufSize,
	); err != nil {
		return nil, fmt.Errorf("GetTokenInformation: %w", err)
	}

	tu := (*windows.Tokenuser)(unsafe.Pointer(&buf[0]))
	// tu.User.Sid points into buf; copy the SID so the backing slice can be
	// GC'd without invalidating the returned *SID.
	copied, err := tu.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("sid copy: %w", err)
	}
	return copied, nil
}
