package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

type fakeHandoffSuccessor struct {
	done chan struct{}
	once sync.Once
}

func newFakeHandoffSuccessor() *fakeHandoffSuccessor {
	return &fakeHandoffSuccessor{done: make(chan struct{})}
}

func (s *fakeHandoffSuccessor) Done() <-chan struct{} { return s.done }
func (s *fakeHandoffSuccessor) Stop() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func setupVersionSkewSuccessor(t *testing.T) {
	t.Helper()
	origSpawn := spawnHandoffSuccessorForRestart
	spawnHandoffSuccessorForRestart = func(tokenPath, socketPath, _, _ string) (handoffSuccessor, error) {
		token, err := readHandoffToken(tokenPath)
		if err != nil {
			return nil, err
		}
		conn, err := dialHandoff(socketPath, time.Second)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		if err := conn.WriteJSON(HelloMsg{
			Type:            MsgHello,
			ProtocolVersion: HandoffProtocolVersion - 1,
			Token:           token,
		}); err != nil {
			return nil, err
		}
		return newFakeHandoffSuccessor(), nil
	}
	t.Cleanup(func() { spawnHandoffSuccessorForRestart = origSpawn })
}

func setupPostHelloFailureSuccessors(t *testing.T) *int {
	t.Helper()
	origHandoffSpawn := spawnHandoffSuccessorForRestart
	origSnapshotSpawn := spawnSnapshotSuccessorForRestart
	spawnedFallbacks := new(int)
	spawnHandoffSuccessorForRestart = func(tokenPath, socketPath, _, _ string) (handoffSuccessor, error) {
		token, err := readHandoffToken(tokenPath)
		if err != nil {
			return nil, err
		}
		conn, err := dialHandoff(socketPath, time.Second)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		if err := conn.WriteJSON(NewHelloMsgWithPID(token, os.Getpid())); err != nil {
			return nil, err
		}
		return newFakeHandoffSuccessor(), nil
	}
	spawnSnapshotSuccessorForRestart = func(string, string) error {
		*spawnedFallbacks++
		return nil
	}
	t.Cleanup(func() {
		spawnHandoffSuccessorForRestart = origHandoffSpawn
		spawnSnapshotSuccessorForRestart = origSnapshotSpawn
	})
	return spawnedFallbacks
}

