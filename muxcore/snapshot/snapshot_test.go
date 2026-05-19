package snapshot_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestOwnerSnapshotOldRoundTrip verifies that a v0.20.x snapshot (no handoff fields)
// deserializes correctly and re-marshals without emitting the new fields.
func TestOwnerSnapshotOldRoundTrip(t *testing.T) {
	// Simulate a v0.20.x snapshot JSON that has NO handoff fields.
	oldJSON := []byte(`{"server_id":"srv-001","command":"uvx","args":["--from","serena"],"cwd":"/dev/project","cwd_set":["/dev/project"],"mode":"cwd"}`)

	var got snapshot.OwnerSnapshot
	if err := json.Unmarshal(oldJSON, &got); err != nil {
		t.Fatalf("Unmarshal old snapshot: %v", err)
	}

	// New fields must default to zero-values.
	if got.UpstreamPID != 0 {
		t.Errorf("UpstreamPID = %d, want 0", got.UpstreamPID)
	}
	if got.HandoffSocketPath != "" {
		t.Errorf("HandoffSocketPath = %q, want empty", got.HandoffSocketPath)
	}
	if got.SpawnPgid != 0 {
		t.Errorf("SpawnPgid = %d, want 0", got.SpawnPgid)
	}
	if got.OwnerGeneration != "" {
		t.Errorf("OwnerGeneration = %q, want empty", got.OwnerGeneration)
	}
	if got.RestoredFromGeneration != "" {
		t.Errorf("RestoredFromGeneration = %q, want empty", got.RestoredFromGeneration)
	}
	if got.RestoreSource != "" {
		t.Errorf("RestoreSource = %q, want empty", got.RestoreSource)
	}

	// Re-marshal: omitempty must suppress zero-value new fields.
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	outStr := string(out)
	if strings.Contains(outStr, "upstream_pid") {
		t.Errorf("re-marshaled output must NOT contain upstream_pid when zero, got: %s", outStr)
	}
	if strings.Contains(outStr, "handoff_socket_path") {
		t.Errorf("re-marshaled output must NOT contain handoff_socket_path when empty, got: %s", outStr)
	}
	if strings.Contains(outStr, "spawn_pgid") {
		t.Errorf("re-marshaled output must NOT contain spawn_pgid when zero, got: %s", outStr)
	}
	if strings.Contains(outStr, "owner_generation") {
		t.Errorf("re-marshaled output must NOT contain owner_generation when empty, got: %s", outStr)
	}
	if strings.Contains(outStr, "restored_from_owner_generation") {
		t.Errorf("re-marshaled output must NOT contain restored_from_owner_generation when empty, got: %s", outStr)
	}
	if strings.Contains(outStr, "restore_source") {
		t.Errorf("re-marshaled output must NOT contain restore_source when empty, got: %s", outStr)
	}
}

// TestOwnerSnapshotNewRoundTrip verifies that new handoff fields marshal and
// unmarshal correctly when populated.
func TestOwnerSnapshotNewRoundTrip(t *testing.T) {
	original := snapshot.OwnerSnapshot{
		ServerID:               "srv-002",
		Command:                "aimux",
		Cwd:                    "/dev/aimux",
		CwdSet:                 []string{"/dev/aimux"},
		Mode:                   "shared",
		OwnerGeneration:        "owner-gen-new",
		RestoredFromGeneration: "owner-gen-old",
		RestoreSource:          "snapshot_handoff",
		UpstreamPID:            12345,
		HandoffSocketPath:      "/tmp/mcp-mux-handoff.sock",
		SpawnPgid:              12345,
	}

	out, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	outStr := string(out)

	// New fields must appear in the marshaled output.
	if !strings.Contains(outStr, "upstream_pid") {
		t.Errorf("marshaled output must contain upstream_pid, got: %s", outStr)
	}
	if !strings.Contains(outStr, "handoff_socket_path") {
		t.Errorf("marshaled output must contain handoff_socket_path, got: %s", outStr)
	}
	if !strings.Contains(outStr, "spawn_pgid") {
		t.Errorf("marshaled output must contain spawn_pgid, got: %s", outStr)
	}
	if !strings.Contains(outStr, "owner_generation") {
		t.Errorf("marshaled output must contain owner_generation, got: %s", outStr)
	}
	if !strings.Contains(outStr, "restored_from_owner_generation") {
		t.Errorf("marshaled output must contain restored_from_owner_generation, got: %s", outStr)
	}
	if !strings.Contains(outStr, "restore_source") {
		t.Errorf("marshaled output must contain restore_source, got: %s", outStr)
	}

	// Round-trip: unmarshal back and verify all three fields match.
	var got snapshot.OwnerSnapshot
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.UpstreamPID != 12345 {
		t.Errorf("UpstreamPID = %d, want 12345", got.UpstreamPID)
	}
	if got.HandoffSocketPath != "/tmp/mcp-mux-handoff.sock" {
		t.Errorf("HandoffSocketPath = %q, want %q", got.HandoffSocketPath, "/tmp/mcp-mux-handoff.sock")
	}
	if got.SpawnPgid != 12345 {
		t.Errorf("SpawnPgid = %d, want 12345", got.SpawnPgid)
	}
	if got.OwnerGeneration != "owner-gen-new" {
		t.Errorf("OwnerGeneration = %q, want owner-gen-new", got.OwnerGeneration)
	}
	if got.RestoredFromGeneration != "owner-gen-old" {
		t.Errorf("RestoredFromGeneration = %q, want owner-gen-old", got.RestoredFromGeneration)
	}
	if got.RestoreSource != "snapshot_handoff" {
		t.Errorf("RestoreSource = %q, want snapshot_handoff", got.RestoreSource)
	}
}

func TestDaemonSnapshotGenerationRoundTrip(t *testing.T) {
	original := snapshot.DaemonSnapshot{
		Version:          snapshot.SnapshotVersion,
		MuxVersion:       "test",
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		DaemonGeneration: "daemon-gen-old",
		PredecessorPID:   4242,
		Owners:           []snapshot.OwnerSnapshot{},
		Sessions:         []snapshot.SessionSnapshot{},
	}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	outStr := string(raw)
	if !strings.Contains(outStr, "daemon_generation") {
		t.Errorf("marshaled output must contain daemon_generation, got: %s", outStr)
	}
	if !strings.Contains(outStr, "predecessor_pid") {
		t.Errorf("marshaled output must contain predecessor_pid, got: %s", outStr)
	}

	var got snapshot.DaemonSnapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.DaemonGeneration != "daemon-gen-old" {
		t.Errorf("DaemonGeneration = %q, want daemon-gen-old", got.DaemonGeneration)
	}
	if got.PredecessorPID != 4242 {
		t.Errorf("PredecessorPID = %d, want 4242", got.PredecessorPID)
	}

	oldJSON := []byte(`{"version":1,"mux_version":"old","timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `","owners":[],"sessions":[]}`)
	var old snapshot.DaemonSnapshot
	if err := json.Unmarshal(oldJSON, &old); err != nil {
		t.Fatalf("Unmarshal old daemon snapshot: %v", err)
	}
	if old.DaemonGeneration != "" {
		t.Errorf("old DaemonGeneration = %q, want empty", old.DaemonGeneration)
	}
	if old.PredecessorPID != 0 {
		t.Errorf("old PredecessorPID = %d, want 0", old.PredecessorPID)
	}
}
