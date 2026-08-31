package daemon

import (
	"sync/atomic"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
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

	d.rehydrateRetryCounter(era.EraLegacy, restoredSid, cmd, args, cwd)

	got := mustLoadCounter(t, d, base)
	if got != 2 {
		t.Fatalf("counter after rehydrate: got %d, want 2", got)
	}
}

func TestRehydrateRetryCounter_ModernDoesNotMutateLegacyCounter(t *testing.T) {
	d, _ := testDaemonWithLog(t)
	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)
	counter := &atomic.Int64{}
	counter.Store(4)
	d.forcedIsolatedRetryCounters.Store(base, counter)

	d.rehydrateRetryCounter(era.EraModern20260728, base+"-r9", cmd, args, cwd)

	if got := mustLoadCounter(t, d, base); got != 4 {
		t.Fatalf("modern rehydrate mutated legacy retry counter: got %d, want 4", got)
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
	d.rehydrateRetryCounter(era.EraLegacy, base+"-r1", cmd, args, cwd)
	d.rehydrateRetryCounter(era.EraLegacy, base+"-r3", cmd, args, cwd)
	// Then a smaller N must NOT regress the counter.
	d.rehydrateRetryCounter(era.EraLegacy, base+"-r2", cmd, args, cwd)

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
	d.rehydrateRetryCounter(era.EraLegacy, base, cmd, args, cwd)
	if _, ok := d.forcedIsolatedRetryCounters.Load(base); ok {
		t.Fatalf("counter map mutated for plain isolated sid %q", base)
	}

	// Global / cwd-keyed sid — should not match retry pattern.
	other := "globalcafe123"
	d.rehydrateRetryCounter(era.EraLegacy, other, cmd, args, cwd)
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

	d.rehydrateRetryCounter(era.EraLegacy, base+"-r2", cmd, args, cwd)

	// Exercise the production promotion helper so the legacy retry contract stays
	// coupled to its real mode and counter mutation path.
	req := &control.Request{Mode: "global"}
	entry := &OwnerEntry{ServerID: base + "-r2", Command: cmd, Args: args, Cwd: cwd, ProtocolEra: era.EraLegacy}
	next := d.promoteIsolatedRetry(req, entry)
	if next != 3 {
		t.Fatalf("post-rehydrate legacy retry: next counter = %d, want 3 (no collision with -r2)", next)
	}
	if req.Mode != "isolated" {
		t.Fatalf("legacy retry mode = %q, want isolated", req.Mode)
	}
}

func TestPromoteIsolatedRetry_RefusesModernOrNativeIdentity(t *testing.T) {
	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)

	tests := []struct {
		name       string
		requestEra string
		entryEra   era.ProtocolEra
		serverID   string
	}{
		{name: "modern request", requestEra: "2026-07-28", entryEra: era.EraLegacy, serverID: base + "-r2"},
		{name: "modern entry", entryEra: era.EraModern20260728, serverID: base + "-r2"},
		{name: "native identity", entryEra: era.EraLegacy, serverID: "native-retry-counter-refusal"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := testDaemonWithLog(t)
			counter := &atomic.Int64{}
			counter.Store(4)
			d.forcedIsolatedRetryCounters.Store(base, counter)
			req := &control.Request{Mode: "global", ProtocolEra: tc.requestEra}
			entry := &OwnerEntry{
				ServerID:    tc.serverID,
				Command:     cmd,
				Args:        args,
				Cwd:         cwd,
				ProtocolEra: tc.entryEra,
			}

			if got := d.promoteIsolatedRetry(req, entry); got != 0 {
				t.Fatalf("refused retry promotion returned %d, want 0", got)
			}
			if req.Mode != "global" {
				t.Fatalf("refused retry promotion mutated mode to %q, want global", req.Mode)
			}
			if got := mustLoadCounter(t, d, base); got != 4 {
				t.Fatalf("refused retry promotion mutated legacy counter to %d, want 4", got)
			}
		})
	}
}

