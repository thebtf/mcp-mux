package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestInstallVersionedEngineDoesNotRenameLauncher(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	pendingPath := launcherPath + "~"
	writeTestFile(t, launcherPath, "stable launcher")
	writeTestFile(t, pendingPath, "engine v1")

	enginePath, installed, err := installVersionedEngine(launcherPath, pendingPath)
	if err != nil {
		t.Fatalf("installVersionedEngine() error = %v", err)
	}
	if !installed {
		t.Fatal("installVersionedEngine() installed = false, want true")
	}

	launcherBytes, err := os.ReadFile(launcherPath)
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	if string(launcherBytes) != "stable launcher" {
		t.Fatalf("launcher content changed to %q", launcherBytes)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending path still exists after install: %v", err)
	}

	activePath, ok := resolveActiveEngine(launcherPath)
	if !ok {
		t.Fatal("active engine pointer was not written")
	}
	if !samePath(activePath, enginePath) {
		t.Fatalf("active engine = %q, want %q", activePath, enginePath)
	}
	engineBytes, err := os.ReadFile(enginePath)
	if err != nil {
		t.Fatalf("read engine: %v", err)
	}
	if string(engineBytes) != "engine v1" {
		t.Fatalf("engine content = %q, want engine v1", engineBytes)
	}
}

func TestInstallVersionedEngineSwitchesPointerAndKeepsOldVersion(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	pendingPath := launcherPath + "~"
	writeTestFile(t, launcherPath, "stable launcher")
	writeTestFile(t, pendingPath, "engine v1")

	firstPath, _, err := installVersionedEngine(launcherPath, pendingPath)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	writeTestFile(t, pendingPath, "engine v2")
	secondPath, installed, err := installVersionedEngine(launcherPath, pendingPath)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !installed {
		t.Fatal("second install installed = false, want true")
	}
	if samePath(firstPath, secondPath) {
		t.Fatalf("second install reused first version path: %s", secondPath)
	}
	if _, err := os.Stat(firstPath); err != nil {
		t.Fatalf("old engine version should remain readable: %v", err)
	}
	activePath, ok := resolveActiveEngine(launcherPath)
	if !ok || !samePath(activePath, secondPath) {
		t.Fatalf("active engine = %q ok=%v, want %q", activePath, ok, secondPath)
	}
}

func TestWriteActiveEngineStoresRelativePointer(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(versionStoreDir(launcherPath), "abc123", engineFileName())
	writeTestFile(t, enginePath, "engine")

	if err := writeActiveEngine(launcherPath, enginePath); err != nil {
		t.Fatalf("writeActiveEngine() error = %v", err)
	}

	data, err := os.ReadFile(activeEngineFile(launcherPath))
	if err != nil {
		t.Fatalf("read active pointer: %v", err)
	}
	pointer := strings.TrimSpace(string(data))
	if filepath.IsAbs(pointer) {
		t.Fatalf("active pointer should be relative, got %q", pointer)
	}
	resolved, ok := resolveActiveEngine(launcherPath)
	if !ok || !samePath(resolved, enginePath) {
		t.Fatalf("resolved active engine = %q ok=%v, want %q", resolved, ok, enginePath)
	}
}
