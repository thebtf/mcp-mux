//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveActiveEngineAcceptsSymlinkedTempAncestor(t *testing.T) {
	sandbox := t.TempDir()
	realTemp := filepath.Join(sandbox, "real-temp")
	if err := os.MkdirAll(realTemp, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalTemp := filepath.Join(sandbox, "temp-alias")
	if err := os.Symlink(realTemp, logicalTemp); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", logicalTemp)
	if got := os.TempDir(); got != logicalTemp {
		t.Fatalf("os.TempDir() = %q, want symlink spelling %q", got, logicalTemp)
	}

	launcherPath := filepath.Join(os.TempDir(), "mcp-mux-test", launcherFileName())
	enginePath := writeContentAddressedTestEngine(t, launcherPath, "active engine")
	if err := writeActiveEngine(launcherPath, enginePath); err != nil {
		t.Fatal(err)
	}
	resolved, ok := resolveActiveEngine(launcherPath)
	if !ok {
		t.Fatal("active engine beneath symlinked temp ancestor was rejected")
	}
	if resolved != enginePath {
		t.Fatalf("resolved engine = %q, want exact advertised spelling %q", resolved, enginePath)
	}
}

func TestResolveActiveEngineRejectsControlledSymlinks(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		root := t.TempDir()
		launcherPath := filepath.Join(root, launcherFileName())
		realStore := filepath.Join(root, "real-store")
		enginePath := filepath.Join(realStore, "version", engineFileName())
		writeTestFile(t, enginePath, "engine")
		if err := os.Symlink(realStore, versionStoreDir(launcherPath)); err != nil {
			t.Fatal(err)
		}
		advertised := filepath.Join(versionStoreDir(launcherPath), "version", engineFileName())
		if err := writeActiveEngine(launcherPath, advertised); err != nil {
			t.Fatal(err)
		}
		if resolved, ok := resolveActiveEngine(launcherPath); ok || resolved != "" {
			t.Fatalf("symlinked store resolved as %q", resolved)
		}
	})

	t.Run("version", func(t *testing.T) {
		root := t.TempDir()
		launcherPath := filepath.Join(root, launcherFileName())
		store := versionStoreDir(launcherPath)
		if err := os.MkdirAll(store, 0o755); err != nil {
			t.Fatal(err)
		}
		realVersion := filepath.Join(root, "real-version")
		writeTestFile(t, filepath.Join(realVersion, engineFileName()), "engine")
		if err := os.Symlink(realVersion, filepath.Join(store, "version")); err != nil {
			t.Fatal(err)
		}
		advertised := filepath.Join(store, "version", engineFileName())
		if err := writeActiveEngine(launcherPath, advertised); err != nil {
			t.Fatal(err)
		}
		if resolved, ok := resolveActiveEngine(launcherPath); ok || resolved != "" {
			t.Fatalf("symlinked version resolved as %q", resolved)
		}
	})

	t.Run("engine", func(t *testing.T) {
		root := t.TempDir()
		launcherPath := filepath.Join(root, launcherFileName())
		enginePath := filepath.Join(versionStoreDir(launcherPath), "version", engineFileName())
		realEngine := filepath.Join(root, "real-engine")
		writeTestFile(t, realEngine, "engine")
		if err := os.MkdirAll(filepath.Dir(enginePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realEngine, enginePath); err != nil {
			t.Fatal(err)
		}
		if err := writeActiveEngine(launcherPath, enginePath); err != nil {
			t.Fatal(err)
		}
		if resolved, ok := resolveActiveEngine(launcherPath); ok || resolved != "" {
			t.Fatalf("symlinked engine resolved as %q", resolved)
		}
	})
}

func TestResolveInstalledEngineRejectsNonFileComponents(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		root := t.TempDir()
		launcherPath := filepath.Join(root, launcherFileName())
		if err := os.WriteFile(versionStoreDir(launcherPath), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(versionStoreDir(launcherPath), "version", engineFileName())
		if resolved, ok := resolveInstalledEnginePath(launcherPath, candidate); ok || resolved != "" {
			t.Fatalf("non-directory store resolved as %q", resolved)
		}
	})

	t.Run("version", func(t *testing.T) {
		root := t.TempDir()
		launcherPath := filepath.Join(root, launcherFileName())
		versionPath := filepath.Join(versionStoreDir(launcherPath), "version")
		writeTestFile(t, versionPath, "not a directory")
		candidate := filepath.Join(versionPath, engineFileName())
		if resolved, ok := resolveInstalledEnginePath(launcherPath, candidate); ok || resolved != "" {
			t.Fatalf("non-directory version resolved as %q", resolved)
		}
	})

	t.Run("engine", func(t *testing.T) {
		root := t.TempDir()
		launcherPath := filepath.Join(root, launcherFileName())
		candidate := filepath.Join(versionStoreDir(launcherPath), "version", engineFileName())
		if err := os.MkdirAll(candidate, 0o755); err != nil {
			t.Fatal(err)
		}
		if resolved, ok := resolveInstalledEnginePath(launcherPath, candidate); ok || resolved != "" {
			t.Fatalf("non-regular engine resolved as %q", resolved)
		}
	})
}
