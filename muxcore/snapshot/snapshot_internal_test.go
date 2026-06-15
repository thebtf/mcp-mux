package snapshot

import (
	"io/fs"
	"os"
	"testing"
	"time"
)

type internalTestLogger struct{ t *testing.T }

func (l internalTestLogger) Printf(format string, args ...any) { l.t.Logf(format, args...) }

func TestSerializeRetriesTransientRenameFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)

	origRename := snapshotRename
	origDelay := snapshotRenameRetryDelay
	attempts := 0
	snapshotRenameRetryDelay = 0
	snapshotRename = func(oldpath, newpath string) error {
		attempts++
		if attempts == 1 {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrPermission}
		}
		return origRename(oldpath, newpath)
	}
	t.Cleanup(func() {
		snapshotRename = origRename
		snapshotRenameRetryDelay = origDelay
	})

	path, err := Serialize(&DaemonSnapshot{
		Version:   SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners:    []OwnerSnapshot{},
		Sessions:  []SessionSnapshot{},
	}, internalTestLogger{t: t})
	if err != nil {
		t.Fatalf("Serialize() error = %v, want nil after transient rename failure", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if attempts != 2 {
		t.Fatalf("rename attempts = %d, want 2", attempts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot path missing after retry: %v", err)
	}
}

func TestSerializeFallsBackToDirectWriteAfterPersistentRenameFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)

	origRename := snapshotRename
	origDelay := snapshotRenameRetryDelay
	attempts := 0
	snapshotRenameRetryDelay = 0
	snapshotRename = func(oldpath, newpath string) error {
		attempts++
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrPermission}
	}
	t.Cleanup(func() {
		snapshotRename = origRename
		snapshotRenameRetryDelay = origDelay
	})

	path, err := Serialize(&DaemonSnapshot{
		Version:   SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners:    []OwnerSnapshot{},
		Sessions:  []SessionSnapshot{},
	}, internalTestLogger{t: t})
	if err != nil {
		t.Fatalf("Serialize() error = %v, want nil after direct-write fallback", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if attempts != snapshotRenameMaxAttempts {
		t.Fatalf("rename attempts = %d, want %d", attempts, snapshotRenameMaxAttempts)
	}
	if got, err := Deserialize(internalTestLogger{t: t}); err != nil || got == nil {
		t.Fatalf("Deserialize() after direct-write fallback = (%v, %v), want snapshot", got, err)
	}
}