func TestSpawnModernPreRegisterInitialFailureDoesNotSeedLegacyRetryCounter(t *testing.T) {
	d := testDaemon(t)
	d.sessionHandler = noopSessionHandler{}
	cwd := t.TempDir()
	cmd := "modern-preregister-failure"
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, nil, nil, cwd)
	var initialReservations atomic.Int64
	d.beforeColdOwnerPromotion = func(o *owner.Owner) {
		initialReservations.Add(1)
		if !o.PreRegisterInitial("already-reserved", cwd, nil) {
			t.Fatal("test hook could not reserve the initial admission token")
		}
	}

	_, _, _, err := d.Spawn(control.Request{
		Cmd:         "spawn",
		Command:     cmd,
		Cwd:         cwd,
		Mode:        "global",
		ProtocolEra: "2026-07-28",
	})
	if err == nil {
		t.Fatal("modern Spawn unexpectedly succeeded after initial admission collision")
	}
	if initialReservations.Load() == 0 {
		t.Fatal("modern Spawn did not exercise the initial admission failure path")
	}
	if _, exists := d.forcedIsolatedRetryCounters.Load(base); exists {
		t.Fatalf("modern initial admission failure seeded legacy retry counter %q", base)
	}
}

func TestDeleteOwnerEntryCleansRetryCounterWhenLastRetryOwnerGone(t *testing.T) {
	d, _ := testDaemonWithLog(t)
	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)

	d.forcedIsolatedRetryCounters.Store(base, &atomic.Int64{})
	mustLoadCounter(t, d, base)

	d.mu.Lock()
	d.owners[base+"-r2"] = &OwnerEntry{ServerID: base + "-r2", Command: cmd, Args: args, Cwd: cwd, ProtocolEra: era.EraLegacy}
	d.deleteOwnerEntryLocked(base + "-r2")
	d.mu.Unlock()

	if _, ok := d.forcedIsolatedRetryCounters.Load(base); ok {
		t.Fatalf("retry counter for %q survived after final retry owner removal", base)
	}
}

func TestDeleteOwnerEntryKeepsRetryCounterWhileSiblingRetryOwnerExists(t *testing.T) {
	d, _ := testDaemonWithLog(t)
	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)

	d.forcedIsolatedRetryCounters.Store(base, &atomic.Int64{})
	d.mu.Lock()
	d.owners[base+"-r1"] = &OwnerEntry{ServerID: base + "-r1", Command: cmd, Args: args, Cwd: cwd, ProtocolEra: era.EraLegacy}
	d.owners[base+"-r2"] = &OwnerEntry{ServerID: base + "-r2", Command: cmd, Args: args, Cwd: cwd, ProtocolEra: era.EraLegacy}
	d.deleteOwnerEntryLocked(base + "-r2")
	d.mu.Unlock()

	if _, ok := d.forcedIsolatedRetryCounters.Load(base); !ok {
		t.Fatalf("retry counter for %q was removed while sibling retry owner remained", base)
	}
}

func TestDeleteOwnerEntry_ModernTypedLegacyShapedEntryPreservesLegacyRetryCounter(t *testing.T) {
	d, _ := testDaemonWithLog(t)
	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)
	counter := &atomic.Int64{}
	counter.Store(6)
	d.forcedIsolatedRetryCounters.Store(base, counter)

	d.mu.Lock()
	d.owners[base+"-r2"] = &OwnerEntry{
		ServerID:    base + "-r2",
		Command:     cmd,
		Args:        args,
		Cwd:         cwd,
		ProtocolEra: era.EraModern20260728,
	}
	d.deleteOwnerEntryLocked(base + "-r2")
	d.mu.Unlock()

	if got := mustLoadCounter(t, d, base); got != 6 {
		t.Fatalf("modern legacy-shaped removal mutated legacy retry counter: got %d, want 6", got)
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
