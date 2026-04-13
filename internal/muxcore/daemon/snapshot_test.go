package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/classify"
	"github.com/thebtf/mcp-mux/internal/muxcore/control"
	mcpsnapshot "github.com/thebtf/mcp-mux/internal/muxcore/snapshot"
)

func TestSnapshotRoundTrip(t *testing.T) {
	d := testDaemon(t)

	// Create a real owner via Spawn
	req := control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../../testdata/mock_server.go"},
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
		Args:    []string{"run", "../../../testdata/mock_server.go"},
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
