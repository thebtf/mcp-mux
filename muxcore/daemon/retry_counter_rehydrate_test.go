package daemon

import (
	"sync/atomic"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// TestRehydrateRetryCounter is the regression test for codex PR #121 P1
// MAJOR finding: forcedIsolatedRetryCounters lives in memory only, so after
// a graceful daemon restart a previously-active isolated-...-rN owner gets
// restored from the snapshot while the counter map starts empty. Without
// rehydration, the next Spawn for the same (cmd,args,cwd) computes the
// base isolated-<hex16> sid (counter=0), misses the restored -rN owner,
// and creates a duplicate.
//
// rehydrateRetryCounter parses the -rN suffix on snapshot load and bumps
// the in-memory counter so future Spawns see N (or higher) and produce
// the same sid as the restored owner — or, if forced-isolated retry fires
// again, -r<N+1>, never colliding.
func TestRehydrateRetryCounter_RestoresFromSuffixedSid(t *testing.T) {
	d, _ := testDaemonWithLog(t)

	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)
	restoredSid := base + "-r2"

	d.rehydrateRetryCounter(restoredSid, cmd, args, cwd)

	got := mustLoadCounter(t, d, base)
	if got != 2 {
		t.Fatalf("counter after rehydrate: got %d, want 2", got)
	}
}

// TestRehydrateRetryCounter_TakesMaxAcrossMultipleEntries verifies the
// CAS-max semantic when a snapshot contains multiple retry-suffixed
// owners sharing the same base sid (e.g. two isolated -r1 and -r2 sids
// from different sessions of the same upstream). The counter must end
// up at the LARGEST N so the next retry produces -r<max+1>, not -r2
// (which would collide with an existing snapshot entry).
func TestRehydrateRetryCounter_TakesMaxAcrossMultipleEntries(t *testing.T) {
	d, _ := testDaemonWithLog(t)

	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)

	// Simulate snapshot replay order with smaller N first then larger.
	d.rehydrateRetryCounter(base+"-r1", cmd, args, cwd)
	d.rehydrateRetryCounter(base+"-r3", cmd, args, cwd)
	// Then a smaller N must NOT regress the counter.
	d.rehydrateRetryCounter(base+"-r2", cmd, args, cwd)

	got := mustLoadCounter(t, d, base)
	if got != 3 {
		t.Fatalf("counter after CAS-max sequence: got %d, want 3 (largest -rN)", got)
	}
}

// TestRehydrateRetryCounter_NonRetrySidNoop verifies plain isolated sids
// (no -rN suffix) and shared/global sids do not affect the counter map.
func TestRehydrateRetryCounter_NonRetrySidNoop(t *testing.T) {
	d, _ := testDaemonWithLog(t)
	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)

	// Plain isolated sid — no suffix.
	d.rehydrateRetryCounter(base, cmd, args, cwd)
	if _, ok := d.forcedIsolatedRetryCounters.Load(base); ok {
		t.Fatalf("counter map mutated for plain isolated sid %q", base)
	}

	// Global / cwd-keyed sid — should not match retry pattern.
	other := "globalcafe123"
	d.rehydrateRetryCounter(other, cmd, args, cwd)
	if _, ok := d.forcedIsolatedRetryCounters.Load(other); ok {
		t.Fatalf("counter map mutated for non-isolated sid %q", other)
	}
}

// TestRehydrateRetryCounter_BumpsBeyondRestoredOnNextRetry models the
// codex scenario end-to-end at the counter level: after a snapshot with
// -r2 is rehydrated, a subsequent forced-isolated retry MUST bump the
// counter to 3, producing -r3 and never colliding with the restored sid.
func TestRehydrateRetryCounter_BumpsBeyondRestoredOnNextRetry(t *testing.T) {
	d, _ := testDaemonWithLog(t)
	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)

	d.rehydrateRetryCounter(base+"-r2", cmd, args, cwd)

	// Simulate the production bump path (daemon.go: forcedIsolatedRetryCounters
	// LoadOrStore + Add(1)) — this is what the next forced-isolated retry would
	// execute against the rehydrated counter.
	ctrI, _ := d.forcedIsolatedRetryCounters.LoadOrStore(base, &atomic.Int64{})
	next := ctrI.(*atomic.Int64).Add(1)
	if next != 3 {
		t.Fatalf("post-rehydrate retry: next counter = %d, want 3 (no collision with -r2)", next)
	}
}

func mustLoadCounter(t *testing.T, d *Daemon, base string) int64 {
	t.Helper()
	ctrI, ok := d.forcedIsolatedRetryCounters.Load(base)
	if !ok {
		t.Fatalf("counter map missing entry for base %q", base)
	}
	return ctrI.(*atomic.Int64).Load()
}
