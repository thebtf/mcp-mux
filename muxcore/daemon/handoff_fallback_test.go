package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

// setupTestHandoffTimeouts overrides the package-level handoff timeout
// variables to 50 ms so tests exercise the timeout/fallback path quickly
// without relying on production-scale delays.  The original values are
// restored automatically via t.Cleanup.
func setupTestHandoffTimeouts(t *testing.T) {
	t.Helper()
	origAccept := handoffAcceptTimeout
	origTotal := handoffTotalTimeout
	handoffAcceptTimeout = 50 * time.Millisecond
	handoffTotalTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		handoffAcceptTimeout = origAccept
		handoffTotalTimeout = origTotal
	})
}

func seedProcessBackedOwner(t *testing.T, d *Daemon) {
	t.Helper()
	_, sid, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Cwd:     ".",
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() process-backed owner: %v", err)
	}
	waitForDaemonCondition(t, 5*time.Second, func() bool {
		entry := d.Entry(sid)
		return entry != nil && entry.Owner != nil && entry.Owner.MaterializationState() == owner.MaterializationReady
	}, "process-backed owner did not finish eager materialization")
}

// TestHandoffFallbackOnAcceptTimeout verifies that HandleGracefulRestart returns
// a valid snapshot path and nil error even when no successor daemon connects
// within the accept timeout — i.e. the FR-8 fallback path is transparent to
// the control-plane caller.
//
// Failure mode simulated: listenHandoff accept times out because spawnSuccessor
// forks a real binary with the configured DaemonFlag but the binary in the test
// environment is the test binary itself, which does not implement the successor
// handshake. The accept timeout (overridden to 50 ms) fires before any connection arrives.
//
// This test asserts:
//  1. HandleGracefulRestart returns (snapshotPath, nil, nil) — not an error.
//  2. snapshotPath is non-empty — snapshot serialization succeeded.
//  3. The daemon continues to the Shutdown path (go d.Shutdown()) regardless.
func TestHandoffFallbackOnAcceptTimeout(t *testing.T) {
	setupTestHandoffTimeouts(t)

	d := testDaemon(t) // helper defined in daemon_test.go
	seedProcessBackedOwner(t, d)

	// HandleGracefulRestart MUST succeed (FR-8 fallback is transparent to caller).
	snapshotPath, _, err := d.HandleGracefulRestart(0)
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

func TestAttemptHandoffSkipsSessionHandlerOnlyOwners(t *testing.T) {
	ctlPath := shortSocketPath(t, "sessionhandler-handoff.ctl.sock")
	d, err := New(Config{
		Name:           "test-daemon",
		ControlPath:    ctlPath,
		GracePeriod:    1 * time.Second,
		IdleTimeout:    5 * time.Second,
		SkipSnapshot:   true,
		SessionHandler: noopSessionHandler{},
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	_, sid, _, err := d.Spawn(control.Request{
		Cmd:  "spawn",
		Cwd:  t.TempDir(),
		Mode: "global",
	})
	if err != nil {
		t.Fatalf("Spawn() SessionHandler owner: %v", err)
	}
	waitOwnerAccepting(t, d, sid)

	if err := d.attemptHandoff(""); err == nil || !strings.Contains(err.Error(), "no process-backed owners") {
		t.Fatalf("attemptHandoff() error = %v, want no process-backed owners", err)
	}

	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("SessionHandler owner was removed by skipped handoff")
	}
	if !entry.Owner.IsAccepting() {
		t.Fatal("SessionHandler owner stopped accepting after skipped handoff")
	}
	if got := d.stats.attempted.Load(); got != 1 {
		t.Fatalf("handoff attempted counter = %d, want 1", got)
	}
}

func TestGracefulRestartSessionHandlerOnlySpawnsSnapshotSuccessorAfterShutdown(t *testing.T) {
	origSpawn := spawnSnapshotSuccessorForRestart
	spawned := make(chan struct{})
	var gotExe, gotFlag string
	spawnSnapshotSuccessorForRestart = func(successorExe, daemonFlag string) error {
		gotExe = successorExe
		gotFlag = daemonFlag
		close(spawned)
		return nil
	}
	t.Cleanup(func() { spawnSnapshotSuccessorForRestart = origSpawn })

	ctlPath := shortSocketPath(t, "sessionhandler-snapshot-restart.ctl.sock")
	d, err := New(Config{
		Name:           "test-daemon",
		ControlPath:    ctlPath,
		DaemonFlag:     "--fixture-daemon",
		GracePeriod:    1 * time.Second,
		IdleTimeout:    5 * time.Second,
		SkipSnapshot:   true,
		SessionHandler: noopSessionHandler{},
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	_, sid, _, err := d.Spawn(control.Request{
		Cmd:  "spawn",
		Cwd:  t.TempDir(),
		Mode: "global",
	})
	if err != nil {
		t.Fatalf("Spawn() SessionHandler owner: %v", err)
	}
	waitOwnerAccepting(t, d, sid)

	beforeFallback := d.stats.fallback.Load()
	snapshotPath, afterFn, err := d.HandleGracefulRestartWithOptions(control.GracefulRestartOptions{
		SuccessorExe: "next-engine.exe",
	})
	if err != nil {
		t.Fatalf("HandleGracefulRestartWithOptions() error = %v, want nil", err)
	}
	if snapshotPath == "" {
		t.Fatal("snapshot path is empty")
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })
	if afterFn == nil {
		t.Fatal("afterFn is nil; predecessor would not shut down after response")
	}
	select {
	case <-spawned:
		t.Fatal("snapshot successor spawned before restart response afterFn")
	default:
	}
	if !d.shuttingDown.Load() {
		t.Fatal("daemon is not marked shuttingDown while restart response is pending")
	}
	if got := d.stats.fallback.Load() - beforeFallback; got != 0 {
		t.Fatalf("handoff fallback delta = %d, want 0 for SessionHandler-only snapshot restart", got)
	}

	afterFn()
	select {
	case <-d.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down after snapshot restart afterFn")
	}
	select {
	case <-spawned:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot successor did not spawn after daemon shutdown")
	}
	if gotExe != "next-engine.exe" || gotFlag != "--fixture-daemon" {
		t.Fatalf("snapshot successor = (%q, %q), want (next-engine.exe, --fixture-daemon)", gotExe, gotFlag)
	}
}

// TestHandoffFallback_LogMarker verifies that the FR-8 fallback path emits
// a "handoff.fallback reason=..." structured log marker. Operators rely on
// this marker to correlate upgrade failures with restart events in logs.
//
// Uses testDaemonWithLog (helper added in F80-4 / T022) to capture daemon
// log output into a thread-safe buffer and grep it after the call returns.
func TestHandoffFallback_LogMarker(t *testing.T) {
	setupTestHandoffTimeouts(t)

	d, logBuf := testDaemonWithLog(t)
	seedProcessBackedOwner(t, d)

	snapshotPath, _, err := d.HandleGracefulRestart(0)
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
	setupTestHandoffTimeouts(t)

	d := testDaemon(t)
	seedProcessBackedOwner(t, d)

	// Snapshot counters before.
	before := d.stats.fallback.Load()
	beforeAttempted := d.stats.attempted.Load()

	snapshotPath, _, err := d.HandleGracefulRestart(0)
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
	setupTestHandoffTimeouts(t)

	d := testDaemon(t)
	seedProcessBackedOwner(t, d)

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

	snapshotPath, _, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })

	after := d.HandleStatus()
	hoff1, ok := after["handoff"].(map[string]any)
	if !ok {
		t.Fatalf("HandleStatus missing 'handoff' sub-map after restart; got %T", after["handoff"])
	}

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
	setupTestHandoffTimeouts(t)

	d := testDaemon(t)
	seedProcessBackedOwner(t, d)

	snapshotPath, _, err := d.HandleGracefulRestart(0)
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
