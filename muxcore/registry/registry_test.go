package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func testDescriptor(name, controlPath string) Descriptor {
	return Descriptor{
		SchemaVersion:     SchemaVersion,
		EngineName:        name,
		ProductName:       name + " product",
		PID:               1234,
		BaseDir:           filepath.Dir(controlPath),
		DaemonControlPath: controlPath,
		StartedAt:         time.Unix(1700000000, 0).UTC(),
		MuxcoreVersion:    "test",
		Capabilities:      Capabilities{ListOwners: true},
	}
}

func TestDescriptorPathStableSafeAndUniqueByControlPath(t *testing.T) {
	base := t.TempDir()
	d1 := testDescriptor("aimux/native", filepath.Join(base, "aimux-one.sock"))
	d2 := testDescriptor("aimux/native", filepath.Join(base, "aimux-two.sock"))

	p1, err := DescriptorPath(base, d1)
	if err != nil {
		t.Fatalf("DescriptorPath d1: %v", err)
	}
	p1Again, err := DescriptorPath(base, d1)
	if err != nil {
		t.Fatalf("DescriptorPath d1 again: %v", err)
	}
	p2, err := DescriptorPath(base, d2)
	if err != nil {
		t.Fatalf("DescriptorPath d2: %v", err)
	}

	if p1 != p1Again {
		t.Fatalf("path is not stable: %q != %q", p1, p1Again)
	}
	if p1 == p2 {
		t.Fatalf("different control paths produced same descriptor path: %q", p1)
	}
	if filepath.Dir(p1) != Dir(base) {
		t.Fatalf("descriptor dir = %q, want %q", filepath.Dir(p1), Dir(base))
	}
	if strings.Contains(filepath.Base(p1), "/") || strings.Contains(filepath.Base(p1), "\\") {
		t.Fatalf("descriptor filename contains path separator: %q", filepath.Base(p1))
	}
}