func TestRetryControlBindStaysPausedUntilRestartStagingActivation(t *testing.T) {
	d := &Daemon{logger: testLogger(t)}
	path := shortSocketPath(t, "paused-restart-control.ctl.sock")
	if err := d.retryControlBind(path); err != nil {
		t.Fatalf("retryControlBind: %v", err)
	}
	defer d.ctlSrv.Close()

	result := make(chan error, 1)
	go func() {
		resp, err := control.SendWithTimeout(path, control.Request{Cmd: "ping"}, 2*time.Second)
		if err == nil && !resp.OK {
			err = os.ErrInvalid
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("restart control dispatched before staging activation: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := d.activateRestartStaging(); err != nil {
		t.Fatalf("activateRestartStaging() error: %v", err)
	}
	d.ctlSrv.Start()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ping after staging activation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart control did not dispatch after staging activation")
	}
}

func registerGracefulRestartCleanup(t *testing.T, d *Daemon, afterFn func()) {
	t.Helper()
	if afterFn == nil {
		t.Fatal("graceful restart afterFn is nil")
	}
	t.Cleanup(func() {
		afterFn()
		select {
		case <-d.Done():
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down after graceful restart afterFn")
		}
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

// TestHandoffAcceptTimeoutKeepsPredecessorServing verifies that a successor
// which never reaches Hello cannot authorize predecessor teardown.
func TestHandoffAcceptTimeoutKeepsPredecessorServing(t *testing.T) {
	setupTestHandoffTimeouts(t)
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })

	d := testDaemon(t)
	seedProcessBackedOwner(t, d)
	d.mu.RLock()
	var entry *OwnerEntry
	for _, candidate := range d.owners {
		entry = candidate
		break
	}
	d.mu.RUnlock()

	snapshotPath, afterFn, err := d.HandleGracefulRestart(0)
	if err == nil || !strings.Contains(err.Error(), "accept") {
		t.Fatalf("HandleGracefulRestart() error = %v, want accept failure", err)
	}
	if snapshotPath != "" || afterFn != nil {
		t.Fatalf("aborted restart returned path=%q afterFn=%v", snapshotPath, afterFn != nil)
	}
	if d.shuttingDown.Load() {
		t.Fatal("daemon remained in shutting-down state after aborted restart")
	}
	if entry == nil || entry.Owner == nil || !entry.Owner.IsAccepting() {
		t.Fatal("accept failure stopped the predecessor owner")
	}
	if got := entry.Owner.Status()["restart_pin_count"]; got != int64(0) {
		t.Fatalf("restart pin count = %v, want 0", got)
	}
	if entry.terminationHint != HintNone {
		t.Fatalf("termination hint = %v, want none", entry.terminationHint)
	}
	if _, statErr := os.Stat(SnapshotPath()); !os.IsNotExist(statErr) {
		t.Fatalf("aborted snapshot still exists: %v", statErr)
	}
}

func TestProtocolVersionMismatchKeepsPredecessorServing(t *testing.T) {
	setupTestHandoffTimeouts(t)
	setupVersionSkewSuccessor(t)
	origSnapshotSpawn := spawnSnapshotSuccessorForRestart
	fallbackSpawns := 0
	spawnSnapshotSuccessorForRestart = func(string, string) error {
		fallbackSpawns++
		return nil
	}
	t.Cleanup(func() { spawnSnapshotSuccessorForRestart = origSnapshotSpawn })
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })

	d, logs := testDaemonWithLog(t)
	seedProcessBackedOwner(t, d)
	d.mu.RLock()
	var entry *OwnerEntry
	for _, candidate := range d.owners {
		entry = candidate
		break
	}
	d.mu.RUnlock()
	beforeFallback := d.stats.fallback.Load()

	snapshotPath, afterFn, err := d.HandleGracefulRestart(0)
	if !errors.Is(err, ErrProtocolVersionMismatch) {
		t.Fatalf("HandleGracefulRestart() error = %v, want ErrProtocolVersionMismatch", err)
	}
	if snapshotPath != "" || afterFn != nil {
		t.Fatalf("version mismatch returned path=%q afterFn=%v", snapshotPath, afterFn != nil)
	}
	if fallbackSpawns != 0 || d.stats.fallback.Load() != beforeFallback {
		t.Fatalf("version mismatch started fallback=%d or advanced counter", fallbackSpawns)
	}
	if d.shuttingDown.Load() || entry == nil || entry.Owner == nil || !entry.Owner.IsAccepting() {
		t.Fatal("version mismatch did not preserve the predecessor")
	}
	if got := entry.Owner.Status()["restart_pin_count"]; got != int64(0) {
		t.Fatalf("restart pin count = %v, want 0", got)
	}
	if entry.terminationHint != HintNone {
		t.Fatalf("termination hint = %v, want none", entry.terminationHint)
	}
	if _, statErr := os.Stat(SnapshotPath()); !os.IsNotExist(statErr) {
		t.Fatalf("aborted snapshot still exists: %v", statErr)
	}
	if !strings.Contains(logs.String(), "handoff.abort") || strings.Contains(logs.String(), "handoff.fallback") {
		t.Fatalf("version mismatch logs did not record abort-only behavior:\n%s", logs.String())
	}
}

func TestAttemptHandoffSuccessorStartFailureLeavesPredecessorServing(t *testing.T) {
	setupTestHandoffTimeouts(t)
	d := testDaemon(t)
	seedProcessBackedOwner(t, d)

	d.mu.RLock()
	var entry *OwnerEntry
	for _, candidate := range d.owners {
		entry = candidate
		break
	}
	d.mu.RUnlock()
	if entry == nil || entry.Owner == nil {
		t.Fatal("process-backed predecessor owner missing")
	}
	beforePID := entry.Owner.Status()["upstream_pid"]
	missingSuccessor := filepath.Join(t.TempDir(), "missing-successor.exe")
	if err := d.attemptHandoff(missingSuccessor); err == nil || !strings.Contains(err.Error(), "spawn successor") {
		t.Fatalf("attemptHandoff error = %v, want successor start failure", err)
	}
	if d.Entry(entry.ServerID) != entry {
		t.Fatal("successor start failure replaced or removed predecessor owner")
	}
	if !entry.Owner.IsAccepting() {
		t.Fatal("successor start failure stopped predecessor listener")
	}
	if afterPID := entry.Owner.Status()["upstream_pid"]; afterPID != beforePID {
		t.Fatalf("successor start failure changed predecessor pid from %v to %v", beforePID, afterPID)
	}
}

