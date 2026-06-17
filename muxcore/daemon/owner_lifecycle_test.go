package daemon

import (
	"errors"
	"sync/atomic"
	"testing"

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

func seedReconnectHistoryForOwner(t *testing.T, o *owner.Owner, token, ownerKey string) {
	t.Helper()
	sess := &owner.Session{ID: len(token)}
	o.SessionMgr().RegisterSession(sess, "")
	o.SessionMgr().PreRegisterForOwner(token, ownerKey, "/project/"+ownerKey, nil)
	if ok := o.SessionMgr().Bind(token, ownerKey, sess); !ok {
		t.Fatalf("Bind(%q) returned false", token)
	}
}
