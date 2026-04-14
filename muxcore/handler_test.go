package muxcore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectContextID_Deterministic(t *testing.T) {
	dir := t.TempDir()
	id1 := ProjectContextID(dir)
	id2 := ProjectContextID(dir)
	if id1 != id2 {
		t.Errorf("ProjectContextID is not deterministic: %q vs %q", id1, id2)
	}
}

func TestProjectContextID_DifferentCwd(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	id1 := ProjectContextID(dir1)
	id2 := ProjectContextID(dir2)
	if id1 == id2 {
		t.Errorf("different dirs produced same ID: %q", id1)
	}
}

func TestProjectContextID_SubdirSameAsRoot(t *testing.T) {
	dir := t.TempDir()
	// Create a .git directory so WorktreeRoot identifies dir as the repo root.
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "src", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	idRoot := ProjectContextID(dir)
	idSub := ProjectContextID(sub)
	if idRoot != idSub {
		t.Errorf("subdir and root produced different IDs: root=%q sub=%q", idRoot, idSub)
	}
}

func TestProjectContextID_Length(t *testing.T) {
	dir := t.TempDir()
	id := ProjectContextID(dir)
	if len(id) != 16 {
		t.Errorf("ProjectContextID length = %d, want 16; got %q", len(id), id)
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("ProjectContextID contains non-hex character %q in %q", c, id)
		}
	}
}
