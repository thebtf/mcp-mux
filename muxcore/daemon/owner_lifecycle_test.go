package daemon

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/session"
)

func TestOwnerLifecycleRemovalCleansOwnedTicketsAndCounters(t *testing.T) {
	d := testDaemon(t)
	d.supervisor = nil
	sid := "owner-lifecycle-cleanup"
	o := testReconnectOwner(t, sid)

	o.SessionMgr().PreRegisterForOwner("pending-owned", sid, "/owned", nil)
	o.SessionMgr().PreRegister("pending-legacy", "/legacy", nil)
	seedReconnectHistoryForOwner(t, o, "bound-owned", sid)
	seedReconnectHistoryForOwner(t, o, "bound-other", "other-owner")

	d.mu.Lock()
	d.owners[sid] = &OwnerEntry{Owner: o, ServerID: sid, OwnerGeneration: "owner_gen_test", RestoreSource: "fresh"}
	d.mu.Unlock()

	result, err := d.removeOwner(sid, ownerRemovalReasonOperatorHard, false)
	if err != nil {
		t.Fatalf("removeOwner() error: %v", err)
	}
	if result.PendingTokensRemoved != 1 {
		t.Fatalf("PendingTokensRemoved = %d, want 1", result.PendingTokensRemoved)
	}
	if result.BoundHistoryRemoved != 1 {
		t.Fatalf("BoundHistoryRemoved = %d, want 1", result.BoundHistoryRemoved)
	}
	if o.SessionMgr().IsPreRegistered("pending-owned") {
		t.Fatal("owned pending token still pre-registered after owner removal")
	}
	if !o.SessionMgr().IsPreRegistered("pending-legacy") {
		t.Fatal("legacy owner-keyless pending token must remain TTL-only")
	}
	if _, err := o.SessionMgr().RegisterReconnect("bound-owned", func(string) bool { return true }); !errors.Is(err, session.ErrUnknownToken) {
		t.Fatalf("RegisterReconnect(bound-owned) error = %v, want session.ErrUnknownToken", err)
	}
	if _, err := o.SessionMgr().RegisterReconnect("bound-other", func(string) bool { return true }); err != nil {
		t.Fatalf("RegisterReconnect(bound-other) error = %v, want retained history", err)
	}

	status := d.HandleStatus()
	assertOwnerRemovalStatus(t, status, 1, "operator_hard", 1)
	ownerRemoval := status["owner_removal"].(map[string]any)
	if got := uint64Status(t, ownerRemoval, "pending_tokens_removed"); got != 1 {
		t.Fatalf("pending_tokens_removed = %d, want 1", got)
	}
	if got := uint64Status(t, ownerRemoval, "bound_history_removed"); got != 1 {
		t.Fatalf("bound_history_removed = %d, want 1", got)
	}
}

func TestCleanupDeadOwnerUsesOwnerRemovalPath(t *testing.T) {
	d := testDaemon(t)
	d.supervisor = nil
	sid := "owner-lifecycle-zombie-cleanup"
	o := testReconnectOwner(t, sid)

	o.SessionMgr().PreRegisterForOwner("pending-zombie", sid, "/owned", nil)
	seedReconnectHistoryForOwner(t, o, "bound-zombie", sid)

	d.mu.Lock()
	entry := &OwnerEntry{Owner: o, ServerID: sid, OwnerGeneration: "owner_gen_test", RestoreSource: "fresh"}
	d.owners[sid] = entry
	d.mu.Unlock()

	d.cleanupDeadOwner("owner[" + sid[:8] + " command]")

	d.mu.RLock()
	_, stillPresent := d.owners[sid]
	d.mu.RUnlock()
	if stillPresent {
		t.Fatal("cleanupDeadOwner left owner in registry")
	}
	if o.SessionMgr().IsPreRegistered("pending-zombie") {
		t.Fatal("owned pending token still pre-registered after zombie cleanup")
	}
	if _, err := o.SessionMgr().RegisterReconnect("bound-zombie", func(string) bool { return true }); !errors.Is(err, session.ErrUnknownToken) {
		t.Fatalf("RegisterReconnect(bound-zombie) error = %v, want session.ErrUnknownToken", err)
	}
	status := d.HandleStatus()
	assertOwnerRemovalStatus(t, status, 1, "zombie", 1)
}