func TestGracefulRestartSuccessorStartFailureLeavesPredecessorServing(t *testing.T) {
	d := testDaemon(t)
	seedProcessBackedOwner(t, d)
	d.mu.RLock()
	var entry *OwnerEntry
	for _, candidate := range d.owners {
		entry = candidate
		break
	}
	d.mu.RUnlock()

	missingSuccessor := filepath.Join(t.TempDir(), "missing-successor.exe")
	snapshotPath, afterFn, err := d.HandleGracefulRestartWithOptions(control.GracefulRestartOptions{SuccessorExe: missingSuccessor})
	if err == nil || !strings.Contains(err.Error(), "spawn successor") {
		t.Fatalf("HandleGracefulRestartWithOptions() error = %v, want successor start failure", err)
	}
	if snapshotPath != "" || afterFn != nil {
		t.Fatalf("aborted restart returned path=%q afterFn=%v", snapshotPath, afterFn != nil)
	}
	if d.shuttingDown.Load() || entry == nil || entry.Owner == nil || !entry.Owner.IsAccepting() {
		t.Fatal("successor start failure did not preserve the predecessor")
	}
	if got := entry.Owner.Status()["restart_pin_count"]; got != int64(0) {
		t.Fatalf("restart pin count = %v, want 0", got)
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

func TestGracefulRestartSessionHandlerOnlySpawnsSnapshotSuccessorBeforeShutdown(t *testing.T) {
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

	_, sid, _, err := d.Spawn(control.Request{Cmd: "spawn", Cwd: t.TempDir(), Mode: "global"})
	if err != nil {
		t.Fatalf("Spawn() SessionHandler owner: %v", err)
	}
	waitOwnerAccepting(t, d, sid)

	beforeFallback := d.stats.fallback.Load()
	snapshotPath, afterFn, err := d.HandleGracefulRestartWithOptions(control.GracefulRestartOptions{SuccessorExe: "next-engine.exe"})
	if err != nil {
		t.Fatalf("HandleGracefulRestartWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })
	select {
	case <-spawned:
	default:
		t.Fatal("snapshot successor was not started before restart returned")
	}
	if afterFn == nil || !d.shuttingDown.Load() {
		t.Fatal("successful snapshot restart did not arm predecessor shutdown")
	}
	if got := d.stats.fallback.Load() - beforeFallback; got != 0 {
		t.Fatalf("handoff fallback delta = %d, want 0", got)
	}
	if gotExe != "next-engine.exe" || gotFlag != "--fixture-daemon" {
		t.Fatalf("snapshot successor = (%q, %q), want (next-engine.exe, --fixture-daemon)", gotExe, gotFlag)
	}

	afterFn()
	select {
	case <-d.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down after snapshot restart afterFn")
	}
}

func TestGracefulRestartSessionHandlerSuccessorStartFailurePreservesPredecessor(t *testing.T) {
	origSpawn := spawnSnapshotSuccessorForRestart
	spawnSnapshotSuccessorForRestart = func(string, string) error { return os.ErrNotExist }
	t.Cleanup(func() { spawnSnapshotSuccessorForRestart = origSpawn })
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })

	ctlPath := shortSocketPath(t, "sessionhandler-snapshot-failure.ctl.sock")
	d, err := New(Config{
		Name:           "test-daemon",
		ControlPath:    ctlPath,
		SkipSnapshot:   true,
		SessionHandler: noopSessionHandler{},
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	_, sid, _, err := d.Spawn(control.Request{Cmd: "spawn", Cwd: t.TempDir(), Mode: "global"})
	if err != nil {
		t.Fatalf("Spawn() SessionHandler owner: %v", err)
	}
	waitOwnerAccepting(t, d, sid)

	snapshotPath, afterFn, err := d.HandleGracefulRestartWithOptions(control.GracefulRestartOptions{SuccessorExe: "missing.exe"})
	if err == nil {
		t.Fatal("snapshot successor start failure returned nil error")
	}
	if snapshotPath != "" || afterFn != nil || d.shuttingDown.Load() {
		t.Fatalf("aborted snapshot restart path=%q afterFn=%v shuttingDown=%v", snapshotPath, afterFn != nil, d.shuttingDown.Load())
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil || !entry.Owner.IsAccepting() {
		t.Fatal("snapshot successor start failure stopped predecessor owner")
	}
	if got := entry.Owner.Status()["restart_pin_count"]; got != int64(0) {
		t.Fatalf("restart pin count = %v, want 0", got)
	}
	if _, statErr := os.Stat(SnapshotPath()); !os.IsNotExist(statErr) {
		t.Fatalf("aborted snapshot still exists: %v", statErr)
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
	spawnedFallbacks := setupPostHelloFailureSuccessors(t)

	d, logBuf := testDaemonWithLog(t)
	seedProcessBackedOwner(t, d)

	snapshotPath, afterFn, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })
	registerGracefulRestartCleanup(t, d, afterFn)
	if *spawnedFallbacks != 1 {
		t.Fatalf("snapshot fallback successor starts = %d, want exactly 1", *spawnedFallbacks)
	}

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
// atomic counter advances by exactly 1 after an exact-Hello protocol failure,
// enabling post-deploy scripts to distinguish live handoff from bounded
// snapshot fallback.
func TestHandoffFallback_CounterIncrement(t *testing.T) {
	setupTestHandoffTimeouts(t)
	spawnedFallbacks := setupPostHelloFailureSuccessors(t)

	d := testDaemon(t)
	seedProcessBackedOwner(t, d)

	// Snapshot counters before.
	before := d.stats.fallback.Load()
	beforeAttempted := d.stats.attempted.Load()

	snapshotPath, afterFn, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })
	registerGracefulRestartCleanup(t, d, afterFn)
	if *spawnedFallbacks != 1 {
		t.Fatalf("snapshot fallback successor starts = %d, want exactly 1", *spawnedFallbacks)
	}

	afterAttempted := d.stats.attempted.Load()
	after := d.stats.fallback.Load()

	// attempted MUST increase by exactly 1 (every graceful restart enters
	// the handoff path regardless of whether it completes).
	if afterAttempted-beforeAttempted != 1 {
		t.Errorf("handoff_attempted delta: got %d, want 1",
			afterAttempted-beforeAttempted)
	}

	// fallback MUST increase by exactly 1 only after exact Hello and detach.
	if after-before != 1 {
		t.Errorf("handoff_fallback delta: got %d, want 1 for post-Hello failure", after-before)
	}

	// transferred + aborted remain zero because no owner transfer began.
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
	spawnedFallbacks := setupPostHelloFailureSuccessors(t)

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

	snapshotPath, afterFn, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })
	registerGracefulRestartCleanup(t, d, afterFn)
	if *spawnedFallbacks != 1 {
		t.Fatalf("snapshot fallback successor starts = %d, want exactly 1", *spawnedFallbacks)
	}

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
	spawnedFallbacks := setupPostHelloFailureSuccessors(t)

	d := testDaemon(t)
	seedProcessBackedOwner(t, d)

	snapshotPath, afterFn, err := d.HandleGracefulRestart(0)
	if err != nil {
		t.Fatalf("HandleGracefulRestart: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(snapshotPath) })
	registerGracefulRestartCleanup(t, d, afterFn)
	if *spawnedFallbacks != 1 {
		t.Fatalf("snapshot fallback successor starts = %d, want exactly 1", *spawnedFallbacks)
	}

	info, statErr := os.Stat(snapshotPath)
	if statErr != nil {
		t.Fatalf("snapshot file missing at %s: %v", snapshotPath, statErr)
	}
	if info.Size() == 0 {
		t.Errorf("snapshot file at %s has zero size", snapshotPath)
	}
}
