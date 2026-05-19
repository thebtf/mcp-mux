package daemon

import (
	"io"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func TestReconnectRefreshPreservesOwner(t *testing.T) {
	d := testDaemon(t)
	sid := "owner-integration-refresh"
	o := testReconnectOwner(t, sid)

	initialReader, initialWriter := io.Pipe()
	defer initialWriter.Close() // safety net for early returns; explicit Close at line 36 handles the normal path
	initial := owner.NewSession(initialReader, io.Discard)
	initial.ID = 1
	o.SessionMgr().RegisterSession(initial, "")
	o.SessionMgr().PreRegister("prev-token", "/project/reconnect", map[string]string{"TOKEN": "live"})
	if ok := o.SessionMgr().Bind("prev-token", sid, initial); !ok {
		t.Fatal("initial Bind returned false")
	}
	if o.SessionMgr().IsPreRegistered("prev-token") {
		t.Fatal("prev-token is still pending after initial Bind; reconnect test requires a consumed token")
	}
	if ownerKey, _, _, ok := o.SessionMgr().LookupHistory("prev-token"); !ok || ownerKey != sid {
		t.Fatalf("LookupHistory(prev-token) = (%q, ok=%v), want owner %q history", ownerKey, ok, sid)
	}

	d.mu.Lock()
	d.owners[sid] = &OwnerEntry{
		Owner:       o,
		ServerID:    sid,
		LastSession: time.Now(),
		IdleTimeout: time.Minute,
	}
	d.mu.Unlock()
	waitOwnerAccepting(t, d, sid)
	if !d.ownerIsAccepting(sid) {
		t.Fatal("owner is not accepting before refresh")
	}

	initial.Close()
	select {
	case <-initial.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("initial session did not close in time")
	}

	newToken, err := d.HandleRefreshSessionToken("prev-token")
	if err != nil {
		t.Fatalf("HandleRefreshSessionToken() error = %v", err)
	}
	if newToken == "" || newToken == "prev-token" {
		t.Fatalf("newToken = %q, want distinct non-empty token", newToken)
	}
	if !o.SessionMgr().IsPreRegistered(newToken) {
		t.Fatal("refreshed token is not pending for reconnect Bind")
	}

	reconnectedReader, reconnectedWriter := io.Pipe()
	defer reconnectedWriter.Close()
	reconnected := owner.NewSession(reconnectedReader, io.Discard)
	reconnected.ID = 2
	o.SessionMgr().RegisterSession(reconnected, "")
	if ok := o.SessionMgr().Bind(newToken, sid, reconnected); !ok {
		t.Fatal("reconnect Bind returned false")
	}
	if reconnected.Cwd != "/project/reconnect" {
		t.Fatalf("reconnected.Cwd = %q, want %q", reconnected.Cwd, "/project/reconnect")
	}
	if reconnected.Env["TOKEN"] != "live" {
		t.Fatalf("reconnected.Env[TOKEN] = %q, want %q", reconnected.Env["TOKEN"], "live")
	}

	reaper := NewReaper(d, time.Hour)
	t.Cleanup(func() { reaper.Stop() })
	if affected := reaper.sweep(); affected != 0 {
		t.Fatalf("reaper.sweep() = %d, want 0 while owner is still alive", affected)
	}

	d.mu.RLock()
	entry := d.owners[sid]
	d.mu.RUnlock()
	if entry == nil || entry.Owner != o {
		t.Fatal("owner entry missing after reconnect refresh")
	}
	if !d.ownerIsAccepting(sid) {
		t.Fatal("owner is not accepting after reconnect refresh")
	}

	status := d.HandleStatus()
	if got := status["shim_reconnect_refreshed"]; got != uint64(1) {
		t.Fatalf("shim_reconnect_refreshed = %v, want 1", got)
	}
	if got := status["shim_reconnect_fallback_spawned"]; got != uint64(0) {
		t.Fatalf("shim_reconnect_fallback_spawned = %v, want 0", got)
	}
	if got := status["shim_reconnect_gave_up"]; got != uint64(0) {
		t.Fatalf("shim_reconnect_gave_up = %v, want 0", got)
	}
}