func TestOwnerLifecycleRemovalCleansFinalRetryCounter(t *testing.T) {
	d := testDaemon(t)
	d.supervisor = nil

	cwd := t.TempDir()
	cmd := "echo"
	args := []string{"hello"}
	base := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)
	sid := base + "-r1"
	o := testReconnectOwner(t, sid)

	d.forcedIsolatedRetryCounters.Store(base, &atomic.Int64{})
	d.mu.Lock()
	d.owners[sid] = &OwnerEntry{
		Owner:           o,
		ServerID:        sid,
		Command:         cmd,
		Args:            args,
		Cwd:             cwd,
		OwnerGeneration: "owner_gen_retry",
		RestoreSource:   "fresh",
	}
	d.mu.Unlock()

	if _, err := d.removeOwner(sid, ownerRemovalReasonIdle, false); err != nil {
		t.Fatalf("removeOwner() error: %v", err)
	}
	if _, ok := d.forcedIsolatedRetryCounters.Load(base); ok {
		t.Fatalf("retry counter for %q survived final retry owner removal", base)
	}
}

func TestStopOwnerResultMessageTreatsPostRemovalErrorAsWarning(t *testing.T) {
	msg, err := stopOwnerResultMessage("stopped", ownerRemovalResult{Removed: true}, errors.New("supervisor wait timed out"))
	if err != nil {
		t.Fatalf("stopOwnerResultMessage() error = %v, want nil warning", err)
	}
	if !strings.Contains(msg, "stopped") || !strings.Contains(msg, "warning: supervisor wait timed out") {
		t.Fatalf("message = %q, want success with warning", msg)
	}

	_, err = stopOwnerResultMessage("stopped", ownerRemovalResult{Removed: false}, errors.New("server not found"))
	if err == nil {
		t.Fatal("expected pre-removal error to remain an error")
	}
}

func TestOwnerRemovalRetriesBeforeForgettingEntry(t *testing.T) {
	d := testDaemon(t)
	d.supervisor = nil
	sid := "owner-finalization-retry"
	o := testReconnectOwner(t, sid)
	entry := &OwnerEntry{Owner: o, ServerID: sid, OwnerGeneration: "owner_gen_retry"}
	o.SessionMgr().PreRegisterForOwner("pending-finalization", sid, "/owned", nil)
	d.mu.Lock()
	d.owners[sid] = entry
	d.mu.Unlock()

	original := finalizeOwnerForRemoval
	t.Cleanup(func() {
		finalizeOwnerForRemoval = original
		o.Shutdown()
	})
	var calls atomic.Int32
	finalizeOwnerForRemoval = func(got *owner.Owner, soft bool) (int, bool, error) {
		if got != o || soft {
			t.Fatalf("finalizer got owner=%p soft=%v, want owner=%p soft=false", got, soft, o)
		}
		d.mu.RLock()
		current := d.owners[sid]
		d.mu.RUnlock()
		if current != entry {
			t.Fatal("owner entry was forgotten before finalization proof")
		}
		if !o.SessionMgr().IsPreRegistered("pending-finalization") {
			t.Fatal("owner tokens were removed before finalization proof")
		}
		if calls.Add(1) == 1 {
			return 0, false, errors.New("retirement not yet proven")
		}
		return 0, true, nil
	}

	result, err := d.removeOwnerIfCurrent(sid, entry, ownerRemovalReasonOperatorHard, false)
	if err != nil {
		t.Fatalf("removeOwnerIfCurrent() error: %v", err)
	}
	if !result.Removed || calls.Load() != 2 {
		t.Fatalf("result=%+v finalizer_calls=%d, want removed after second proof", result, calls.Load())
	}
	if d.Entry(sid) != nil {
		t.Fatal("owner entry survived proven finalization")
	}
}

