package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
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

func TestSpawnResponseFailureRevokesReservationAndSchedulesCleanup(t *testing.T) {
	cleanupDelay := 100 * time.Millisecond
	d := testZeroSessionCleanupDaemon(t, cleanupDelay)

	ipcPath, sid, initialToken := spawnLifecycleOwner(t, d, "undelivered-spawn")
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner entry missing after spawn")
	}
	conn := dialLifecycleSession(t, ipcPath, initialToken)
	waitOwnerSessionCount(t, entry, 1)
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close() error = %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)

	_, reusedSID, token := spawnLifecycleOwner(t, d, "undelivered-spawn")
	if reusedSID != sid {
		t.Fatalf("undelivered spawn created owner %q, want reuse of %q", reusedSID, sid)
	}
	if !entry.Owner.SessionMgr().IsPreRegistered(token) {
		t.Fatal("spawn token was not pending before rollback")
	}

	// The disconnect above already scheduled zero-session cleanup. The new
	// reservation must keep that stale timer from removing the reused owner.
	time.Sleep(2 * cleanupDelay)
	if d.Entry(sid) != entry || !entry.Owner.SessionMgr().IsPreRegistered(token) {
		t.Fatal("pending spawn token did not retain owner before rollback")
	}

	d.HandleSpawnResponseFailure(sid, token)
	if entry.Owner.SessionMgr().IsPreRegistered(token) {
		t.Fatal("undelivered spawn token remained pending")
	}
	waitForOwnerMissing(t, d, sid)
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
	waitForDaemonCondition(t, time.Second, func() bool {
		d.mu.RLock()
		defer d.mu.RUnlock()
		return !entry.LastSession.Equal(zeroAt)
	}, "wake disconnect did not publish zero-session timestamp")

	d.mu.Lock()
	zeroAt = time.Now().Add(-time.Second)
	entry.LastSession = zeroAt
	d.mu.Unlock()
	var cleanupErr error
	waitForDaemonCondition(t, 5*time.Second, func() bool {
		_, removed, err := d.removeOwnerIfCurrentAndZeroIdle(sid, entry, zeroAt, time.Millisecond)
		if err != nil {
			cleanupErr = err
			return true
		}
		return removed
	}, "zero-session cleanup remained blocked after reservation was consumed")
	if cleanupErr != nil {
		t.Fatalf("removeOwnerIfCurrentAndZeroIdle() after bind error = %v", cleanupErr)
	}
}

func TestOwnerEntryIdentityAndZeroIdleOwnerViewRequireExactModernFacts(t *testing.T) {
	const (
		sid        = "native-zero-idle-exact"
		generation = "owner_gen_modern_exact"
	)
	zeroAt := time.Now().UTC()
	ownerRef := testReconnectOwner(t, sid)
	entry := &OwnerEntry{
		Owner:           ownerRef,
		ServerID:        sid,
		ProtocolEra:     era.EraModern20260728,
		OwnerGeneration: generation,
		LastSession:     zeroAt,
	}
	identity := ownerEntryIdentity{
		serverID:        entry.ServerID,
		protocolEra:     entry.ProtocolEra,
		ownerGeneration: entry.OwnerGeneration,
	}
	view := zeroIdleOwnerView{
		identity: identity,
		entry:    entry,
		owner:    ownerRef,
		zeroAt:   zeroAt,
	}

	if !identity.matches(entry) {
		t.Fatal("ownerEntryIdentity rejected unchanged modern entry")
	}
	if !view.matches(entry) {
		t.Fatal("zeroIdleOwnerView rejected unchanged exact modern entry")
	}

	sameFacts := *entry
	if !identity.matches(&sameFacts) {
		t.Fatal("ownerEntryIdentity rejected equal modern identity facts")
	}
	if view.matches(&sameFacts) {
		t.Fatal("zeroIdleOwnerView accepted a different entry pointer")
	}

	replacementOwner := testReconnectOwner(t, "native-zero-idle-replacement")
	entry.Owner = replacementOwner
	if view.matches(entry) {
		t.Fatal("zeroIdleOwnerView accepted a different owner pointer")
	}
	entry.Owner = ownerRef

	for _, test := range []struct {
		name            string
		identityMatches bool
		mutate          func(*OwnerEntry)
	}{
		{
			name: "server ID drift",
			mutate: func(current *OwnerEntry) {
				current.ServerID = "native-zero-idle-drifted"
			},
		},
		{
			name: "protocol era drift",
			mutate: func(current *OwnerEntry) {
				current.ProtocolEra = era.EraLegacy
			},
		},
		{
			name: "owner generation drift",
			mutate: func(current *OwnerEntry) {
				current.OwnerGeneration = "owner_gen_modern_drifted"
			},
		},
		{
			name:            "persistence changed",
			identityMatches: true,
			mutate: func(current *OwnerEntry) {
				current.Persistent = true
			},
		},
		{
			name:            "zero session timestamp changed",
			identityMatches: true,
			mutate: func(current *OwnerEntry) {
				current.LastSession = zeroAt.Add(time.Nanosecond)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := *entry
			test.mutate(&candidate)
			if got := identity.matches(&candidate); got != test.identityMatches {
				t.Fatalf("ownerEntryIdentity.matches() = %v, want %v", got, test.identityMatches)
			}

			saved := *entry
			test.mutate(entry)
			if view.matches(entry) {
				t.Fatal("zeroIdleOwnerView accepted changed eligibility facts")
			}
			*entry = saved
		})
	}
}

