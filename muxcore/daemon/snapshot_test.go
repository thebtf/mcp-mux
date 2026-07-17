package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/control"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
)

func TestSerializeSnapshotPinnedRetainsOwnerBarrierUntilRelease(t *testing.T) {
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })

	d := testDaemon(t)
	command := "pinned-snapshot-owner"
	d.updateTemplate(command, nil, daemonMaterializationSnapshot(false))
	_, sid, _, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	path, lease, err := d.serializeSnapshotPinned()
	if err != nil {
		t.Fatalf("serializeSnapshotPinned: %v", err)
	}
	if path == "" || lease == nil {
		t.Fatalf("pinned snapshot result path=%q lease=%v", path, lease)
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner.Status()["restart_pin_count"] != int64(1) {
		t.Fatalf("owner restart barrier was released before predecessor completion: entry=%+v", entry)
	}
	d.mu.RLock()
	registryPins := entry.snapshotPins
	d.mu.RUnlock()
	if registryPins != 1 {
		t.Fatalf("daemon registry pin count=%d, want 1 through takeover", registryPins)
	}

	lease.ReleaseOwnerPins()
	lease.ReleaseOwnerPins()
	if got := entry.Owner.Status()["restart_pin_count"]; got != int64(0) {
		t.Fatalf("owner restart lease release count=%v, want 0", got)
	}
	d.mu.RLock()
	registryPins = entry.snapshotPins
	d.mu.RUnlock()
	if registryPins != 1 {
		t.Fatalf("owner-pin release also dropped daemon registry pin: %d", registryPins)
	}

	lease.ReleaseRegistryPins()
	lease.ReleaseRegistryPins()
	d.mu.RLock()
	registryPins = entry.snapshotPins
	d.mu.RUnlock()
	if registryPins != 0 {
		t.Fatalf("daemon registry pin count=%d after release, want 0", registryPins)
	}
	lease.Release()
}

func TestRestartRestoreInvalidatesSecondaryDiscoveryCaches(t *testing.T) {
	t.Setenv(snapshotRestartEnv, "1")
	d := testDaemon(t)
	snap := daemonMaterializationSnapshot(false)
	snap.ServerID = "restart-cache-invalidation"
	snap.Command = "cache-invalidation-command"
	snap.Classification = classify.ModeShared
	snap.CachedPrompts = snap.CachedTools
	snap.CachedResources = snap.CachedTools
	snap.CachedResourceTemplates = snap.CachedTools

	entry, reattached, err := d.restoreSnapshotPlan(d.makeSnapshotRestorePlan(snap), nil, "snapshot_fallback", false, true)
	if err != nil {
		t.Fatalf("restoreSnapshotPlan: %v", err)
	}
	if reattached || entry == nil || entry.Owner == nil {
		t.Fatalf("restore result entry=%+v reattached=%v", entry, reattached)
	}
	restored := entry.Owner.ExportSnapshot()
	if restored.CachedInit == "" {
		t.Fatal("restart restore discarded cached initialize policy")
	}
	if restored.CachedTools != "" || restored.CachedPrompts != "" || restored.CachedResources != "" || restored.CachedResourceTemplates != "" {
		t.Fatalf("restart restore exposed stale secondary discovery caches: tools=%t prompts=%t resources=%t templates=%t",
			restored.CachedTools != "", restored.CachedPrompts != "", restored.CachedResources != "", restored.CachedResourceTemplates != "")
	}
	if entry.Owner.CacheReady() {
		t.Fatal("restart-restored owner reported cache ready before live tools/list refresh")
	}
	if _, ok := d.getTemplate(snap.Command, snap.Args); ok {
		t.Fatal("restart restore republished stale template cache")
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	d := testDaemon(t)

	// Create a real owner via Spawn
	req := control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "cwd",
	}
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	// Mark classified so ExportSnapshot has data
	d.mu.RLock()
	entry := d.owners[sid]
	d.mu.RUnlock()
	if entry != nil && entry.Owner != nil {
		entry.Owner.MarkClassified()
	}

	// Serialize
	path, err := d.SerializeSnapshot()
	if err != nil {
		t.Fatalf("SerializeSnapshot() error: %v", err)
	}
	defer os.Remove(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not found at %s", path)
	}

	// Deserialize
	snap, err := DeserializeSnapshot(testLogger(t))
	if err != nil {
		t.Fatalf("DeserializeSnapshot() error: %v", err)
	}
	if snap == nil {
		t.Fatal("DeserializeSnapshot() returned nil")
	}
	if snap.Version != mcpsnapshot.SnapshotVersion {
		t.Errorf("version = %d, want %d", snap.Version, mcpsnapshot.SnapshotVersion)
	}
	if len(snap.Owners) != 1 {
		t.Errorf("owners count = %d, want 1", len(snap.Owners))
	}
	if snap.Owners[0].Command != req.Command {
		t.Errorf("owner command = %q, want %q", snap.Owners[0].Command, req.Command)
	}

	// Verify file was consumed (deleted)
	if _, err := os.Stat(SnapshotPath()); !os.IsNotExist(err) {
		t.Error("snapshot file should be deleted after successful load")
	}
}

