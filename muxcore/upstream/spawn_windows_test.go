//go:build windows

package upstream

import (
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
	if p.jobHandle == 0 {
		t.Fatal("Start succeeded without a Windows tree authority")
	}

	original := windows.Handle(p.jobHandle)
	var successor windows.Handle
	current := windows.CurrentProcess()
	if err := windows.DuplicateHandle(
		current, original, current, &successor, 0, false, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		t.Fatalf("DuplicateHandle: %v", err)
	}
	p.jobHandle = uintptr(successor)
	if err := windows.CloseHandle(original); err != nil {
		t.Fatalf("close predecessor authority: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	select {
	case <-p.Done:
		t.Fatal("upstream exited while successor authority remained open")
	default:
	}

	p.jobHandle = 0
	if err := windows.CloseHandle(successor); err != nil {
		t.Fatalf("close successor authority: %v", err)
	}
	select {
	case <-p.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream survived closing the final transferred authority")
	}
}
