package daemon

import (
	"bufio"
	"fmt"
	"strings"
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

func TestZeroSessionCleanupReconnectReservationClosesWakeRace(t *testing.T) {
	d := testZeroSessionCleanupDaemon(t, time.Hour)

	ipcPath, sid, prevToken := spawnLifecycleOwner(t, d, "zero-cleanup-wake-race")
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner entry missing after spawn")
	}
	conn := dialLifecycleSession(t, ipcPath, prevToken)
	waitOwnerSessionCount(t, entry, 1)
	if err := conn.Close(); err != nil {
		t.Fatalf("initial conn.Close() error = %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)
	time.Sleep(20 * time.Millisecond)

	d.mu.Lock()
	zeroAt := time.Now().Add(-time.Second)
	entry.LastSession = zeroAt
	d.mu.Unlock()

	ownerAliveEntered := make(chan struct{})
	releaseOwnerAlive := make(chan struct{})
	type reconnectResult struct {
		token string
		err   error
	}
	reconnected := make(chan reconnectResult, 1)
	go func() {
		token, err := entry.Owner.SessionMgr().RegisterReconnect(prevToken, func(string) bool {
			close(ownerAliveEntered)
			<-releaseOwnerAlive
			return true
		})
		reconnected <- reconnectResult{token: token, err: err}
	}()

	select {
	case <-ownerAliveEntered:
	case <-time.After(time.Second):
		t.Fatal("ownerAlive barrier was not reached")
	}
	if got := entry.Owner.SessionMgr().PendingCount(); got != 1 {
		t.Fatalf("PendingCount() at ownerAlive barrier = %d, want 1", got)
	}
	if _, removed, err := d.removeOwnerIfCurrentAndZeroIdle(sid, entry, zeroAt, time.Millisecond); err != nil {
		t.Fatalf("removeOwnerIfCurrentAndZeroIdle() error = %v", err)
	} else if removed {
		t.Fatal("zero-session cleanup removed owner with reconnect reservation")
	}
	close(releaseOwnerAlive)

	result := <-reconnected
	if result.err != nil {
		t.Fatalf("RegisterReconnect() error = %v", result.err)
	}
	wakeConn := dialLifecycleSession(t, ipcPath, result.token)
	waitOwnerSessionCount(t, entry, 1)
	if _, err := fmt.Fprintln(wakeConn, `{"jsonrpc":"2.0","id":99,"method":"wake"}`); err != nil {
		t.Fatalf("write wake request: %v", err)
	}
	if err := wakeConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	reader := bufio.NewReader(wakeConn)
	response, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read wake response: %v", err)
	}
	if !strings.Contains(response, `"result"`) || strings.Contains(response, `-32603`) {
		t.Fatalf("wake response = %s, want successful result without orphan error", response)
	}
	if err := wakeConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline() for replay check error = %v", err)
	}
	if replay, err := reader.ReadString('\n'); err == nil {
		t.Fatalf("unexpected replayed wake response: %s", replay)
	}
	if err := wakeConn.Close(); err != nil {
		t.Fatalf("wake conn.Close() error = %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)

	d.mu.Lock()
	zeroAt = time.Now().Add(-time.Second)
	entry.LastSession = zeroAt
	d.mu.Unlock()
	if _, removed, err := d.removeOwnerIfCurrentAndZeroIdle(sid, entry, zeroAt, time.Millisecond); err != nil {
		t.Fatalf("removeOwnerIfCurrentAndZeroIdle() after bind error = %v", err)
	} else if !removed {
		t.Fatal("zero-session cleanup did not remove owner after reservation was consumed")
	}
}
