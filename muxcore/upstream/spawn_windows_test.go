//go:build windows

package upstream

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestUpstreamJob_SurvivesDaemonJobClose proves the two-phase authority lease:
// predecessor close is safe after duplication, and final authority close reaps the tree.
func TestUpstreamJob_SurvivesDaemonJobClose(t *testing.T) {
	p, err := Start("ping", []string{"-n", "30", "127.0.0.1"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.authorityMu.Lock()
	original := windows.Handle(p.jobHandle)
	p.authorityMu.Unlock()
	if original == 0 {
		t.Fatal("Start succeeded without a Windows tree authority")
	}

	var successor windows.Handle
	current := windows.CurrentProcess()
	if err := windows.DuplicateHandle(
		current, original, current, &successor, 0, false, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		t.Fatalf("DuplicateHandle: %v", err)
	}
	p.authorityMu.Lock()
	p.jobHandle = uintptr(successor)
	p.authorityMu.Unlock()
	if err := windows.CloseHandle(original); err != nil {
		t.Fatalf("close predecessor authority: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	select {
	case <-p.Done:
		t.Fatal("upstream exited while successor authority remained open")
	default:
	}

	p.authorityMu.Lock()
	p.jobHandle = 0
	p.authorityMu.Unlock()
	if err := windows.CloseHandle(successor); err != nil {
		t.Fatalf("close successor authority: %v", err)
	}
	select {
	case <-p.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream survived closing the final transferred authority")
	}
}

func TestResumeFailureRetainsJobAuthorityUntilFinalization(t *testing.T) {
	setupErr := errors.New("injected resume failure")
	retirementErr := errors.New("injected unproven retirement")
	previousResume := resumeSpawnedProcessThreads
	previousFinalizer := finalizeFailedStartTree
	var captured *Process
	var sawJobAuthority bool

	resumeSpawnedProcessThreads = func(int) error { return setupErr }
	finalizeFailedStartTree = func(p *Process) error {
		captured = p
		p.authorityMu.Lock()
		sawJobAuthority = p.jobHandle != 0
		p.authorityMu.Unlock()
		return retirementErr
	}
	t.Cleanup(func() {
		resumeSpawnedProcessThreads = previousResume
		finalizeFailedStartTree = previousFinalizer
		if captured == nil || captured.RetirementProven() {
			return
		}
		if err := captured.finalizeOwnedTree(); err != nil {
			t.Errorf("finalize retained resume-failure authority: %v", err)
			return
		}
		select {
		case <-captured.Done:
		case <-time.After(2 * time.Second):
			t.Error("resume-failure process did not exit during cleanup")
		}
	})

	proc, err := Start("ping", []string{"-n", "30", "127.0.0.1"}, nil, "", nil)
	if proc == nil {
		t.Fatal("Start() process = nil, want retained resume-failure authority")
	}
	if proc != captured {
		t.Fatal("Start() did not return the resume-failure Process authority")
	}
	if !sawJobAuthority {
		t.Fatal("Windows Job authority was discarded before failed-start finalization")
	}
	if !errors.Is(err, setupErr) || !errors.Is(err, retirementErr) {
		t.Fatalf("Start() error = %v, want resume and retirement failures", err)
	}
	if proc.RetirementProven() {
		t.Fatal("unfinalized resume-failure process reported proven retirement")
	}
}
