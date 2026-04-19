package daemon

import (
	"path/filepath"
	"testing"
)

// TestWriteReadDeleteHandoffToken_PublicAPI verifies the token lifecycle via the
// public API: Write → Read → Delete → Delete (idempotent).
func TestWriteReadDeleteHandoffToken_PublicAPI(t *testing.T) {
	dir := t.TempDir()

	token, path, err := WriteHandoffToken(dir)
	if err != nil {
		t.Fatalf("WriteHandoffToken: %v", err)
	}
	if token == "" {
		t.Fatal("WriteHandoffToken: returned empty token")
	}
	if path == "" {
		t.Fatal("WriteHandoffToken: returned empty path")
	}
	// path must be inside dir
	if filepath.Dir(path) != dir {
		t.Errorf("WriteHandoffToken path %q not in dir %q", path, dir)
	}

	got, err := ReadHandoffToken(path)
	if err != nil {
		t.Fatalf("ReadHandoffToken: %v", err)
	}
	if got != token {
		t.Errorf("ReadHandoffToken: got %q, want %q", got, token)
	}

	if err := DeleteHandoffToken(path); err != nil {
		t.Fatalf("DeleteHandoffToken: %v", err)
	}
	// Idempotent: second delete must NOT error.
	if err := DeleteHandoffToken(path); err != nil {
		t.Fatalf("DeleteHandoffToken idempotent second call: %v", err)
	}
}