func TestSerializeSnapshotPinsOwnerAgainstConcurrentRemoval(t *testing.T) {
	d := testDaemon(t)
	req := control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	}
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	d.beforeSnapshotOwnerExport = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { d.beforeSnapshotOwnerExport = nil })

	type serializeResult struct {
		path string
		err  error
	}
	serializeDone := make(chan serializeResult, 1)
	go func() {
		path, serializeErr := d.SerializeSnapshot()
		serializeDone <- serializeResult{path: path, err: serializeErr}
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("SerializeSnapshot did not reach pinned owner export")
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- d.Remove(sid) }()
	select {
	case err := <-removeDone:
		t.Fatalf("Remove completed while snapshot held owner pin: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	result := <-serializeDone
	if result.err != nil {
		t.Fatalf("SerializeSnapshot() error: %v", result.err)
	}
	defer os.Remove(result.path)

	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("Remove() after snapshot release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Remove remained blocked after snapshot released owner pin")
	}

	snap, err := DeserializeSnapshot(testLogger(t))
	if err != nil {
		t.Fatalf("DeserializeSnapshot() error: %v", err)
	}
	if snap == nil || len(snap.Owners) != 1 || snap.Owners[0].ServerID != sid {
		t.Fatalf("snapshot lost pinned owner %s: %#v", sid, snap)
	}
}

func TestSnapshotCorruptJSON(t *testing.T) {
	path := SnapshotPath()
	if err := os.WriteFile(path, []byte("{invalid json!!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	snap, err := DeserializeSnapshot(testLogger(t))
	if err != nil {
		t.Fatalf("DeserializeSnapshot() should not return error for corrupt JSON: %v", err)
	}
	if snap != nil {
		t.Error("DeserializeSnapshot() should return nil for corrupt JSON")
	}

	// File should be deleted
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("corrupt snapshot file should be deleted")
	}
}

func TestSnapshotStaleTimestamp(t *testing.T) {
	stale := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
		Owners:    []mcpsnapshot.OwnerSnapshot{},
		Sessions:  []mcpsnapshot.SessionSnapshot{},
	}
	data, _ := json.Marshal(stale)

	path := SnapshotPath()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	snap, err := DeserializeSnapshot(testLogger(t))
	if err != nil {
		t.Fatalf("DeserializeSnapshot() error: %v", err)
	}
	if snap != nil {
		t.Error("DeserializeSnapshot() should return nil for stale snapshot")
	}

	// Stale file should be deleted
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("stale snapshot file should be deleted")
	}
}

func TestSnapshotVersionMismatch(t *testing.T) {
	future := DaemonSnapshot{
		Version:   999,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners:    []mcpsnapshot.OwnerSnapshot{},
		Sessions:  []mcpsnapshot.SessionSnapshot{},
	}
	data, _ := json.Marshal(future)

	path := SnapshotPath()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	snap, err := DeserializeSnapshot(testLogger(t))
	if err != nil {
		t.Fatalf("DeserializeSnapshot() error: %v", err)
	}
	if snap != nil {
		t.Error("DeserializeSnapshot() should return nil for version mismatch")
	}
}

func TestSnapshotEmptyOwners(t *testing.T) {
	valid := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners:    []mcpsnapshot.OwnerSnapshot{},
		Sessions:  []mcpsnapshot.SessionSnapshot{},
	}
	data, _ := json.Marshal(valid)

	path := SnapshotPath()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	snap, err := DeserializeSnapshot(testLogger(t))
	if err != nil {
		t.Fatalf("DeserializeSnapshot() error: %v", err)
	}
	if snap == nil {
		t.Fatal("DeserializeSnapshot() should return valid snapshot with 0 owners")
	}
	if len(snap.Owners) != 0 {
		t.Errorf("owners count = %d, want 0", len(snap.Owners))
	}
}