func TestWriteReadListDescriptorsRoundTrip(t *testing.T) {
	base := t.TempDir()
	want := testDescriptor("aimux", filepath.Join(base, "aimux.sock"))

	path, err := WriteDescriptor(base, want)
	if err != nil {
		t.Fatalf("WriteDescriptor: %v", err)
	}
	got, err := ReadDescriptor(path)
	if err != nil {
		t.Fatalf("ReadDescriptor: %v", err)
	}
	if got.EngineName != want.EngineName || got.DaemonControlPath != want.DaemonControlPath || !got.Capabilities.ListOwners {
		t.Fatalf("descriptor roundtrip mismatch: got %+v want %+v", got, want)
	}

	records, err := ListDescriptors(base)
	if err != nil {
		t.Fatalf("ListDescriptors: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("ListDescriptors returned %d records, want 1: %+v", len(records), records)
	}
	if records[0].Err != nil {
		t.Fatalf("record has unexpected parse error: %v", records[0].Err)
	}
	if records[0].Path != path {
		t.Fatalf("record path = %q, want %q", records[0].Path, path)
	}
}

func TestListDescriptorsReturnsMalformedRecords(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(Dir(base), 0o700); err != nil {
		t.Fatalf("mkdir registry dir: %v", err)
	}
	badPath := filepath.Join(Dir(base), "bad.json")
	if err := os.WriteFile(badPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write malformed descriptor: %v", err)
	}

	records, err := ListDescriptors(base)
	if err != nil {
		t.Fatalf("ListDescriptors: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].Err == nil {
		t.Fatalf("malformed descriptor did not return an error record")
	}
	verified := VerifyDescriptorWithSender(records[0], func(string, control.Request) (*control.Response, error) {
		t.Fatal("sender should not be called for malformed descriptor")
		return nil, nil
	})
	if verified.State != StateInvalid {
		t.Fatalf("state = %q, want %q", verified.State, StateInvalid)
	}
}

func TestReadDescriptorRejectsSchemaVersionZero(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(Dir(base), 0o700); err != nil {
		t.Fatalf("mkdir registry dir: %v", err)
	}
	path := filepath.Join(Dir(base), "schema-zero.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":0,"engine_name":"aimux","daemon_control_path":"aimux.sock"}`), 0o600); err != nil {
		t.Fatalf("write schema-zero descriptor: %v", err)
	}

	if _, err := ReadDescriptor(path); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("ReadDescriptor error = %v, want ErrInvalidDescriptor", err)
	}
}

func TestDuplicateEngineNamesAndResolveEngine(t *testing.T) {
	base := t.TempDir()
	d1 := testDescriptor("aimux", filepath.Join(base, "one.sock"))
	d2 := testDescriptor("aimux", filepath.Join(base, "two.sock"))
	if _, err := WriteDescriptor(base, d1); err != nil {
		t.Fatalf("WriteDescriptor d1: %v", err)
	}
	if _, err := WriteDescriptor(base, d2); err != nil {
		t.Fatalf("WriteDescriptor d2: %v", err)
	}
	records, err := ListDescriptors(base)
	if err != nil {
		t.Fatalf("ListDescriptors: %v", err)
	}
	dups := DuplicateEngineNames(records)
	if dups["aimux"] != 2 {
		t.Fatalf("duplicate count = %v, want aimux=2", dups)
	}
	if _, err := ResolveEngine(records, "aimux"); !errors.Is(err, ErrDuplicateEngine) {
		t.Fatalf("ResolveEngine duplicate err = %v, want ErrDuplicateEngine", err)
	}
	if _, err := ResolveEngine(records, "engram"); !errors.Is(err, ErrEngineNotFound) {
		t.Fatalf("ResolveEngine missing err = %v, want ErrEngineNotFound", err)
	}
}

func TestVerifyDescriptorStatusMismatchAndHealthy(t *testing.T) {
	base := t.TempDir()
	rec := Record{
		Path:       "aimux.json",
		Descriptor: testDescriptor("aimux", filepath.Join(base, "aimux.sock")),
	}

	statusData := func(engineName string, pid int) json.RawMessage {
		data, err := json.Marshal(map[string]any{
			"engine_name":       engineName,
			"pid":               pid,
			"daemon_generation": "daemon-test",
			"owner_count":       7,
		})
		if err != nil {
			t.Fatalf("marshal status: %v", err)
		}
		return data
	}

	mismatch := VerifyDescriptorWithSender(rec, func(path string, req control.Request) (*control.Response, error) {
		if path != rec.Descriptor.DaemonControlPath || req.Cmd != "status" {
			t.Fatalf("unexpected verify request path=%q req=%+v", path, req)
		}
		return &control.Response{OK: true, Data: statusData("engram", rec.Descriptor.PID)}, nil
	})
	if mismatch.State != StateStale || !strings.Contains(mismatch.Reason, "engine_name_mismatch") {
		t.Fatalf("mismatch verify = %+v, want stale engine mismatch", mismatch)
	}

	pidMismatch := VerifyDescriptorWithSender(rec, func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true, Data: statusData("aimux", rec.Descriptor.PID+1)}, nil
	})
	if pidMismatch.State != StateStale || !strings.Contains(pidMismatch.Reason, "pid_mismatch") {
		t.Fatalf("pid mismatch verify = %+v, want stale pid mismatch", pidMismatch)
	}

	healthy := VerifyDescriptorWithSender(rec, func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true, Data: statusData("aimux", rec.Descriptor.PID)}, nil
	})
	if healthy.State != StateHealthy || !healthy.Reachable {
		t.Fatalf("healthy verify = %+v, want healthy reachable", healthy)
	}
	if healthy.PID != rec.Descriptor.PID || healthy.OwnerCount != 7 || healthy.DaemonGeneration != "daemon-test" {
		t.Fatalf("healthy status fields = %+v", healthy)
	}
}

func TestRemoveDescriptorIfOwnedSkipsSuccessorDescriptor(t *testing.T) {
	base := t.TempDir()
	oldDesc := testDescriptor("aimux", filepath.Join(base, "aimux.sock"))
	oldDesc.PID = 1111
	path, err := WriteDescriptor(base, oldDesc)
	if err != nil {
		t.Fatalf("WriteDescriptor old: %v", err)
	}

	successor := oldDesc
	successor.PID = 2222
	if successorPath, err := WriteDescriptor(base, successor); err != nil {
		t.Fatalf("WriteDescriptor successor: %v", err)
	} else if successorPath != path {
		t.Fatalf("successor descriptor path = %q, want %q", successorPath, path)
	}

	removed, err := RemoveDescriptorIfOwned(path, oldDesc)
	if err != nil {
		t.Fatalf("RemoveDescriptorIfOwned old: %v", err)
	}
	if removed {
		t.Fatal("old descriptor cleanup removed successor descriptor")
	}
	got, err := ReadDescriptor(path)
	if err != nil {
		t.Fatalf("ReadDescriptor after skipped remove: %v", err)
	}
	if got.PID != successor.PID {
		t.Fatalf("descriptor PID = %d, want successor PID %d", got.PID, successor.PID)
	}

	removed, err = RemoveDescriptorIfOwned(path, successor)
	if err != nil {
		t.Fatalf("RemoveDescriptorIfOwned successor: %v", err)
	}
	if !removed {
		t.Fatal("successor descriptor was not removed by matching cleanup")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descriptor still exists after matching cleanup: %v", err)
	}
}

func TestVerifyDescriptorUnreachableIsStale(t *testing.T) {
	rec := Record{Descriptor: testDescriptor("aimux", "missing.sock")}
	verified := VerifyDescriptorWithSender(rec, func(string, control.Request) (*control.Response, error) {
		return nil, errors.New("dial failed")
	})
	if verified.State != StateStale || !strings.Contains(verified.Reason, "control_unreachable") {
		t.Fatalf("verify = %+v, want stale unreachable", verified)
	}
}

func TestVerifyDescriptorEmptyStatusDataIsStale(t *testing.T) {
	rec := Record{Descriptor: testDescriptor("aimux", "empty-status.sock")}
	verified := VerifyDescriptorWithSender(rec, func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	})
	if verified.State != StateStale || verified.Reason != "status_empty_response_data" {
		t.Fatalf("verify = %+v, want stale empty response data", verified)
	}
}
