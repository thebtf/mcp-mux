package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
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

func TestInstallVersionedEngineUpdatesLauncher(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	pendingPath := launcherPath + "~"
	writeTestFile(t, launcherPath, "stable launcher")
	writeTestFile(t, pendingPath, "engine v1")

	enginePath, installed, launcherUpdated, err := installVersionedEngine(launcherPath, pendingPath)
	if err != nil {
		t.Fatalf("installVersionedEngine() error = %v", err)
	}
	if !installed {
		t.Fatal("installVersionedEngine() installed = false, want true")
	}
	if !launcherUpdated {
		t.Fatal("installVersionedEngine() launcherUpdated = false, want true")
	}

	launcherBytes, err := os.ReadFile(launcherPath)
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	if string(launcherBytes) != "engine v1" {
		t.Fatalf("launcher content = %q, want engine v1", launcherBytes)
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
	writeTestFile(t, launcherPath, "engine v1")
	writeTestFile(t, pendingPath, "engine v1")

	firstPath, _, _, err := installVersionedEngine(launcherPath, pendingPath)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	writeTestFile(t, pendingPath, "engine v2")
	secondPath, installed, _, err := installVersionedEngine(launcherPath, pendingPath)
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

func TestDaemonExecutableForSpawnUsesActiveEnginePointer(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	oldEnginePath := filepath.Join(versionStoreDir(launcherPath), "old123", engineFileName())
	activeEnginePath := filepath.Join(versionStoreDir(launcherPath), "new456", engineFileName())
	writeTestFile(t, oldEnginePath, "old engine")
	writeTestFile(t, activeEnginePath, "new engine")
	if err := writeActiveEngine(launcherPath, activeEnginePath); err != nil {
		t.Fatalf("writeActiveEngine() error = %v", err)
	}
	t.Setenv(envActiveEngineFile, activeEngineFile(launcherPath))

	got := daemonExecutableForSpawn(oldEnginePath)
	if !samePath(got, activeEnginePath) {
		t.Fatalf("daemonExecutableForSpawn() = %q, want active engine %q", got, activeEnginePath)
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

func TestRestartDaemonAfterEngineSwitchSendsSuccessorExe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(versionStoreDir(launcherPath), "abc123", engineFileName())

	oldIsDaemonRunning := launcherIsDaemonRunning
	oldStartDaemonProcessFrom := launcherStartDaemonProcessFrom
	oldWaitForDaemon := launcherWaitForDaemon
	oldWaitForDaemonExit := launcherWaitForDaemonExit
	oldControlSendWithTimeout := launcherControlSendWithTimeout
	t.Cleanup(func() {
		launcherIsDaemonRunning = oldIsDaemonRunning
		launcherStartDaemonProcessFrom = oldStartDaemonProcessFrom
		launcherWaitForDaemon = oldWaitForDaemon
		launcherWaitForDaemonExit = oldWaitForDaemonExit
		launcherControlSendWithTimeout = oldControlSendWithTimeout
	})

	var gotReq control.Request
	sendCalled := false
	exitCalled := false
	waitCalled := false
	startCalled := false
	launcherIsDaemonRunning = func(path string) bool {
		return path == serverid.DaemonControlPath("", engineName)
	}
	launcherControlSendWithTimeout = func(path string, req control.Request, timeout time.Duration) (*control.Response, error) {
		sendCalled = true
		gotReq = req
		if path != serverid.DaemonControlPath("", engineName) {
			t.Fatalf("control path = %q, want daemon control path", path)
		}
		if timeout != 60*time.Second {
			t.Fatalf("timeout = %s, want 60s", timeout)
		}
		return &control.Response{OK: true}, nil
	}
	launcherWaitForDaemonExit = func(path, prefix string) {
		exitCalled = true
		if path != serverid.DaemonControlPath("", engineName) {
			t.Fatalf("exit wait path = %q, want daemon control path", path)
		}
	}
	launcherWaitForDaemon = func(path string, timeout time.Duration) error {
		waitCalled = true
		return nil
	}
	launcherStartDaemonProcessFrom = func(string, string) error {
		startCalled = true
		return nil
	}

	if err := restartDaemonAfterEngineSwitch(launcherPath, enginePath); err != nil {
		t.Fatalf("restartDaemonAfterEngineSwitch() error = %v, want nil", err)
	}
	if !sendCalled {
		t.Fatal("graceful-restart request was not sent")
	}
	if gotReq.Cmd != "graceful-restart" {
		t.Fatalf("Cmd = %q, want graceful-restart", gotReq.Cmd)
	}
	if gotReq.DrainTimeoutMs != 30000 {
		t.Fatalf("DrainTimeoutMs = %d, want 30000", gotReq.DrainTimeoutMs)
	}
	if gotReq.SuccessorExe != enginePath {
		t.Fatalf("SuccessorExe = %q, want %q", gotReq.SuccessorExe, enginePath)
	}
	if !exitCalled {
		t.Fatal("daemon-exit wait was not called")
	}
	if !waitCalled {
		t.Fatal("successor readiness wait was not called")
	}
	if startCalled {
		t.Fatal("restartDaemonAfterEngineSwitch started fallback daemon despite ready successor")
	}
}

func TestRunLauncherUpgradeRestartNoDaemonSucceeds(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	pendingPath := launcherPath + "~"
	writeTestFile(t, launcherPath, "engine v1")
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
	gotLauncher, err := os.ReadFile(launcherPath)
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	if string(gotLauncher) != "engine v1" {
		t.Fatalf("launcher content = %q, want staged binary", gotLauncher)
	}
}

func TestRunLauncherUpgradeRestartReexecsUpdatedLauncher(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	pendingPath := launcherPath + "~"
	writeTestFile(t, launcherPath, "old launcher")
	writeTestFile(t, pendingPath, "new launcher and engine")

	oldIsDaemonRunning := launcherIsDaemonRunning
	oldRunRestartActive := launcherRunRestartActive
	t.Cleanup(func() {
		launcherIsDaemonRunning = oldIsDaemonRunning
		launcherRunRestartActive = oldRunRestartActive
	})

	reexecCalled := false
	launcherIsDaemonRunning = func(string) bool {
		t.Fatal("restart should be delegated to the updated launcher, not run inline")
		return false
	}
	launcherRunRestartActive = func(gotLauncherPath, gotEnginePath string) int {
		reexecCalled = true
		if gotLauncherPath != launcherPath {
			t.Fatalf("launcherPath = %q, want %q", gotLauncherPath, launcherPath)
		}
		if !strings.Contains(gotEnginePath, versionStoreDir(launcherPath)) {
			t.Fatalf("enginePath = %q, want version store path", gotEnginePath)
		}
		return 0
	}

	if code := runLauncherUpgrade(launcherPath, []string{"--restart"}); code != 0 {
		t.Fatalf("runLauncherUpgrade(--restart) code = %d, want 0", code)
	}
	if !reexecCalled {
		t.Fatal("updated launcher restart was not re-executed")
	}
}

func TestRunLauncherUpgradeRestartActiveDoesNotRequirePendingUpdate(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(versionStoreDir(launcherPath), "abc123", engineFileName())
	writeTestFile(t, launcherPath, "launcher")
	writeTestFile(t, enginePath, "engine")

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
		t.Fatal("--restart-active should not start a daemon when no daemon is running")
		return nil
	}
	launcherWaitForDaemon = func(string, time.Duration) error {
		t.Fatal("--restart-active should not wait when no daemon is running")
		return nil
	}

	if code := runLauncherUpgrade(launcherPath, []string{"--restart-active", enginePath}); code != 0 {
		t.Fatalf("runLauncherUpgrade(--restart-active) code = %d, want 0", code)
	}
}

func TestInstallVersionedEngineUpdatesStaleLauncherWhenEngineAlreadyInstalled(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	pendingPath := launcherPath + "~"
	writeTestFile(t, launcherPath, "old launcher")
	writeTestFile(t, pendingPath, "new engine")
	hash, err := fileHash(pendingPath)
	if err != nil {
		t.Fatalf("fileHash() error = %v", err)
	}
	enginePath := filepath.Join(versionStoreDir(launcherPath), hash[:12], engineFileName())
	writeTestFile(t, enginePath, "new engine")
	if err := writeActiveEngine(launcherPath, enginePath); err != nil {
		t.Fatalf("writeActiveEngine() error = %v", err)
	}

	gotEnginePath, installed, launcherUpdated, err := installVersionedEngine(launcherPath, pendingPath)
	if err != nil {
		t.Fatalf("installVersionedEngine() error = %v", err)
	}
	if gotEnginePath != enginePath {
		t.Fatalf("enginePath = %q, want %q", gotEnginePath, enginePath)
	}
	if installed {
		t.Fatal("installed = true, want false for already-installed engine")
	}
	if !launcherUpdated {
		t.Fatal("launcherUpdated = false, want true")
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending path still exists after launcher update: %v", err)
	}
	gotLauncher, err := os.ReadFile(launcherPath)
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	if string(gotLauncher) != "new engine" {
		t.Fatalf("launcher content = %q, want staged binary", gotLauncher)
	}
}