func TestSnapshotMissingFile(t *testing.T) {
	// Ensure no snapshot file exists
	os.Remove(SnapshotPath())

	snap, err := DeserializeSnapshot(testLogger(t))
	if err != nil {
		t.Fatalf("DeserializeSnapshot() error for missing file: %v", err)
	}
	if snap != nil {
		t.Error("DeserializeSnapshot() should return nil for missing file (cold start)")
	}
}

func TestSnapshotAtomicWrite(t *testing.T) {
	d := testDaemon(t)

	// Serialize creates temp file and renames atomically
	path, err := d.SerializeSnapshot()
	if err != nil {
		t.Fatalf("SerializeSnapshot() error: %v", err)
	}
	defer os.Remove(path)

	// Verify no temp files remain
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "mcp-muxd-snapshot-*.tmp"))
	if len(matches) > 0 {
		t.Errorf("temp files remaining after atomic write: %v", matches)
	}

	// Verify final file exists and is valid JSON
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap DaemonSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Errorf("snapshot is not valid JSON: %v", err)
	}
}

func TestSnapshotOwnerWithClassification(t *testing.T) {
	valid := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:             "abc123",
				Command:              "uvx",
				Args:                 []string{"--from", "serena"},
				Cwd:                  "/dev/project",
				CwdSet:               []string{"/dev/project", "/dev/other"},
				Mode:                 "cwd",
				Classification:       classify.ModeIsolated,
				ClassificationSource: "capability",
				CachedInit:           "eyJqc29ucnBjIjoiMi4wIn0=", // base64 of {"jsonrpc":"2.0"}
			},
		},
		Sessions: []mcpsnapshot.SessionSnapshot{
			{
				MuxSessionID:  "sess_12345678",
				Cwd:           "/dev/project",
				OwnerServerID: "abc123",
			},
		},
	}
	data, _ := json.Marshal(valid)

	path := SnapshotPath()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	snap, err := DeserializeSnapshot(testLogger(t))
	if err != nil {
		t.Fatalf("DeserializeSnapshot() error: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot should load successfully")
	}
	if len(snap.Owners) != 1 {
		t.Fatalf("owners = %d, want 1", len(snap.Owners))
	}
	owner := snap.Owners[0]
	if owner.Classification != classify.ModeIsolated {
		t.Errorf("classification = %q, want %q", owner.Classification, classify.ModeIsolated)
	}
	if owner.CachedInit == "" {
		t.Error("cached_init should not be empty")
	}
	if len(snap.Sessions) != 1 {
		t.Errorf("sessions = %d, want 1", len(snap.Sessions))
	}
}

func TestGracefulRestartCycle(t *testing.T) {
	// Clean any stale snapshot from previous test runs
	os.Remove(SnapshotPath())

	// Phase 1: Create daemon with a real owner
	d1 := testDaemon(t)
	req := control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "cwd",
	}
	_, sid, _, err := d1.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	// Wait for proactive init to complete (init response cached)
	d1.mu.RLock()
	entry := d1.owners[sid]
	d1.mu.RUnlock()
	if entry != nil && entry.Owner != nil {
		select {
		case <-entry.Owner.InitReady():
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for init")
		}
		entry.Owner.MarkClassified()
	}

	// Phase 2: Serialize snapshot (graceful restart)
	snapshotPath, err := d1.SerializeSnapshot()
	if err != nil {
		t.Fatalf("SerializeSnapshot() error: %v", err)
	}

	// Verify snapshot file exists
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatalf("snapshot file not found: %v", err)
	}

	// Phase 3: Shutdown old daemon
	d1.Shutdown()

	// Phase 4: New daemon loads snapshot explicitly (testDaemon skips snapshot)
	d2 := testDaemon(t)
	restored := d2.loadSnapshot()
	if restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
	}

	// Verify owner was restored
	d2.mu.RLock()
	ownerCount := len(d2.owners)
	var restoredEntry *OwnerEntry
	for _, e := range d2.owners {
		restoredEntry = e
		break
	}
	d2.mu.RUnlock()

	if ownerCount != 1 {
		t.Fatalf("daemon has %d owners after snapshot load, want 1", ownerCount)
	}
	if restoredEntry == nil || restoredEntry.Owner == nil {
		t.Fatal("restored owner is nil")
	}
	if restoredEntry.Command != "go" {
		t.Errorf("restored command = %q, want %q", restoredEntry.Command, "go")
	}

	// Verify snapshot file was consumed (deleted)
	if _, err := os.Stat(SnapshotPath()); !os.IsNotExist(err) {
		t.Error("snapshot file should be consumed after load")
	}

	// Verify restored owner has InitSuccess (cached init from snapshot)
	if !restoredEntry.Owner.InitSuccess() {
		t.Error("restored owner should have InitSuccess=true from snapshot cache")
	}

	d2.Shutdown()
}

