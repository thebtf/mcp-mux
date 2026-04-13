package snapshot_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/snapshot"
)

// testLogger wraps testing.T to satisfy the logger interface.
type testLogger struct{ t *testing.T }

func (l *testLogger) Printf(format string, args ...any) { l.t.Logf(format, args...) }

func newLogger(t *testing.T) *testLogger { return &testLogger{t: t} }

// TestSnapshotPathDefault verifies SnapshotPath with empty baseDir uses os.TempDir.
func TestSnapshotPathDefault(t *testing.T) {
	path := snapshot.SnapshotPath("")
	expected := filepath.Join(os.TempDir(), snapshot.SnapshotFileName)
	if path != expected {
		t.Errorf("SnapshotPath(\"\") = %q, want %q", path, expected)
	}
}

// TestSnapshotPathCustomBaseDir verifies SnapshotPath respects a custom baseDir.
func TestSnapshotPathCustomBaseDir(t *testing.T) {
	dir := t.TempDir()
	path := snapshot.SnapshotPath(dir)
	expected := filepath.Join(dir, snapshot.SnapshotFileName)
	if path != expected {
		t.Errorf("SnapshotPath(%q) = %q, want %q", dir, path, expected)
	}
}

// TestSerializeDeserializeRoundTrip exercises the full serialize → deserialize path.
func TestSerializeDeserializeRoundTrip(t *testing.T) {
	// Ensure no leftover snapshot from prior runs.
	os.Remove(snapshot.SnapshotPath(""))

	data := &snapshot.DaemonSnapshot{
		Version:    snapshot.SnapshotVersion,
		MuxVersion: "test",
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Owners: []snapshot.OwnerSnapshot{
			{
				ServerID: "srv-001",
				Command:  "uvx",
				Args:     []string{"--from", "serena"},
				Cwd:      "/dev/project",
				CwdSet:   []string{"/dev/project"},
				Mode:     "cwd",
			},
		},
		Sessions: []snapshot.SessionSnapshot{
			{
				MuxSessionID:  "sess_001",
				Cwd:           "/dev/project",
				OwnerServerID: "srv-001",
			},
		},
	}

	path, err := snapshot.Serialize(data, newLogger(t))
	if err != nil {
		t.Fatalf("Serialize() error: %v", err)
	}
	defer os.Remove(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not found at %s after Serialize()", path)
	}

	got, err := snapshot.Deserialize(newLogger(t))
	if err != nil {
		t.Fatalf("Deserialize() error: %v", err)
	}
	if got == nil {
		t.Fatal("Deserialize() returned nil, want *DaemonSnapshot")
	}

	if got.Version != snapshot.SnapshotVersion {
		t.Errorf("version = %d, want %d", got.Version, snapshot.SnapshotVersion)
	}
	if len(got.Owners) != 1 {
		t.Fatalf("owners count = %d, want 1", len(got.Owners))
	}
	if got.Owners[0].Command != "uvx" {
		t.Errorf("owner command = %q, want %q", got.Owners[0].Command, "uvx")
	}
	if len(got.Sessions) != 1 {
		t.Errorf("sessions count = %d, want 1", len(got.Sessions))
	}

	// Deserialize must consume (delete) the file.
	if _, err := os.Stat(snapshot.SnapshotPath("")); !os.IsNotExist(err) {
		t.Error("snapshot file should be deleted after successful Deserialize()")
	}
}

