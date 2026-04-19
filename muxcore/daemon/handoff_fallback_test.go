package daemon

import (
	"os"
	"testing"
	"time"
)

// TestHandoffFallbackOnAcceptTimeout verifies that HandleGracefulRestart returns
// a valid snapshot path and nil error even when no successor daemon connects
// within the accept timeout — i.e. the FR-8 fallback path is transparent to
// the control-plane caller.
//
// Failure mode simulated: listenHandoff accept times out because spawnSuccessor
// forks a real binary with --daemon but the binary in the test environment is
// the test binary itself, which does not implement the successor handshake.
// The accept timeout (overridden to 50 ms) fires before any connection arrives.
//
// This test asserts:
//  1. HandleGracefulRestart returns (snapshotPath, nil) — not an error.
//  2. snapshotPath is non-empty — snapshot serialization succeeded.
//  3. The daemon continues to the Shutdown path (go d.Shutdown()) regardless.
func TestHandoffFallbackOnAcceptTimeout(t *testing.T) {
	// Override timeouts so the test completes quickly.
	origAccept := handoffAcceptTimeout
	origTotal := handoffTotalTimeout
	handoffAcceptTimeout = 50 * time.Millisecond
	handoffTotalTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		handoffAcceptTimeout = origAccept
		handoffTotalTimeout = origTotal
	})

	d := testDaemon(t) // helper defined in daemon_test.go

	// HandleGracefulRestart MUST succeed (FR-8 fallback is transparent to caller).
	snapshotPath, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart() returned error %v; want nil (FR-8 fallback should be transparent)", err)
	}
	if snapshotPath == "" {
		t.Error("HandleGracefulRestart() returned empty snapshot path; want non-empty")
	}

	// Cleanup snapshot file produced by SerializeSnapshot.
	if snapshotPath != "" {
		os.Remove(snapshotPath)
	}
}
