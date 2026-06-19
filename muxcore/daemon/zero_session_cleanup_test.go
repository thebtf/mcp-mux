package daemon

import (
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func testZeroSessionCleanupDaemon(t *testing.T, cleanupDelay time.Duration) *Daemon {
	t.Helper()
	ctlPath := shortSocketPath(t, "zero-session.ctl.sock")
	d, err := New(Config{
		Name:                    "test-daemon",
		ControlPath:             ctlPath,
		IdleTimeout:             5 * time.Second,
		OwnerIdleTimeout:        time.Hour,
		ZeroSessionCleanupDelay: cleanupDelay,
		SkipSnapshot:            true,
		Logger:                  testLogger(t),
		SessionHandler:          noopSessionHandler{},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	return d
}

func waitForOwnerMissing(t *testing.T, d *Daemon, sid string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.Entry(sid) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owner %s still present after zero-session cleanup", sid)
}

func TestZeroSessionCleanupAutoReapsDisposableOwner(t *testing.T) {
	d := testZeroSessionCleanupDaemon(t, 25*time.Millisecond)

	ipcPath, sid, token := spawnLifecycleOwner(t, d, "zero-cleanup-disposable")
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner entry missing after spawn")
	}
	conn := dialLifecycleSession(t, ipcPath, token)
	waitOwnerSessionCount(t, entry, 1)

	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close() error = %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)

	waitForOwnerMissing(t, d, sid)
	assertOwnerRemovalStatus(t, d.HandleStatus(), 1, "idle", 1)
}

func TestZeroSessionCleanupPersistentOwnerSurvives(t *testing.T) {
	d := testZeroSessionCleanupDaemon(t, 25*time.Millisecond)

	ipcPath, sid, token := spawnLifecycleOwner(t, d, "zero-cleanup-persistent")
	d.SetPersistent(sid, true)
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner entry missing after spawn")
	}
	conn := dialLifecycleSession(t, ipcPath, token)
	waitOwnerSessionCount(t, entry, 1)

	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close() error = %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)
	time.Sleep(100 * time.Millisecond)

	if got := d.Entry(sid); got == nil || !got.Persistent {
		t.Fatalf("persistent owner was removed by zero-session cleanup: %#v", got)
	}
}

func TestZeroSessionCleanupStaleTimerDoesNotRemoveReattachedOwner(t *testing.T) {
	d := testZeroSessionCleanupDaemon(t, 100*time.Millisecond)

	ipcPath1, sid1, token1 := spawnLifecycleOwner(t, d, "zero-cleanup-reattach")
	entry := d.Entry(sid1)
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner entry missing after first spawn")
	}
	conn1 := dialLifecycleSession(t, ipcPath1, token1)
	waitOwnerSessionCount(t, entry, 1)
	if err := conn1.Close(); err != nil {
		t.Fatalf("conn1.Close() error = %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)

	time.Sleep(25 * time.Millisecond)
	ipcPath2, sid2, token2, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "session-handler",
		Args:    []string{"zero-cleanup-reattach"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("second Spawn() error = %v", err)
	}
	if sid2 != sid1 || ipcPath2 != ipcPath1 {
		t.Fatalf("second Spawn should reuse owner, got sid=%q ipc=%q want sid=%q ipc=%q", sid2, ipcPath2, sid1, ipcPath1)
	}
	conn2 := dialLifecycleSession(t, ipcPath2, token2)
	waitOwnerSessionCount(t, entry, 1)

	time.Sleep(150 * time.Millisecond)
	if got := d.Entry(sid1); got == nil {
		t.Fatal("reattached owner was removed by stale zero-session cleanup timer")
	}

	if err := conn2.Close(); err != nil {
		t.Fatalf("conn2.Close() error = %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)
	waitForOwnerMissing(t, d, sid1)
}