func TestReaperFinalizationRetryRetainsLivePIDAndOwnerState(t *testing.T) {
	d := testDaemon(t)
	generationFile := t.TempDir() + string(os.PathSeparator) + "reaper-generation.txt"
	command, args, env := daemonRespawnHelperCommand(generationFile)
	path, sid, token, err := d.Spawn(control.Request{Cmd: "spawn", Command: command, Args: args, Env: env, Mode: "global"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("spawned owner missing")
	}
	conn, _ := connectSpawnedOwner(t, path, token)
	waitForDaemonCondition(t, 3*time.Second, func() bool {
		return entry.Owner.SessionCount() == 1 && entry.Owner.MaterializationState() == owner.MaterializationReady
	}, "owner did not become ready while session was retained")
	_ = conn.Close()
	waitForDaemonCondition(t, 3*time.Second, func() bool {
		return entry.Owner.SessionCount() == 0 && entry.Owner.SessionMgr().PendingCount() == 0
	}, "owner did not become idle after session close")
	pid, _ := entry.Owner.Status()["upstream_pid"].(int)
	if !daemonTestProcessAlive(pid) {
		t.Fatalf("upstream pid %d is not live before reaper retry", pid)
	}
	if ownerKey, _, _, ok := entry.Owner.SessionMgr().LookupHistory(token); !ok || ownerKey != sid {
		t.Fatalf("reconnect history before reaper=(%q,%v), want owner %q", ownerKey, ok, sid)
	}

	d.mu.Lock()
	entry.LastSession = time.Now().Add(-time.Second)
	entry.IdleTimeout = time.Millisecond
	d.mu.Unlock()
	time.Sleep(20 * time.Millisecond)

	original := finalizeOwnerForRemoval
	t.Cleanup(func() { finalizeOwnerForRemoval = original })
	var calls atomic.Int32
	finalizeOwnerForRemoval = func(got *owner.Owner, soft bool) (int, bool, error) {
		call := calls.Add(1)
		if got != entry.Owner || !soft {
			t.Fatalf("finalizer call %d got owner=%p soft=%v, want owner=%p soft=true", call, got, soft, entry.Owner)
		}
		if call <= ownerFinalizationAttempts {
			if current := d.Entry(sid); current != entry {
				t.Fatalf("call %d forgot owner before process retirement proof", call)
			}
			if !daemonTestProcessAlive(pid) {
				t.Fatalf("call %d lost live pid %d before retryable finalization", call, pid)
			}
			if ownerKey, _, _, ok := entry.Owner.SessionMgr().LookupHistory(token); !ok || ownerKey != sid {
				t.Fatalf("call %d removed owner history before proof: owner=%q ok=%v", call, ownerKey, ok)
			}
			return 0, false, errors.New("synthetic whole-tree proof pending")
		}
		return original(got, soft)
	}

	if affected := (&Reaper{daemon: d, logger: d.logger}).sweep(); affected != 0 {
		t.Fatalf("first reaper sweep affected=%d, want retained owner while finalization is retryable", affected)
	}
	if got := calls.Load(); got != ownerFinalizationAttempts {
		t.Fatalf("synchronous finalizer calls=%d, want %d", got, ownerFinalizationAttempts)
	}
	if current := d.Entry(sid); current != entry {
		t.Fatal("reaper forgot owner after unproven finalization")
	}
	if !daemonTestProcessAlive(pid) {
		t.Fatalf("upstream pid %d died before scheduled finalization retry", pid)
	}

	waitForDaemonCondition(t, 5*time.Second, func() bool {
		return d.Entry(sid) == nil && !daemonTestProcessAlive(pid)
	}, "scheduled finalization retry did not retire process and remove owner")
	if calls.Load() < ownerFinalizationAttempts+1 {
		t.Fatalf("finalizer calls=%d, want scheduled retry after %d synchronous attempts", calls.Load(), ownerFinalizationAttempts)
	}
	if _, _, _, ok := entry.Owner.SessionMgr().LookupHistory(token); ok {
		t.Fatal("owner history survived proven final removal")
	}
}

func TestOwnerRemovalKeepsEntryWhenFinalizationUnproven(t *testing.T) {
	d := testDaemon(t)
	d.supervisor = nil
	sid := "owner-finalization-blocked"
	o := testReconnectOwner(t, sid)
	entry := &OwnerEntry{Owner: o, ServerID: sid, OwnerGeneration: "owner_gen_blocked"}
	d.mu.Lock()
	d.owners[sid] = entry
	d.mu.Unlock()

	original := finalizeOwnerForRemoval
	t.Cleanup(func() {
		finalizeOwnerForRemoval = original
		o.Shutdown()
	})
	var calls atomic.Int32
	finalizeOwnerForRemoval = func(*owner.Owner, bool) (int, bool, error) {
		calls.Add(1)
		return 0, false, errors.New("retirement unproven")
	}

	result, err := d.finalizeAndRemoveOwner(sid, entry, ownerRemovalReasonOperatorHard, false, nil, false)
	if err == nil || result.Removed {
		t.Fatalf("result=%+v err=%v, want blocked removal", result, err)
	}
	if calls.Load() != ownerFinalizationAttempts {
		t.Fatalf("finalizer calls=%d, want %d", calls.Load(), ownerFinalizationAttempts)
	}
	if d.Entry(sid) != entry {
		t.Fatal("owner entry was forgotten without finalization proof")
	}
	d.mu.RLock()
	removalInProgress := entry.removalInProgress
	d.mu.RUnlock()
	if removalInProgress {
		t.Fatal("blocked removal did not release retryable registry state")
	}
}

func seedReconnectHistoryForOwner(t *testing.T, o *owner.Owner, token, ownerKey string) {
	t.Helper()
	sess := &owner.Session{ID: len(token)}
	o.SessionMgr().RegisterSession(sess, "")
	o.SessionMgr().PreRegisterForOwner(token, ownerKey, "/project/"+ownerKey, nil)
	if ok := o.SessionMgr().Bind(token, ownerKey, sess); !ok {
		t.Fatalf("Bind(%q) returned false", token)
	}
}