func TestZeroSessionCleanupRejectsRegistryKeyIdentityMismatch(t *testing.T) {
	d := testZeroSessionCleanupDaemon(t, time.Hour)
	d.supervisor = nil
	const (
		registryKey = "native-zero-idle-registry-key"
		entryID     = "native-zero-idle-entry-id"
	)
	ownerRef := testReconnectOwner(t, entryID)
	zeroAt := time.Now().Add(-time.Second)
	entry := &OwnerEntry{
		Owner:           ownerRef,
		ServerID:        entryID,
		ProtocolEra:     era.EraModern20260728,
		OwnerGeneration: "owner_gen_registry_mismatch",
		LastSession:     zeroAt,
		IdleTimeout:     time.Millisecond,
	}
	d.mu.Lock()
	d.owners[registryKey] = entry
	d.mu.Unlock()
	time.Sleep(20 * time.Millisecond)

	original := finalizeOwnerForRemoval
	finalizerCalled := false
	finalizeOwnerForRemoval = func(*owner.Owner, bool) (int, bool, error) {
		finalizerCalled = true
		return 0, true, nil
	}
	t.Cleanup(func() { finalizeOwnerForRemoval = original })

	if _, removed, err := d.removeOwnerIfCurrentAndZeroIdle(registryKey, entry, zeroAt, time.Millisecond); err != nil {
		t.Fatalf("removeOwnerIfCurrentAndZeroIdle() error: %v", err)
	} else if removed {
		t.Fatal("zero-session cleanup removed an entry whose ServerID did not match its registry key")
	}
	if finalizerCalled {
		t.Fatal("zero-session cleanup finalized an entry whose ServerID did not match its registry key")
	}
	if current := d.Entry(registryKey); current != entry {
		t.Fatalf("registry-key mismatch changed current entry: got=%p want=%p", current, entry)
	}
}

func TestZeroSessionCleanupModernOwnerRemovesWithoutLegacyRecoveryState(t *testing.T) {
	const label = "zero-cleanup-modern-disposable"
	d := testZeroSessionCleanupDaemon(t, 25*time.Millisecond)

	ipcPath, sid, token, err := d.Spawn(control.Request{
		Cmd:         "spawn",
		Command:     "session-handler",
		Args:        []string{label},
		Mode:        "global",
		ProtocolEra: "2026-07-28",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("modern owner entry missing after spawn")
	}
	if entry.ProtocolEra != era.EraModern20260728 || !isModernOwnerID(sid) {
		t.Fatalf("spawned entry is not exact modern owner: sid=%q era=%v", sid, entry.ProtocolEra)
	}
	generation := entry.OwnerGeneration
	if generation == "" {
		t.Fatal("modern owner generation is empty")
	}

	conn := dialLifecycleSession(t, ipcPath, token)
	waitOwnerSessionCount(t, entry, 1)
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close() error: %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)
	waitForOwnerMissing(t, d, sid)

	if current := d.Entry(sid); current != nil {
		t.Fatalf("zero-session cleanup left a modern successor: %#v", current)
	}
	if got := d.OwnerCount(); got != 0 {
		t.Fatalf("owner count after modern cleanup = %d, want no successor", got)
	}
	if entry.OwnerGeneration != generation {
		t.Fatalf("removed modern entry generation changed from %q to %q", generation, entry.OwnerGeneration)
	}
	if _, ok := d.getTemplate("session-handler", []string{label}); ok {
		t.Fatal("modern zero-session cleanup published a legacy template")
	}
	retryCounters := 0
	d.forcedIsolatedRetryCounters.Range(func(_, _ any) bool {
		retryCounters++
		return true
	})
	if retryCounters != 0 {
		t.Fatalf("modern zero-session cleanup hydrated %d retry counters", retryCounters)
	}
	if _, err := d.HandleRefreshSessionToken(token); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("HandleRefreshSessionToken(%q) error = %v, want ErrUnknownToken after removal", token, err)
	}
	assertOwnerRemovalStatus(t, d.HandleStatus(), 1, "idle", 1)
}

