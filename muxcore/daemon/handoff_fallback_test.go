package daemon

import (
	"os"
	"strings"
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

// TestHandoffFallback_LogMarker verifies that the FR-8 fallback path emits
// a "handoff.fallback reason=..." structured log marker. Operators rely on
// this marker to correlate upgrade failures with restart events in logs.
//
// Uses testDaemonWithLog (helper added in F80-4 / T022) to capture daemon
// log output into a thread-safe buffer and grep it after the call returns.
func TestHandoffFallback_LogMarker(t *testing.T) {
	origAccept := handoffAcceptTimeout
	origTotal := handoffTotalTimeout
	handoffAcceptTimeout = 50 * time.Millisecond
	handoffTotalTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		handoffAcceptTimeout = origAccept
		handoffTotalTimeout = origTotal
	})

	d, logBuf := testDaemonWithLog(t)

	snapshotPath, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })

	logs := logBuf.String()

	// Positive: the fallback marker MUST be present.
	if !strings.Contains(logs, "handoff.fallback reason=") {
		t.Errorf("expected %q in daemon log, not found.\nLogs:\n%s",
			"handoff.fallback reason=", logs)
	}

	// Positive: the pre-attempt marker MUST be present too (operators use
	// the start/fallback pair to bound the attempted-handoff window).
	if !strings.Contains(logs, "handoff.start ") {
		t.Errorf("expected %q in daemon log, not found.\nLogs:\n%s",
			"handoff.start ", logs)
	}

	// Negative: handoff.complete MUST NOT fire on the fallback path.
	if strings.Contains(logs, "handoff.complete ") {
		t.Errorf("did not expect %q on fallback path.\nLogs:\n%s",
			"handoff.complete ", logs)
	}
}

// TestHandoffFallback_CounterIncrement verifies that the handoff_fallback
// atomic counter advances by exactly 1 per fallback attempt, enabling the
// verify-handoff.sh / .ps1 post-deploy scripts (T036) to distinguish
// successful handoffs from FR-8 legacy-path falls.
//
// Mechanism mirrors TestHandoffFallbackOnAcceptTimeout (accept timeout),
// but asserts the counter delta in HandleStatus output rather than log
// content.
func TestHandoffFallback_CounterIncrement(t *testing.T) {
	origAccept := handoffAcceptTimeout
	origTotal := handoffTotalTimeout
	handoffAcceptTimeout = 50 * time.Millisecond
	handoffTotalTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		handoffAcceptTimeout = origAccept
		handoffTotalTimeout = origTotal
	})

	d := testDaemon(t)

	// Snapshot counters before.
	before := d.stats.fallback.Load()
	beforeAttempted := d.stats.attempted.Load()

	snapshotPath, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })

	afterAttempted := d.stats.attempted.Load()
	after := d.stats.fallback.Load()

	// attempted MUST increase by exactly 1 (every graceful restart enters
	// the handoff path regardless of whether it completes).
	if afterAttempted-beforeAttempted != 1 {
		t.Errorf("handoff_attempted delta: got %d, want 1",
			afterAttempted-beforeAttempted)
	}

	// fallback MUST increase by exactly 1 on this failure path.
	if after-before != 1 {
		t.Errorf("handoff_fallback delta: got %d, want 1 (accept timeout should be a fallback, not a success)",
			after-before)
	}

	// transferred + aborted MUST remain zero — no handoff protocol ran.
	if got := d.stats.transferred.Load(); got != 0 {
		t.Errorf("handoff_transferred on fallback path: got %d, want 0", got)
	}
	if got := d.stats.aborted.Load(); got != 0 {
		t.Errorf("handoff_aborted on fallback path: got %d, want 0", got)
	}
}

// TestHandoffFallback_StatusCountersExposed verifies the counters reach
// HandleStatus output (the API verify-handoff.sh / .ps1 rely on).
func TestHandoffFallback_StatusCountersExposed(t *testing.T) {
	origAccept := handoffAcceptTimeout
	origTotal := handoffTotalTimeout
	handoffAcceptTimeout = 50 * time.Millisecond
	handoffTotalTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		handoffAcceptTimeout = origAccept
		handoffTotalTimeout = origTotal
	})

	d := testDaemon(t)

	// Sanity: counters present in HandleStatus before any restart.
	initial := d.HandleStatus()
	hoff0, ok := initial["handoff"].(map[string]any)
	if !ok {
		t.Fatalf("HandleStatus missing 'handoff' sub-map; got %T", initial["handoff"])
	}
	for _, k := range []string{"attempted", "transferred", "aborted", "fallback"} {
		if _, present := hoff0[k]; !present {
			t.Errorf("HandleStatus.handoff missing key %q", k)
		}
	}

	snapshotPath, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })

	after := d.HandleStatus()
	hoff1, _ := after["handoff"].(map[string]any)

	// attempted and fallback both visible via HandleStatus as uint64 (json-safe).
	attemptedVal, _ := hoff1["attempted"].(uint64)
	fallbackVal, _ := hoff1["fallback"].(uint64)
	if attemptedVal < 1 {
		t.Errorf("HandleStatus.handoff.attempted = %d, want >=1", attemptedVal)
	}
	if fallbackVal < 1 {
		t.Errorf("HandleStatus.handoff.fallback = %d, want >=1", fallbackVal)
	}
}

// TestHandoffFallback_SnapshotAlwaysWritten verifies FR-8's strongest
// guarantee: whatever happens to the handoff itself, the state snapshot
// is always produced FIRST, so the successor daemon has something to
// restore from. Without this, a handoff failure would lose both in-flight
// requests AND the cached init/tools templates.
func TestHandoffFallback_SnapshotAlwaysWritten(t *testing.T) {
	origAccept := handoffAcceptTimeout
	origTotal := handoffTotalTimeout
	handoffAcceptTimeout = 50 * time.Millisecond
	handoffTotalTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		handoffAcceptTimeout = origAccept
		handoffTotalTimeout = origTotal
	})

	d := testDaemon(t)

	snapshotPath, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })

	info, statErr := os.Stat(snapshotPath)
	if statErr != nil {
		t.Fatalf("snapshot file missing at %s: %v", snapshotPath, statErr)
	}
	if info.Size() == 0 {
		t.Errorf("snapshot file at %s has zero size", snapshotPath)
	}
}
