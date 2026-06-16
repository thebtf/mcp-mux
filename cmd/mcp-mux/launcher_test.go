package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRunEngineProcessStartFailureFallsBackToLauncher(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	missingEnginePath := filepath.Join(versionStoreDir(launcherPath), "missing", engineFileName())

	handled, code := runEngineProcess(launcherPath, missingEnginePath, []string{"status"})
	if handled {
		t.Fatal("runEngineProcess handled missing active engine, want launcher fallback")
	}
	if code != 0 {
		t.Fatalf("fallback code = %d, want 0", code)
	}
}

func TestDaemonStartReportsActiveAndFallbackStartFailures(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	missingEnginePath := filepath.Join(versionStoreDir(launcherPath), "missing", engineFileName())

	_, fallbackToCaller, err := startEngineOrStableLauncher(launcherPath, missingEnginePath, []string{"daemon"}, false, func(cmd *exec.Cmd) {})
	if fallbackToCaller {
		t.Fatal("daemon start requested caller fallback, want child-process fallback")
	}
	if err == nil {
		t.Fatal("daemon start with missing engine and launcher succeeded, want error")
	}
	if !strings.Contains(err.Error(), "start stable launcher fallback") {
		t.Fatalf("error = %q, want stable launcher fallback context", err)
	}
}

func TestRestartDaemonAfterEngineSwitchNoDaemonIsNoop(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(versionStoreDir(launcherPath), "abc123", engineFileName())

	oldIsDaemonRunning := launcherIsDaemonRunning
	oldStartDaemonProcessFrom := launcherStartDaemonProcessFrom
	oldWaitForDaemon := launcherWaitForDaemon
	t.Cleanup(func() {
		launcherIsDaemonRunning = oldIsDaemonRunning
		launcherStartDaemonProcessFrom = oldStartDaemonProcessFrom
		launcherWaitForDaemon = oldWaitForDaemon
	})

	startCalled := false
	waitCalled := false
	launcherIsDaemonRunning = func(string) bool { return false }
	launcherStartDaemonProcessFrom = func(string, string) error {
		startCalled = true
		return nil
	}
	launcherWaitForDaemon = func(string, time.Duration) error {
		waitCalled = true
		return nil
	}

	if err := restartDaemonAfterEngineSwitch(launcherPath, enginePath); err != nil {
		t.Fatalf("restartDaemonAfterEngineSwitch() error = %v, want nil", err)
	}
	if startCalled {
		t.Fatal("restartDaemonAfterEngineSwitch started a daemon when none was running")
	}
	if waitCalled {
		t.Fatal("restartDaemonAfterEngineSwitch waited for daemon readiness when none was running")
	}
}

func TestRunLauncherUpgradeRestartNoDaemonSucceeds(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	pendingPath := launcherPath + "~"
	writeTestFile(t, launcherPath, "stable launcher")
	writeTestFile(t, pendingPath, "engine v1")

	oldIsDaemonRunning := launcherIsDaemonRunning
	oldStartDaemonProcessFrom := launcherStartDaemonProcessFrom
	oldWaitForDaemon := launcherWaitForDaemon
	t.Cleanup(func() {
		launcherIsDaemonRunning = oldIsDaemonRunning
		launcherStartDaemonProcessFrom = oldStartDaemonProcessFrom
		launcherWaitForDaemon = oldWaitForDaemon
	})

	launcherIsDaemonRunning = func(string) bool { return false }
	launcherStartDaemonProcessFrom = func(string, string) error {
		t.Fatal("runLauncherUpgrade started a daemon when none was running")
		return nil
	}
	launcherWaitForDaemon = func(string, time.Duration) error {
		t.Fatal("runLauncherUpgrade waited for daemon readiness when none was running")
		return nil
	}

	if code := runLauncherUpgrade(launcherPath, []string{"--restart"}); code != 0 {
		t.Fatalf("runLauncherUpgrade(--restart) code = %d, want 0", code)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending path still exists after install: %v", err)
	}
	if _, ok := resolveActiveEngine(launcherPath); !ok {
		t.Fatal("active engine pointer was not written")
	}
}