func TestZeroSessionCleanupModernReconnectReservationRetainsThenRemovesSameGeneration(t *testing.T) {
	d := testZeroSessionCleanupDaemon(t, time.Hour)

	ipcPath, sid, previousToken, err := d.Spawn(control.Request{
		Cmd:         "spawn",
		Command:     "session-handler",
		Args:        []string{"zero-cleanup-modern-reconnect"},
		Mode:        "global",
		ProtocolEra: "2026-07-28",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("modern owner entry missing after spawn")
	}
	if entry.ProtocolEra != era.EraModern20260728 || !isModernOwnerID(sid) {
		t.Fatalf("spawned entry is not exact modern owner: sid=%q era=%v", sid, entry.ProtocolEra)
	}
	generation := entry.OwnerGeneration

	conn := dialLifecycleSession(t, ipcPath, previousToken)
	waitOwnerSessionCount(t, entry, 1)
	lastSessionBeforeDisconnect := entry.LastSession
	if err := conn.Close(); err != nil {
		t.Fatalf("initial conn.Close() error: %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)
	waitForDaemonCondition(t, time.Second, func() bool {
		d.mu.RLock()
		defer d.mu.RUnlock()
		return !entry.LastSession.Equal(lastSessionBeforeDisconnect)
	}, "initial modern disconnect did not publish zero-session timestamp")

	zeroAt := time.Now().Add(-time.Second)
	d.mu.Lock()
	entry.LastSession = zeroAt
	d.mu.Unlock()
	reconnectToken, err := entry.Owner.SessionMgr().RegisterReconnect(previousToken, func(ownerKey string) bool {
		return ownerKey == sid && d.Entry(ownerKey) == entry
	})
	if err != nil {
		t.Fatalf("RegisterReconnect() error: %v", err)
	}
	if got := entry.Owner.SessionMgr().PendingCount(); got != 1 {
		t.Fatalf("PendingCount() with modern reconnect reservation = %d, want 1", got)
	}
	if _, removed, err := d.removeOwnerIfCurrentAndZeroIdle(sid, entry, zeroAt, time.Millisecond); err != nil {
		t.Fatalf("removeOwnerIfCurrentAndZeroIdle() with reservation error: %v", err)
	} else if removed {
		t.Fatal("zero-session cleanup removed modern owner with reconnect reservation")
	}
	if current := d.Entry(sid); current != entry || current.OwnerGeneration != generation {
		t.Fatalf("reconnect reservation replaced modern generation: %#v", current)
	}

	wakeConn := dialLifecycleSession(t, ipcPath, reconnectToken)
	waitOwnerSessionCount(t, entry, 1)
	if got := entry.Owner.SessionMgr().PendingCount(); got != 0 {
		t.Fatalf("PendingCount() after reconnect consumption = %d, want 0", got)
	}
	lastSessionBeforeWakeDisconnect := entry.LastSession
	if err := wakeConn.Close(); err != nil {
		t.Fatalf("wake conn.Close() error: %v", err)
	}
	waitOwnerSessionCount(t, entry, 0)
	waitForDaemonCondition(t, time.Second, func() bool {
		d.mu.RLock()
		defer d.mu.RUnlock()
		return !entry.LastSession.Equal(lastSessionBeforeWakeDisconnect)
	}, "consumed modern reconnect did not publish zero-session timestamp")

	zeroAt = time.Now().Add(-time.Second)
	d.mu.Lock()
	entry.LastSession = zeroAt
	d.mu.Unlock()
	var cleanupErr error
	waitForDaemonCondition(t, 5*time.Second, func() bool {
		_, removed, err := d.removeOwnerIfCurrentAndZeroIdle(sid, entry, zeroAt, time.Millisecond)
		if err != nil {
			cleanupErr = err
			return true
		}
		return removed
	}, "zero-session cleanup remained blocked after modern reservation consumption")
	if cleanupErr != nil {
		t.Fatalf("removeOwnerIfCurrentAndZeroIdle() after reservation consumption error: %v", cleanupErr)
	}
	if entry.OwnerGeneration != generation {
		t.Fatalf("modern generation changed before removal: got %q want %q", entry.OwnerGeneration, generation)
	}
	if current := d.Entry(sid); current != nil {
		t.Fatalf("modern owner remained after consumed reconnect cleanup: %#v", current)
	}
	if got := d.OwnerCount(); got != 0 {
		t.Fatalf("owner count after consumed reconnect cleanup = %d, want 0", got)
	}
	if _, err := d.HandleRefreshSessionToken(reconnectToken); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("HandleRefreshSessionToken(%q) error = %v, want ErrUnknownToken after removal", reconnectToken, err)
	}
}

func TestZeroSessionCleanupModernFinalizationUnprovenRetainsExactGeneration(t *testing.T) {
	const (
		sid        = "native-zero-idle-finalization"
		generation = "owner_gen_modern_finalization"
		pending    = "modern-finalization-pending"
		bound      = "modern-finalization-bound"
	)
	d := testZeroSessionCleanupDaemon(t, time.Hour)
	d.supervisor = nil
	ownerRef := testReconnectOwner(t, sid)
	zeroAt := time.Now().Add(-time.Second)
	entry := &OwnerEntry{
		Owner:           ownerRef,
		ServerID:        sid,
		Command:         "modern-finalization-command",
		Args:            []string{"--modern"},
		ProtocolEra:     era.EraModern20260728,
		OwnerGeneration: generation,
		LastSession:     zeroAt,
	}
	ownerRef.SessionMgr().PreRegisterForOwner(pending, sid, "/modern", nil)
	seedReconnectHistoryForOwner(t, ownerRef, bound, sid)
	d.mu.Lock()
	d.owners[sid] = entry
	d.mu.Unlock()

	original := finalizeOwnerForRemoval
	t.Cleanup(func() { finalizeOwnerForRemoval = original })
	calls := 0
	finalizeOwnerForRemoval = func(got *owner.Owner, soft bool) (int, bool, error) {
		if got != ownerRef || !soft {
			t.Fatalf("finalizer got owner=%p soft=%v, want owner=%p soft=true", got, soft, ownerRef)
		}
		calls++
		return 0, false, errors.New("synthetic modern retirement proof pending")
	}

	view := zeroIdleOwnerView{
		identity: ownerEntryIdentity{
			serverID:        entry.ServerID,
			protocolEra:     entry.ProtocolEra,
			ownerGeneration: entry.OwnerGeneration,
		},
		entry:  entry,
		owner:  ownerRef,
		zeroAt: zeroAt,
	}
	result, err := d.finalizeAndRemoveOwner(sid, entry, ownerRemovalReasonIdle, true, view.matches, false)
	if err == nil {
		t.Fatal("finalizeAndRemoveOwner() error = nil, want unproven retirement")
	}
	if result.Removed {
		t.Fatalf("finalizeAndRemoveOwner() removed modern owner before proof: %+v", result)
	}
	if calls != ownerFinalizationAttempts {
		t.Fatalf("finalizer calls = %d, want %d bounded proof attempts", calls, ownerFinalizationAttempts)
	}
	current := d.Entry(sid)
	if current != entry {
		t.Fatalf("unproven finalization replaced or forgot modern entry: %#v", current)
	}
	if current.Owner != ownerRef || current.ServerID != sid || current.ProtocolEra != era.EraModern20260728 || current.OwnerGeneration != generation {
		t.Fatalf("unproven finalization changed modern identity: %#v", current)
	}
	if current.removalInProgress {
		t.Fatal("unproven finalization left removal serialization stuck")
	}
	if !ownerRef.SessionMgr().IsPreRegistered(pending) {
		t.Fatal("unproven finalization removed modern pending reservation")
	}
	if ownerKey, _, _, ok := ownerRef.SessionMgr().LookupHistory(bound); !ok || ownerKey != sid {
		t.Fatalf("unproven finalization removed modern reconnect history: owner=%q ok=%v", ownerKey, ok)
	}
	if _, ok := d.getTemplate(entry.Command, entry.Args); ok {
		t.Fatal("unproven modern finalization published a legacy template")
	}
	retryCounters := 0
	d.forcedIsolatedRetryCounters.Range(func(_, _ any) bool {
		retryCounters++
		return true
	})
	if retryCounters != 0 {
		t.Fatalf("unproven modern finalization hydrated %d retry counters", retryCounters)
	}
	if got := d.OwnerCount(); got != 1 {
		t.Fatalf("unproven modern finalization owner count = %d, want exact retained owner", got)
	}
}