func TestLoadSnapshot_HealsIsolatedOwnerCwdSet(t *testing.T) {
	os.Remove(SnapshotPath())

	primaryCwd := t.TempDir()
	otherCwd := t.TempDir()
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:             "abc12345-heal-test",
				Command:              "uvx",
				Args:                 []string{"--from", "serena"},
				Cwd:                  primaryCwd,
				CwdSet:               []string{primaryCwd, otherCwd},
				Mode:                 "cwd",
				Classification:       classify.ModeIsolated,
				ClassificationSource: "tools",
			},
		},
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}

	path := SnapshotPath()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	d := testDaemon(t)
	restored := d.loadSnapshot()
	if restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
	}

	d.mu.RLock()
	entry := d.owners["abc12345-heal-test"]
	d.mu.RUnlock()
	if entry == nil || entry.Owner == nil {
		t.Fatal("restored owner is nil")
	}

	status := entry.Owner.Status()
	cwdSet, ok := status["cwd_set"].([]string)
	if !ok {
		t.Fatalf("cwd_set not []string in status: %T", status["cwd_set"])
	}
	if len(cwdSet) != 1 {
		t.Fatalf("cwd_set size = %d, want 1 (%v)", len(cwdSet), cwdSet)
	}
	if cwdSet[0] != primaryCwd {
		t.Fatalf("cwd_set[0] = %q, want %q", cwdSet[0], primaryCwd)
	}
}

func TestMakeSnapshotRestorePlanPreservesPinnedEnvironment(t *testing.T) {
	t.Setenv("MCPMUX_TEST_SUCCESSOR_ONLY_ENV", "must-not-leak")
	d := testDaemon(t)
	snap := daemonMaterializationSnapshot(false)
	snap.Env = map[string]string{
		"CONFIG_PATH":  "captured-config",
		"GITHUB_TOKEN": "captured-token",
	}

	plan := d.makeSnapshotRestorePlan(snap)
	if len(plan.env) != len(snap.Env) {
		t.Fatalf("restored env size=%d, want exact captured size %d", len(plan.env), len(snap.Env))
	}
	if _, leaked := plan.env["MCPMUX_TEST_SUCCESSOR_ONLY_ENV"]; leaked {
		t.Fatal("successor daemon environment leaked into pinned snapshot launch context")
	}
	for key, want := range snap.Env {
		if got := plan.env[key]; got != want {
			t.Fatalf("restored env[%q]=%q, want %q", key, got, want)
		}
	}
	plan.env["CONFIG_PATH"] = "mutated-plan"
	if snap.Env["CONFIG_PATH"] != "captured-config" {
		t.Fatal("restore plan aliases the snapshot environment map")
	}
}

