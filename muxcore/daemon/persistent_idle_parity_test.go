package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/session"
)

const lifecycleOwnerIdleTimeout = 25 * time.Millisecond

func testLifecycleDaemon(t *testing.T, persistent bool) (*Daemon, *Reaper) {
	t.Helper()
	ctlPath := shortSocketPath(t, "lifecycle.ctl.sock")
	d, err := New(Config{
		Name:             "test-daemon",
		ControlPath:      ctlPath,
		IdleTimeout:      5 * time.Second,
		OwnerIdleTimeout: lifecycleOwnerIdleTimeout,
		SkipSnapshot:     true,
		Logger:           testLogger(t),
		Persistent:       persistent,
		SessionHandler:   noopSessionHandler{},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	return d, &Reaper{daemon: d, logger: d.logger}
}

func spawnLifecycleOwner(t *testing.T, d *Daemon, label string) (ipcPath, sid, token string) {
	t.Helper()
	ipcPath, sid, token, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "session-handler",
		Args:    []string{label},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn(%s) error: %v", label, err)
	}
	if ipcPath == "" || sid == "" || token == "" {
		t.Fatalf("Spawn(%s) returned ipcPath=%q sid=%q token=%q", label, ipcPath, sid, token)
	}
	return ipcPath, sid, token
}

func waitPastLifecycleIdle(t *testing.T) {
	t.Helper()
	time.Sleep(lifecycleOwnerIdleTimeout + 20*time.Millisecond)
}

func dialLifecycleSession(t *testing.T, ipcPath, token string) net.Conn {
	t.Helper()
	var lastErr error
	for i := 0; i < 50; i++ {
		conn, err := ipc.Dial(ipcPath)
		if err != nil {
			lastErr = err
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if _, err := fmt.Fprintf(conn, "%s\n", token); err != nil {
			lastErr = err
			conn.Close()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return conn
	}
	t.Fatalf("dial lifecycle owner %s: %v", ipcPath, lastErr)
	return nil
}

func waitOwnerSessionCount(t *testing.T, entry *OwnerEntry, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if entry.Owner.SessionCount() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owner %s session count = %d, want %d", entry.ServerID, entry.Owner.SessionCount(), want)
}

func TestPersistentIdleParityEvictsOnlyDisposableIdleOwners(t *testing.T) {
	d, r := testLifecycleDaemon(t, false)

	_, idleSID, _ := spawnLifecycleOwner(t, d, "idle-disposable")
	waitPastLifecycleIdle(t)
	if affected := r.sweep(); affected != 0 {
		t.Fatalf("pending reservation sweep affected %d owners, want 0", affected)
	}
	idleEntry := d.Entry(idleSID)
	if idleEntry == nil {
		t.Fatal("pending reservation did not protect idle owner")
	}
	if removed := idleEntry.Owner.SessionMgr().RemovePendingForOwner(idleSID); removed != 1 {
		t.Fatalf("RemovePendingForOwner() = %d, want 1", removed)
	}
	if affected := r.sweep(); affected != 1 {
		t.Fatalf("idle disposable sweep after reservation removal affected %d owners, want 1", affected)
	}
	if entry := d.Entry(idleSID); entry != nil {
		t.Fatalf("idle disposable owner still present after sweep: %#v", entry)
	}
	assertOwnerRemovalStatus(t, d.HandleStatus(), 1, "idle", 1)

	activeIPC, activeSID, activeToken := spawnLifecycleOwner(t, d, "active-disposable")
	activeEntry := d.Entry(activeSID)
	if activeEntry == nil || activeEntry.Owner == nil {
		t.Fatal("active owner entry missing after spawn")
	}
	conn := dialLifecycleSession(t, activeIPC, activeToken)
	waitOwnerSessionCount(t, activeEntry, 1)
	waitPastLifecycleIdle(t)
	if affected := r.sweep(); affected != 0 {
		t.Fatalf("active owner sweep affected %d owners, want 0", affected)
	}
	if entry := d.Entry(activeSID); entry == nil {
		t.Fatal("active owner was reaped while a session was connected")
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("active conn close: %v", err)
	}
	waitOwnerSessionCount(t, activeEntry, 0)
	waitPastLifecycleIdle(t)
	if affected := r.sweep(); affected != 1 {
		t.Fatalf("post-close active owner sweep affected %d owners, want 1", affected)
	}
	if entry := d.Entry(activeSID); entry != nil {
		t.Fatalf("post-close owner still present after sweep: %#v", entry)
	}
	assertOwnerRemovalStatus(t, d.HandleStatus(), 2, "idle", 2)

	_, persistentSID, _ := spawnLifecycleOwner(t, d, "persistent-idle")
	d.SetPersistent(persistentSID, true)
	waitPastLifecycleIdle(t)
	if affected := r.sweep(); affected != 0 {
		t.Fatalf("persistent idle sweep affected %d owners, want 0", affected)
	}
	if entry := d.Entry(persistentSID); entry == nil || !entry.Persistent {
		t.Fatalf("persistent owner missing or lost persistence after sweep: %#v", entry)
	}
}

func TestOwnerRemovalRemovesStaleTickets(t *testing.T) {
	d, _ := testLifecycleDaemon(t, false)

	_, sid, spawnToken := spawnLifecycleOwner(t, d, "stale-ticket-reap")
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner entry missing after spawn")
	}
	o := entry.Owner
	o.SessionMgr().PreRegisterForOwner("pending-reaped", sid, "/pending", nil)
	o.SessionMgr().PreRegister("pending-legacy", "/legacy", nil)
	seedReconnectHistoryForOwner(t, o, "bound-reaped", sid)

	if _, err := d.removeOwner(sid, ownerRemovalReasonOperatorHard, false); err != nil {
		t.Fatalf("removeOwner() error: %v", err)
	}
	if o.SessionMgr().IsPreRegistered("pending-reaped") {
		t.Fatal("owned pending token survived owner removal")
	}
	if o.SessionMgr().IsPreRegistered(spawnToken) {
		t.Fatal("spawn pending token survived owner removal")
	}
	if !o.SessionMgr().IsPreRegistered("pending-legacy") {
		t.Fatal("owner-keyless legacy pending token must remain TTL-only")
	}
	if _, err := o.SessionMgr().RegisterReconnect("bound-reaped", func(string) bool { return true }); !errors.Is(err, session.ErrUnknownToken) {
		t.Fatalf("RegisterReconnect(bound-reaped) error = %v, want session.ErrUnknownToken", err)
	}
	if _, err := d.HandleRefreshSessionToken("bound-reaped"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("HandleRefreshSessionToken(bound-reaped) error = %v, want ErrUnknownToken", err)
	}

	status := d.HandleStatus()
	assertOwnerRemovalStatus(t, status, 1, "operator_hard", 1)
	ownerRemoval := status["owner_removal"].(map[string]any)
	if got := uint64Status(t, ownerRemoval, "pending_tokens_removed"); got != 2 {
		t.Fatalf("pending_tokens_removed = %d, want 2", got)
	}
	if got := uint64Status(t, ownerRemoval, "bound_history_removed"); got != 1 {
		t.Fatalf("bound_history_removed = %d, want 1", got)
	}
}

func TestPersistentIdleParityPersistsThroughSnapshotRestore(t *testing.T) {
	os.Remove(SnapshotPath())
	t.Cleanup(func() { os.Remove(SnapshotPath()) })

	d1, _ := testLifecycleDaemon(t, false)
	_, sid, _ := spawnLifecycleOwner(t, d1, "persistent-restart")
	d1.SetPersistent(sid, true)
	before := d1.Entry(sid)
	if before == nil || !before.Persistent {
		t.Fatal("persistent owner missing before snapshot")
	}
	beforeGeneration := before.OwnerGeneration
	snapshotPath, err := d1.SerializeSnapshot()
	if err != nil {
		t.Fatalf("SerializeSnapshot() error: %v", err)
	}
	if snapshotPath == "" {
		t.Fatal("SerializeSnapshot() returned empty path")
	}
	d1.Shutdown()

	d2, _ := testLifecycleDaemon(t, false)
	if restored := d2.loadSnapshot(); restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
	}
	after := d2.Entry(sid)
	if after == nil || after.Owner == nil {
		t.Fatal("persistent owner missing after restore")
	}
	if !after.Persistent {
		t.Fatal("persistent flag was not preserved through snapshot restore")
	}
	if after.RestoredFromOwnerGeneration != beforeGeneration {
		t.Fatalf("restored_from_owner_generation = %q, want %q", after.RestoredFromOwnerGeneration, beforeGeneration)
	}
	if after.OwnerGeneration == "" || after.OwnerGeneration == beforeGeneration {
		t.Fatalf("restored owner generation = %q, want fresh non-empty generation distinct from %q", after.OwnerGeneration, beforeGeneration)
	}
	waitPastLifecycleIdle(t)
	if affected := (&Reaper{daemon: d2, logger: d2.logger}).sweep(); affected != 0 {
		t.Fatalf("restored persistent idle sweep affected %d owners, want 0", affected)
	}
}

func TestPersistentIdleParityReaperDoesNotReplaceOwnerGeneration(t *testing.T) {
	d, r := testLifecycleDaemon(t, false)

	_, sid, _ := spawnLifecycleOwner(t, d, "persistent-no-replacement")
	d.SetPersistent(sid, true)
	before := d.Entry(sid)
	if before == nil || before.Owner == nil {
		t.Fatal("persistent owner missing before shutdown")
	}
	beforeGeneration := before.OwnerGeneration
	beforeOwner := before.Owner
	beforeOwner.Shutdown()
	select {
	case <-beforeOwner.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not shut down")
	}

	if affected := r.sweep(); affected != 0 {
		t.Fatalf("persistent dead-owner sweep affected %d owners, want 0", affected)
	}
	after := d.Entry(sid)
	if after != before || after.Owner != beforeOwner {
		t.Fatal("reaper replaced persistent owner generation")
	}
	if !after.Persistent {
		t.Fatal("persistent flag lost")
	}
	if after.OwnerGeneration != beforeGeneration {
		t.Fatalf("owner generation = %q, want unchanged %q", after.OwnerGeneration, beforeGeneration)
	}

	status := d.HandleStatus()
	servers := status["servers"].([]map[string]any)
	if len(servers) != 1 {
		t.Fatalf("status servers = %d, want 1", len(servers))
	}
	if got := servers[0]["owner_generation"]; got != beforeGeneration {
		t.Fatalf("status owner_generation = %#v, want %q", got, beforeGeneration)
	}
}