// TestDeserializeCorruptJSON verifies corrupt JSON is silently dropped.
func TestDeserializeCorruptJSON(t *testing.T) {
	path := snapshot.SnapshotPath("")
	if err := os.WriteFile(path, []byte("{invalid json!!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, err := snapshot.Deserialize(newLogger(t))
	if err != nil {
		t.Fatalf("Deserialize() should not return error for corrupt JSON: %v", err)
	}
	if got != nil {
		t.Error("Deserialize() should return nil for corrupt JSON")
	}

	// File should be deleted.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("corrupt snapshot file should be deleted after Deserialize()")
	}
}

// TestDeserializeStaleTimestamp verifies stale snapshots are rejected.
func TestDeserializeStaleTimestamp(t *testing.T) {
	stale := snapshot.DaemonSnapshot{
		Version:   snapshot.SnapshotVersion,
		Timestamp: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
		Owners:    []snapshot.OwnerSnapshot{},
		Sessions:  []snapshot.SessionSnapshot{},
	}
	raw, _ := json.Marshal(stale)

	path := snapshot.SnapshotPath("")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, err := snapshot.Deserialize(newLogger(t))
	if err != nil {
		t.Fatalf("Deserialize() error: %v", err)
	}
	if got != nil {
		t.Error("Deserialize() should return nil for stale snapshot")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("stale snapshot file should be deleted")
	}
}

// TestDeserializeVersionMismatch verifies mismatched version is rejected.
func TestDeserializeVersionMismatch(t *testing.T) {
	future := snapshot.DaemonSnapshot{
		Version:   999,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners:    []snapshot.OwnerSnapshot{},
		Sessions:  []snapshot.SessionSnapshot{},
	}
	raw, _ := json.Marshal(future)

	path := snapshot.SnapshotPath("")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, err := snapshot.Deserialize(newLogger(t))
	if err != nil {
		t.Fatalf("Deserialize() error: %v", err)
	}
	if got != nil {
		t.Error("Deserialize() should return nil for version mismatch")
	}
}

// TestDeserializeMissingFile verifies cold start returns nil without error.
func TestDeserializeMissingFile(t *testing.T) {
	os.Remove(snapshot.SnapshotPath(""))

	got, err := snapshot.Deserialize(newLogger(t))
	if err != nil {
		t.Fatalf("Deserialize() error for missing file: %v", err)
	}
	if got != nil {
		t.Error("Deserialize() should return nil for missing file (cold start)")
	}
}

// TestSerializeAtomicWrite verifies no temp files remain after Serialize.
func TestSerializeAtomicWrite(t *testing.T) {
	os.Remove(snapshot.SnapshotPath(""))

	data := &snapshot.DaemonSnapshot{
		Version:   snapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners:    []snapshot.OwnerSnapshot{},
		Sessions:  []snapshot.SessionSnapshot{},
	}

	path, err := snapshot.Serialize(data, newLogger(t))
	if err != nil {
		t.Fatalf("Serialize() error: %v", err)
	}
	defer os.Remove(path)

	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "mcp-muxd-snapshot-*.tmp"))
	if len(matches) > 0 {
		t.Errorf("temp files remaining after Serialize(): %v", matches)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap snapshot.DaemonSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Errorf("snapshot is not valid JSON: %v", err)
	}
}

// TestDeserializeOwnerWithClassification verifies classification fields survive round-trip.
func TestDeserializeOwnerWithClassification(t *testing.T) {
	valid := snapshot.DaemonSnapshot{
		Version:   snapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []snapshot.OwnerSnapshot{
			{
				ServerID:             "abc123",
				Command:              "uvx",
				Args:                 []string{"--from", "serena"},
				Cwd:                  "/dev/project",
				CwdSet:               []string{"/dev/project", "/dev/other"},
				Mode:                 "cwd",
				Classification:       classify.ModeIsolated,
				ClassificationSource: "capability",
				CachedInit:           "eyJqc29ucnBjIjoiMi4wIn0=",
			},
		},
		Sessions: []snapshot.SessionSnapshot{
			{
				MuxSessionID:  "sess_12345678",
				Cwd:           "/dev/project",
				OwnerServerID: "abc123",
			},
		},
	}
	raw, _ := json.Marshal(valid)

	path := snapshot.SnapshotPath("")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, err := snapshot.Deserialize(newLogger(t))
	if err != nil {
		t.Fatalf("Deserialize() error: %v", err)
	}
	if got == nil {
		t.Fatal("snapshot should load successfully")
	}
	if len(got.Owners) != 1 {
		t.Fatalf("owners = %d, want 1", len(got.Owners))
	}
	owner := got.Owners[0]
	if owner.Classification != classify.ModeIsolated {
		t.Errorf("classification = %q, want %q", owner.Classification, classify.ModeIsolated)
	}
	if owner.CachedInit == "" {
		t.Error("cached_init should not be empty")
	}
	if len(got.Sessions) != 1 {
		t.Errorf("sessions = %d, want 1", len(got.Sessions))
	}
}