func TestActivateRestartStagingFailureRollsBackAndPreservesRetry(t *testing.T) {
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })
	t.Setenv(snapshotRestartEnv, "1")
	d := testDaemon(t)
	d.handlerFunc = func(_ context.Context, stdin io.Reader, _ io.Writer) error {
		_, err := io.Copy(io.Discard, stdin)
		return err
	}

	makeSnapshot := func(serverID, command string) mcpsnapshot.OwnerSnapshot {
		snap := daemonMaterializationSnapshot(false)
		snap.ServerID = serverID
		snap.Command = command
		snap.Cwd = t.TempDir()
		snap.Mode = "global"
		snap.Env = map[string]string{"CONFIG_PATH": serverID}
		return snap
	}
	first := makeSnapshot("restart-stage-first", "restart-stage-first-command")
	second := makeSnapshot("restart-stage-second", "restart-stage-second-command")
	plans := []snapshotRestorePlan{d.makeSnapshotRestorePlan(first), d.makeSnapshotRestorePlan(second)}
	base := &DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Owners:    []mcpsnapshot.OwnerSnapshot{first, second},
		Sessions: []mcpsnapshot.SessionSnapshot{
			{MuxSessionID: "first-session", OwnerServerID: first.ServerID},
			{MuxSessionID: "second-session", OwnerServerID: second.ServerID},
		},
	}
	d.mu.Lock()
	d.restartStaging = append([]snapshotRestorePlan(nil), plans...)
	d.restartRecoverySnapshot = buildRestartRecoverySnapshot(base, plans)
	d.mu.Unlock()

	originalGenerateToken := generateTokenFunc
	calls := 0
	generateTokenFunc = func() (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("synthetic second-owner generation failure")
		}
		return originalGenerateToken()
	}
	t.Cleanup(func() { generateTokenFunc = originalGenerateToken })

	activated, err := d.activateRestartStaging()
	if err == nil || activated != 0 {
		t.Fatalf("activateRestartStaging()=(%d, %v), want transactional failure", activated, err)
	}
	if d.OwnerCount() != 0 {
		t.Fatalf("partial staged activation left %d owners registered", d.OwnerCount())
	}
	d.mu.RLock()
	retainedPlans := len(d.restartStaging)
	retainedRecovery := d.restartRecoverySnapshot != nil
	d.mu.RUnlock()
	if retainedPlans != 2 || !retainedRecovery {
		t.Fatalf("failed activation retained plans=%d recovery=%v, want 2/true", retainedPlans, retainedRecovery)
	}

	path, err := d.rewriteRestartRecoverySnapshot()
	if err != nil || path == "" {
		t.Fatalf("rewriteRestartRecoverySnapshot()=(%q, %v)", path, err)
	}
	recovered, err := DeserializeSnapshot(d.logger)
	if err != nil || recovered == nil {
		t.Fatalf("DeserializeSnapshot()=(%v, %v)", recovered, err)
	}
	if len(recovered.Owners) != 2 || len(recovered.Sessions) != 2 {
		t.Fatalf("recovered snapshot owners/sessions=(%d, %d), want 2/2", len(recovered.Owners), len(recovered.Sessions))
	}

	generateTokenFunc = originalGenerateToken
	activated, err = d.activateRestartStaging()
	if err != nil || activated != 2 {
		t.Fatalf("retry activateRestartStaging()=(%d, %v), want 2/nil", activated, err)
	}
	if d.OwnerCount() != 2 {
		t.Fatalf("retry registered %d owners, want 2", d.OwnerCount())
	}
	d.mu.RLock()
	stagingCleared := len(d.restartStaging) == 0 && d.restartRecoverySnapshot == nil
	d.mu.RUnlock()
	if !stagingCleared {
		t.Fatal("successful activation did not clear retained restart recovery")
	}
}

func TestNewRestartActivationFailureRewritesRecoverySnapshot(t *testing.T) {
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })
	t.Setenv(snapshotRestartEnv, "1")
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", "")
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "")
	t.Setenv("MCPMUX_TEST_SUCCESSOR_ONLY_ENV", "must-not-leak")
	originalDelay := snapshotRestartControlBindDelay
	snapshotRestartControlBindDelay = 0
	t.Cleanup(func() { snapshotRestartControlBindDelay = originalDelay })

	ownerSnap := daemonMaterializationSnapshot(false)
	ownerSnap.ServerID = "restart-constructor-recovery"
	ownerSnap.Command = "restart-constructor-command"
	ownerSnap.Cwd = t.TempDir()
	ownerSnap.Mode = "global"
	ownerSnap.Env = map[string]string{"CONFIG_PATH": "captured-only"}
	_, err := mcpsnapshot.Serialize(&DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Owners:    []mcpsnapshot.OwnerSnapshot{ownerSnap},
	}, testLogger(t))
	if err != nil {
		t.Fatalf("Serialize recovery fixture: %v", err)
	}

	originalGenerateToken := generateTokenFunc
	calls := 0
	generateTokenFunc = func() (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("synthetic restart activation failure")
		}
		return originalGenerateToken()
	}
	t.Cleanup(func() { generateTokenFunc = originalGenerateToken })

	d, err := New(Config{
		Name:        "restart-recovery-test",
		ControlPath: shortSocketPath(t, "restart-recovery.ctl.sock"),
		GracePeriod: time.Second,
		IdleTimeout: 5 * time.Second,
		Logger:      testLogger(t),
	})
	if d != nil {
		d.Shutdown()
	}
	if err == nil || !strings.Contains(err.Error(), "activate restart staging") {
		t.Fatalf("New() error=%v, want restart activation failure", err)
	}

	generateTokenFunc = originalGenerateToken
	recovered, err := DeserializeSnapshot(testLogger(t))
	if err != nil || recovered == nil || len(recovered.Owners) != 1 {
		t.Fatalf("rewritten recovery snapshot=(%v, %v)", recovered, err)
	}
	if got := recovered.Owners[0].Env; len(got) != 1 || got["CONFIG_PATH"] != "captured-only" {
		t.Fatalf("rewritten recovery env=%v, want exact pinned context", got)
	}
	if _, leaked := recovered.Owners[0].Env["MCPMUX_TEST_SUCCESSOR_ONLY_ENV"]; leaked {
		t.Fatal("rewritten recovery snapshot captured successor-only environment")
	}
}
