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
	defer initialWriter.Close()
	initial := owner.NewSession(initialReader, io.Discard)
	initial.ID = 1
	o.SessionMgr().RegisterSession(initial, "")
	o.SessionMgr().PreRegister("prev-token", "/project/reconnect", map[string]string{"TOKEN": "live"})
	if ok := o.SessionMgr().Bind("prev-token", sid, initial); !ok {
		t.Fatal("initial Bind returned false")
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

	status := d.HandleStatus()
	if got := status["shim_reconnect_refreshed"]; got != uint64(1) {
		t.Fatalf("shim_reconnect_refreshed = %v, want 1", got)
	}
	if got := status["shim_reconnect_gave_up"]; got != uint64(0) {
		t.Fatalf("shim_reconnect_gave_up = %v, want 0", got)
	}
}
