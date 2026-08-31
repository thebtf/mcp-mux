package daemon

import (
	"bytes"
	"errors"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func testDaemonWithReaper(t *testing.T, grace, idle time.Duration) (*Daemon, *Reaper) {
	t.Helper()
	ctlPath := shortSocketPath(t, "daemon.ctl.sock")
	d, err := New(Config{
		Name:        "test-daemon",
		ControlPath: ctlPath,
		GracePeriod: grace,
		IdleTimeout: idle,
		Logger:      log.New(os.Stderr, "[reaper-test] ", log.LstdFlags),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	r := NewReaper(d, 200*time.Millisecond) // fast sweep for tests
	t.Cleanup(func() {
		r.Stop()
		d.Shutdown()
	})
	return d, r
}

func TestReaperGracePeriodExpiry(t *testing.T) {
	d, _ := testDaemonWithReaper(t, 500*time.Millisecond, 1*time.Minute)

	_, sid, _, err := d.Spawn(control.Request{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner entry missing after spawn")
	}
	if removed := entry.Owner.SessionMgr().RemovePendingForOwner(sid); removed != 1 {
		t.Fatalf("RemovePendingForOwner() = %d, want 1", removed)
	}

	// Simulate zero sessions by setting LastSession to the past
	d.mu.Lock()
	if entry, ok := d.owners[sid]; ok {
		entry.LastSession = time.Now().Add(-2 * time.Second)
	}
	d.mu.Unlock()

	// Wait for reaper to sweep
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("reaper did not remove owner within timeout (count=%d)", d.OwnerCount())
		default:
			if d.OwnerCount() == 0 {
				return // success
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func TestReaperPersistentSurvivesGrace(t *testing.T) {
	d, _ := testDaemonWithReaper(t, 500*time.Millisecond, 1*time.Minute)

	_, sid, _, err := d.Spawn(control.Request{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	d.SetPersistent(sid, true)

	// Set LastSession to the past
	d.mu.Lock()
	if entry, ok := d.owners[sid]; ok {
		entry.LastSession = time.Now().Add(-2 * time.Second)
	}
	d.mu.Unlock()

	// Wait a few sweep cycles — persistent owner should survive
	time.Sleep(1 * time.Second)

	if d.OwnerCount() != 1 {
		t.Errorf("persistent owner was removed, OwnerCount() = %d", d.OwnerCount())
	}
}

func TestReaperRespectsConfigPersistent(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "[reaper-test] ", 0)
	ctlPath := shortSocketPath(t, "persistent.ctl.sock")
	d, err := New(Config{
		Name:             "test-daemon",
		ControlPath:      ctlPath,
		IdleTimeout:      5 * time.Second,
		OwnerIdleTimeout: 100 * time.Millisecond,
		SkipSnapshot:     true,
		Logger:           logger,
		Persistent:       true,
		SessionHandler:   noopSessionHandler{},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	_, sid, _, err := d.Spawn(control.Request{
		Cmd:  "spawn",
		Args: []string{t.Name()},
		Mode: "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	d.mu.Lock()
	entry := d.owners[sid]
	if entry == nil {
		d.mu.Unlock()
		t.Fatal("owner entry not found after Spawn")
	}
	entry.LastSession = time.Now().Add(-1 * time.Second)
	d.mu.Unlock()

	time.Sleep(150 * time.Millisecond)

	r := &Reaper{daemon: d, logger: logger}
	if affected := r.sweep(); affected != 0 {
		t.Fatalf("sweep() affected %d owners, want 0 for persistent owner", affected)
	}
	if d.OwnerCount() != 1 {
		t.Fatalf("OwnerCount() = %d after sweep, want 1", d.OwnerCount())
	}
	if got := d.Entry(sid); got == nil || !got.Persistent {
		t.Fatal("persistent owner lost after sweep")
	}
	if logOutput := logs.String(); strings.Contains(logOutput, "soft-removing") || strings.Contains(logOutput, "upstream dead with 0 sessions, removing") {
		t.Fatalf("reaper log indicates eviction for persistent owner: %s", logOutput)
	}
}

func TestReaperSnapshotsMutableOwnerEntryMetadata(t *testing.T) {
	d := testDaemon(t)
	sid := "reaper-entry-snapshot"
	o := testReconnectOwner(t, sid)
	entry := &OwnerEntry{
		Owner:       o,
		ServerID:    sid,
		Persistent:  true,
		LastSession: time.Now(),
		IdleTimeout: time.Hour,
	}
	d.mu.Lock()
	d.owners[sid] = entry
	d.mu.Unlock()

	const iterations = 1000
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			d.mu.Lock()
			entry.Persistent = true
			entry.LastSession = time.Now()
			entry.IdleTimeout = time.Hour
			d.mu.Unlock()
			runtime.Gosched()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		r := &Reaper{daemon: d, logger: d.logger}
		for range iterations {
			if affected := r.sweep(); affected != 0 {
				t.Errorf("sweep() affected %d persistent owners, want 0", affected)
			}
			runtime.Gosched()
		}
	}()
	close(start)
	wg.Wait()

	if got := d.Entry(sid); got != entry {
		t.Fatal("concurrent metadata snapshots replaced the owner entry")
	}
}

func TestReaperOwnerViewRejectsChangedEntryState(t *testing.T) {
	ownerRef := testReconnectOwner(t, "reaper-view-current")
	otherOwner := testReconnectOwner(t, "reaper-view-other")
	lastSession := time.Now()
	newView := func() (*OwnerEntry, reaperOwnerView) {
		entry := &OwnerEntry{
			Owner:           ownerRef,
			ServerID:        "native-reaper-view-current",
			ProtocolEra:     era.EraModern20260728,
			OwnerGeneration: "owner_gen_reaper_view",
			LastSession:     lastSession,
			IdleTimeout:     time.Minute,
		}
		return entry, reaperOwnerView{
			entry: entry,
			sid:   entry.ServerID,
			identity: ownerEntryIdentity{
				serverID:        entry.ServerID,
				protocolEra:     entry.ProtocolEra,
				ownerGeneration: entry.OwnerGeneration,
			},
			owner:       ownerRef,
			lastSession: lastSession,
			idleTimeout: time.Minute,
		}
	}

	entry, view := newView()
	if !view.matches(entry) {
		t.Fatal("unchanged exact owner identity did not match its reaper snapshot")
	}
	replacement := *entry
	if view.matches(&replacement) {
		t.Fatal("replacement entry matched stale reaper snapshot")
	}

	tests := []struct {
		name   string
		mutate func(*OwnerEntry)
	}{
		{name: "owner", mutate: func(entry *OwnerEntry) { entry.Owner = otherOwner }},
		{name: "server ID", mutate: func(entry *OwnerEntry) { entry.ServerID = "native-reaper-view-drift" }},
		{name: "protocol era", mutate: func(entry *OwnerEntry) { entry.ProtocolEra = era.EraLegacy }},
		{name: "owner generation", mutate: func(entry *OwnerEntry) { entry.OwnerGeneration = "owner_gen_reaper_view_drift" }},
		{name: "persistent", mutate: func(entry *OwnerEntry) { entry.Persistent = true }},
		{name: "idle timeout", mutate: func(entry *OwnerEntry) { entry.IdleTimeout += time.Second }},
		{name: "last session", mutate: func(entry *OwnerEntry) { entry.LastSession = entry.LastSession.Add(time.Second) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry, view := newView()
			test.mutate(entry)
			if view.matches(entry) {
				t.Fatal("changed owner entry matched stale reaper snapshot")
			}
		})
	}
}

func TestReaperRemovesEligibleModernOwnerWithoutReplacement(t *testing.T) {
	d := testDaemon(t)
	d.supervisor = nil

	const (
		sid        = "native-reaper-drain-modern"
		generation = "owner_gen_reaper_drain_modern"
	)
	o := testReconnectOwner(t, sid)
	entry := &OwnerEntry{
		Owner:           o,
		ServerID:        sid,
		ProtocolEra:     era.EraModern20260728,
		OwnerGeneration: generation,
		LastSession:     time.Now().Add(-time.Second),
		IdleTimeout:     time.Millisecond,
	}
	d.mu.Lock()
	d.owners[sid] = entry
	d.mu.Unlock()
	// Let the owner-local activity timestamp cross the configured idle boundary.
	time.Sleep(20 * time.Millisecond)

	original := finalizeOwnerForRemoval
	var calls atomic.Int32
	finalizeOwnerForRemoval = func(got *owner.Owner, soft bool) (int, bool, error) {
		if got != o || !soft {
			t.Fatalf("finalizer got owner=%p soft=%v, want owner=%p soft=true", got, soft, o)
		}
		calls.Add(1)
		return 0, true, nil
	}
	t.Cleanup(func() { finalizeOwnerForRemoval = original })

	if affected := (&Reaper{daemon: d, logger: d.logger}).sweep(); affected != 1 {
		t.Fatalf("sweep() affected=%d, want one eligible modern owner removed", affected)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("finalizer calls=%d, want one proven removal", got)
	}

	d.mu.RLock()
	current := d.owners[sid]
	ownerCount := len(d.owners)
	templateCount := len(d.templateCache)
	generationPresent := false
	for _, candidate := range d.owners {
		if candidate != nil && candidate.OwnerGeneration == generation {
			generationPresent = true
			break
		}
	}
	d.mu.RUnlock()
	if current != nil {
		t.Fatalf("modern owner entry remained after proven reaper removal: %p", current)
	}
	if generationPresent {
		t.Fatalf("removed modern generation %q remained in the owner registry", generation)
	}
	if ownerCount != 0 {
		t.Fatalf("owner registry count=%d, want no replacement owner", ownerCount)
	}
	if templateCount != 0 {
		t.Fatalf("template cache entries=%d, want no legacy template replacement", templateCount)
	}
	retryCounters := 0
	d.forcedIsolatedRetryCounters.Range(func(_, _ any) bool {
		retryCounters++
		return true
	})
	if retryCounters != 0 {
		t.Fatalf("forced isolated retry counters=%d, want no retry replacement", retryCounters)
	}
}

func TestReaperModernFinalizationUnprovenRetainsExactGeneration(t *testing.T) {
	d := testDaemon(t)
	d.supervisor = nil

	const (
		sid        = "native-reaper-finalization-modern"
		generation = "owner_gen_reaper_finalization_modern"
	)
	o := testReconnectOwner(t, sid)
	entry := &OwnerEntry{
		Owner:           o,
		ServerID:        sid,
		ProtocolEra:     era.EraModern20260728,
		OwnerGeneration: generation,
		LastSession:     time.Now().Add(-time.Second),
		IdleTimeout:     time.Millisecond,
	}
	d.mu.Lock()
	d.owners[sid] = entry
	d.mu.Unlock()
	// Let the owner-local activity timestamp cross the configured idle boundary.
	time.Sleep(20 * time.Millisecond)

	original := finalizeOwnerForRemoval
	var calls atomic.Int32
	retryStarted := make(chan struct{})
	allowRetry := make(chan struct{})
	var retryStartedOnce sync.Once
	var allowRetryOnce sync.Once
	finalizeOwnerForRemoval = func(got *owner.Owner, soft bool) (int, bool, error) {
		call := calls.Add(1)
		if got != o || !soft {
			t.Fatalf("finalizer call %d got owner=%p soft=%v, want owner=%p soft=true", call, got, soft, o)
		}
		if call <= ownerFinalizationAttempts {
			return 0, false, errors.New("synthetic modern retirement proof pending")
		}
		retryStartedOnce.Do(func() { close(retryStarted) })
		<-allowRetry
		return 0, true, nil
	}
	t.Cleanup(func() {
		allowRetryOnce.Do(func() { close(allowRetry) })
		finalizeOwnerForRemoval = original
	})

	if affected := (&Reaper{daemon: d, logger: d.logger}).sweep(); affected != 0 {
		t.Fatalf("sweep() affected=%d, want blocked modern owner retained until finalization proof", affected)
	}
	if got := calls.Load(); got != ownerFinalizationAttempts {
		t.Fatalf("synchronous finalizer calls=%d, want %d", got, ownerFinalizationAttempts)
	}

	d.mu.RLock()
	current := d.owners[sid]
	ownerCount := len(d.owners)
	templateCount := len(d.templateCache)
	retrying := current != nil && current.removalRetrying
	removalInProgress := current != nil && current.removalInProgress
	d.mu.RUnlock()
	if current != entry {
		t.Fatalf("reaper replaced blocked modern entry: got=%p want=%p", current, entry)
	}
	if current.OwnerGeneration != generation || current.ProtocolEra != era.EraModern20260728 {
		t.Fatalf("blocked modern entry identity=(generation=%q era=%v), want (%q,%v)", current.OwnerGeneration, current.ProtocolEra, generation, era.EraModern20260728)
	}
	if ownerCount != 1 {
		t.Fatalf("owner registry count=%d, want exact blocked generation only", ownerCount)
	}
	if templateCount != 0 {
		t.Fatalf("template cache entries=%d, want no legacy template replacement", templateCount)
	}
	if !retrying {
		t.Fatal("reaper did not schedule retry for unproven modern finalization")
	}
	if removalInProgress {
		t.Fatal("unproven finalization left the exact owner removal attempt serialized forever")
	}

	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		t.Fatal("scheduled modern finalization retry did not use the existing finalizer")
	}
	d.mu.RLock()
	retryCurrent := d.owners[sid]
	d.mu.RUnlock()
	if retryCurrent != entry {
		t.Fatalf("scheduled finalization retried replacement entry: got=%p want=%p", retryCurrent, entry)
	}
	if retryCurrent.OwnerGeneration != generation || retryCurrent.ProtocolEra != era.EraModern20260728 {
		t.Fatalf("scheduled finalization retried identity=(generation=%q era=%v), want (%q,%v)", retryCurrent.OwnerGeneration, retryCurrent.ProtocolEra, generation, era.EraModern20260728)
	}
	allowRetryOnce.Do(func() { close(allowRetry) })

	waitForDaemonCondition(t, time.Second, func() bool { return d.Entry(sid) == nil }, "scheduled modern finalization retry did not remove the proven generation")
	if got := calls.Load(); got != ownerFinalizationAttempts+1 {
		t.Fatalf("finalizer calls=%d, want %d including one scheduled retry", got, ownerFinalizationAttempts+1)
	}
	if d.OwnerCount() != 0 {
		t.Fatalf("owner registry count=%d after proven retry, want no replacement", d.OwnerCount())
	}
}

func TestReaperIdleAutoExit(t *testing.T) {
	d, _ := testDaemonWithReaper(t, 100*time.Millisecond, 500*time.Millisecond)

	// No owners spawned — daemon should auto-exit after idle timeout
	select {
	case <-d.Done():
		// success — daemon auto-exited
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not auto-exit after idle timeout")
	}
}
