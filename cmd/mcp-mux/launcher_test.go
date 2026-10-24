package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func writeContentAddressedTestEngine(t *testing.T, launcherPath, content string) string {
	t.Helper()
	pending := filepath.Join(t.TempDir(), engineFileName())
	writeTestFile(t, pending, content)
	hash, err := fileHash(pending)
	if err != nil {
		t.Fatal(err)
	}
	enginePath := filepath.Join(versionStoreDir(launcherPath), hash[:12], engineFileName())
	writeTestFile(t, enginePath, content)
	return enginePath
}

func TestInstallVersionedEngineKeepsLauncherStableByDefault(t *testing.T) {
	isolateMaintenanceActivation(t)
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
	if launcherUpdated {
		t.Fatal("installVersionedEngine() launcherUpdated = true, want false for ordinary engine update")
	}

	launcherBytes, err := os.ReadFile(launcherPath)
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	if string(launcherBytes) != "stable launcher" {
		t.Fatalf("launcher content = %q, want stable launcher", launcherBytes)
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
	isolateMaintenanceActivation(t)
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

	if err := writeActiveEngineUnderLock(launcherPath, enginePath); err != nil {
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

func TestResolveActiveEngineRejectsPointerOutsideVersionStore(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, launcherFileName())
	outside := filepath.Join(dir, "outside", engineFileName())
	writeTestFile(t, outside, "outside engine")
	if err := os.MkdirAll(versionStoreDir(launcherPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeEngineFile(launcherPath), []byte(outside+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if resolved, ok := resolveActiveEngine(launcherPath); ok || resolved != "" {
		t.Fatalf("outside active engine resolved as %q", resolved)
	}
}

func TestResolveActiveEngineRejectsTraversalOutsideVersionStore(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, launcherFileName())
	outside := filepath.Join(dir, "outside", engineFileName())
	writeTestFile(t, outside, "outside engine")
	if err := os.MkdirAll(versionStoreDir(launcherPath), 0o755); err != nil {
		t.Fatal(err)
	}
	traversal := filepath.Join("..", "outside", engineFileName())
	if err := os.WriteFile(activeEngineFile(launcherPath), []byte(traversal+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if resolved, ok := resolveActiveEngine(launcherPath); ok || resolved != "" {
		t.Fatalf("traversal active engine resolved as %q", resolved)
	}
}

func TestInstalledEngineAuthorizationRejectsModifiedContent(t *testing.T) {
	launcherPath := filepath.Join(t.TempDir(), launcherFileName())
	enginePath := writeContentAddressedTestEngine(t, launcherPath, "authorized engine")
	if !authorizeInstalledEnginePath(launcherPath, enginePath) {
		t.Fatal("content-addressed engine was not authorized")
	}
	writeTestFile(t, enginePath, "modified engine")
	if authorizeInstalledEnginePath(launcherPath, enginePath) {
		t.Fatal("modified content retained authorization")
	}
}

func TestDaemonExecutableForSpawnUsesActiveEnginePointer(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	oldEnginePath := filepath.Join(versionStoreDir(launcherPath), "old123", engineFileName())
	activeEnginePath := filepath.Join(versionStoreDir(launcherPath), "new456", engineFileName())
	writeTestFile(t, oldEnginePath, "old engine")
	writeTestFile(t, activeEnginePath, "new engine")
	if err := writeActiveEngineUnderLock(launcherPath, activeEnginePath); err != nil {
		t.Fatalf("writeActiveEngine() error = %v", err)
	}
	t.Setenv(envActiveEngineFile, activeEngineFile(launcherPath))

	got := daemonExecutableForSpawn(oldEnginePath)
	if !samePath(got, activeEnginePath) {
		t.Fatalf("daemonExecutableForSpawn() = %q, want active engine %q", got, activeEnginePath)
	}
}

func TestDaemonExecutableForSpawnIgnoresMissingActiveEnginePointer(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	currentEnginePath := filepath.Join(versionStoreDir(launcherPath), "old123", engineFileName())
	missingActiveEnginePath := filepath.Join(versionStoreDir(launcherPath), "missing", engineFileName())
	writeTestFile(t, currentEnginePath, "old engine")
	if err := writeActiveEngineUnderLock(launcherPath, missingActiveEnginePath); err != nil {
		t.Fatalf("writeActiveEngine() error = %v", err)
	}
	t.Setenv(envActiveEngineFile, activeEngineFile(launcherPath))

	got := daemonExecutableForSpawn(currentEnginePath)
	if !samePath(got, currentEnginePath) {
		t.Fatalf("daemonExecutableForSpawn() = %q, want current engine %q", got, currentEnginePath)
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

func TestRunEngineProcessKeepsOperatorCommandsInLauncher(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(versionStoreDir(launcherPath), "abc123", engineFileName())

	for _, args := range [][]string{
		{"status"},
		{"stop"},
		{"stop", "--force"},
	} {
		handled, code := runEngineProcess(launcherPath, enginePath, args)
		if handled {
			t.Fatalf("runEngineProcess(%v) handled command in active engine, want launcher-local handling", args)
		}
		if code != 0 {
			t.Fatalf("runEngineProcess(%v) code = %d, want 0", args, code)
		}
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
	isolateMaintenanceActivation(t)
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

	if err := restartDaemonAfterEngineSwitch(launcherPath, enginePath, false); err != nil {
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
	isolateMaintenanceActivation(t)
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
		if req.Cmd == "status" {
			return &control.Response{OK: true, Data: []byte(`{"maintenance":[]}`)}, nil
		}
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

	if err := restartDaemonAfterEngineSwitch(launcherPath, enginePath, true); err != nil {
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

func TestRestartDaemonAfterEngineSwitchDefersWhenLiveSessionsExist(t *testing.T) {
	isolateMaintenanceActivation(t)
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

	gracefulCalled := false
	launcherIsDaemonRunning = func(path string) bool {
		return path == serverid.DaemonControlPath("", engineName)
	}
	launcherControlSendWithTimeout = func(path string, req control.Request, timeout time.Duration) (*control.Response, error) {
		if path != serverid.DaemonControlPath("", engineName) {
			t.Fatalf("control path = %q, want daemon control path", path)
		}
		switch req.Cmd {
		case "status":
			return &control.Response{
				OK:   true,
				Data: []byte(`{"maintenance":[],"servers":[{"session_count":1}]}`),
			}, nil
		case "graceful-restart":
			gracefulCalled = true
			return &control.Response{OK: true}, nil
		default:
			t.Fatalf("unexpected control command %q", req.Cmd)
			return nil, nil
		}
	}
	launcherWaitForDaemonExit = func(string, string) {
		t.Fatal("daemon-exit wait should not be called when live sessions exist")
	}
	launcherWaitForDaemon = func(string, time.Duration) error {
		t.Fatal("successor readiness wait should not be called when live sessions exist")
		return nil
	}
	launcherStartDaemonProcessFrom = func(string, string) error {
		t.Fatal("fallback daemon should not be started when live sessions exist")
		return nil
	}

	if err := restartDaemonAfterEngineSwitch(launcherPath, enginePath, false); err != nil {
		t.Fatalf("restartDaemonAfterEngineSwitch() error = %v, want nil deferred restart", err)
	}
	if gracefulCalled {
		t.Fatal("graceful-restart was sent despite live sessions")
	}
}

func TestRunLauncherUpgradeRestartNoDaemonSucceeds(t *testing.T) {
	isolateMaintenanceActivation(t)
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
		t.Fatalf("launcher content = %q, want stable launcher", gotLauncher)
	}
}

func TestRunLauncherUpgradeRestartReexecsExplicitLauncherUpdate(t *testing.T) {
	isolateMaintenanceActivation(t)
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
	launcherRunRestartActive = func(gotLauncherPath, gotEnginePath string, forceDaemonRestart bool) int {
		reexecCalled = true
		if gotLauncherPath != launcherPath {
			t.Fatalf("launcherPath = %q, want %q", gotLauncherPath, launcherPath)
		}
		if !strings.Contains(gotEnginePath, versionStoreDir(launcherPath)) {
			t.Fatalf("enginePath = %q, want version store path", gotEnginePath)
		}
		if forceDaemonRestart {
			t.Fatal("forceDaemonRestart = true, want false")
		}
		return 0
	}

	if code := runLauncherUpgrade(launcherPath, []string{"--update-launcher", "--restart"}); code != 0 {
		t.Fatalf("runLauncherUpgrade(--update-launcher --restart) code = %d, want 0", code)
	}
	if !reexecCalled {
		t.Fatal("updated launcher restart was not re-executed")
	}
}

func TestRunLauncherUpgradeRestartActiveDoesNotRequirePendingUpdate(t *testing.T) {
	isolateMaintenanceActivation(t)
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

func TestRunLauncherUpgradeRestartActiveCanonicalizesEnginePath(t *testing.T) {
	isolateMaintenanceActivation(t)
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(dir, "relative-engine", engineFileName())
	writeTestFile(t, launcherPath, "launcher")
	writeTestFile(t, enginePath, "engine")

	oldRestart := launcherRunRestartActive
	oldWd, wdErr := os.Getwd()
	if wdErr != nil {
		t.Fatalf("Getwd(): %v", wdErr)
	}
	t.Cleanup(func() {
		launcherRunRestartActive = oldRestart
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%s): %v", dir, err)
	}

	var gotEnginePath string
	launcherRunRestartActive = func(string, string, bool) int {
		t.Fatal("--restart-active should restart inline, not re-exec")
		return 1
	}
	oldIsDaemonRunning := launcherIsDaemonRunning
	oldControlSendWithTimeout := launcherControlSendWithTimeout
	oldWaitForDaemonExit := launcherWaitForDaemonExit
	oldWaitForDaemon := launcherWaitForDaemon
	oldStartDaemonProcessFrom := launcherStartDaemonProcessFrom
	t.Cleanup(func() {
		launcherIsDaemonRunning = oldIsDaemonRunning
		launcherControlSendWithTimeout = oldControlSendWithTimeout
		launcherWaitForDaemonExit = oldWaitForDaemonExit
		launcherWaitForDaemon = oldWaitForDaemon
		launcherStartDaemonProcessFrom = oldStartDaemonProcessFrom
	})
	launcherIsDaemonRunning = func(string) bool { return true }
	launcherControlSendWithTimeout = func(_ string, req control.Request, _ time.Duration) (*control.Response, error) {
		if req.Cmd == "status" {
			return &control.Response{OK: true, Data: []byte(`{"maintenance":[]}`)}, nil
		}
		gotEnginePath = req.SuccessorExe
		return &control.Response{OK: true}, nil
	}
	launcherWaitForDaemonExit = func(string, string) {}
	launcherWaitForDaemon = func(string, time.Duration) error { return nil }
	launcherStartDaemonProcessFrom = func(string, string) error {
		t.Fatal("--restart-active should not fallback-start when successor is ready")
		return nil
	}

	if code := runLauncherUpgrade(launcherPath, []string{"--restart-active", filepath.Join("relative-engine", engineFileName()), "--force-daemon-restart"}); code != 0 {
		t.Fatalf("runLauncherUpgrade(--restart-active) code = %d, want 0", code)
	}
	gotReal, gotRealErr := filepath.EvalSymlinks(gotEnginePath)
	if gotRealErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", gotEnginePath, gotRealErr)
	}
	wantReal, wantRealErr := filepath.EvalSymlinks(enginePath)
	if wantRealErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", enginePath, wantRealErr)
	}
	if gotReal != wantReal {
		t.Fatalf("SuccessorExe = %q, want absolute path %q", gotEnginePath, enginePath)
	}
}

func TestInstallVersionedEngineKeepsStaleLauncherWhenEngineAlreadyInstalled(t *testing.T) {
	isolateMaintenanceActivation(t)
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
	if err := writeActiveEngineUnderLock(launcherPath, enginePath); err != nil {
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
	if launcherUpdated {
		t.Fatal("launcherUpdated = true, want false for ordinary engine update")
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending path still exists after launcher update: %v", err)
	}
	gotLauncher, err := os.ReadFile(launcherPath)
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	if string(gotLauncher) != "old launcher" {
		t.Fatalf("launcher content = %q, want old launcher", gotLauncher)
	}
}

func TestRestartDaemonAfterEngineSwitchFallbackRequiresSameAwareGeneration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		before    string
		after     string
		transport error
		want      error
	}{
		{"aware-clear", `{"maintenance":[],"daemon_generation":"old"}`, `{"maintenance":[],"daemon_generation":"old"}`, nil, nil},
		{"old-endpoint", `{"daemon_generation":"old"}`, `{"daemon_generation":"old"}`, nil, control.ErrMaintenanceUnsupported},
		{"changed-generation", `{"maintenance":[],"daemon_generation":"old"}`, `{"maintenance":[],"daemon_generation":"new"}`, nil, control.ErrMaintenanceInvalid},
		{"unknown-transport", `{"maintenance":[],"daemon_generation":"old"}`, `{"maintenance":[],"daemon_generation":"old"}`, io.EOF, io.EOF},
		{"unknown-deadline", `{"maintenance":[],"daemon_generation":"old"}`, `{"maintenance":[],"daemon_generation":"old"}`, os.ErrDeadlineExceeded, os.ErrDeadlineExceeded},
		{"invalid-status", `{"maintenance":[],"daemon_generation":"old"}`, `{"maintenance":null,"daemon_generation":"old"}`, nil, control.ErrMaintenanceInvalid},
		{"writer-failed", `{"maintenance":[],"daemon_generation":"old"}`, `{"maintenance":[],"daemon_generation":"old","maintenance_error_code":"maintenance_persistence_failed"}`, nil, control.ErrMaintenancePersistenceFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateMaintenanceActivation(t)
			oldRunning, oldSend := launcherIsDaemonRunning, launcherControlSendWithTimeout
			oldExit, oldWait, oldStart := launcherWaitForDaemonExit, launcherWaitForDaemon, launcherStartDaemonProcessFrom
			t.Cleanup(func() {
				launcherIsDaemonRunning, launcherControlSendWithTimeout = oldRunning, oldSend
				launcherWaitForDaemonExit, launcherWaitForDaemon, launcherStartDaemonProcessFrom = oldExit, oldWait, oldStart
			})
			statusCalls, graceful, shutdown, starts, exits := 0, 0, 0, 0, 0
			launcherIsDaemonRunning = func(string) bool { return true }
			launcherWaitForDaemonExit = func(string, string) { exits++ }
			launcherWaitForDaemon = func(string, time.Duration) error { return nil }
			launcherStartDaemonProcessFrom = func(string, string) error { starts++; return nil }
			launcherControlSendWithTimeout = func(_ string, req control.Request, _ time.Duration) (*control.Response, error) {
				switch req.Cmd {
				case "status":
					statusCalls++
					data := tc.before
					if statusCalls > 2 {
						data = tc.after
					}
					return &control.Response{OK: true, Data: []byte(data)}, nil
				case "graceful-restart":
					graceful++
					if tc.transport != nil {
						return nil, tc.transport
					}
					return &control.Response{Message: "graceful restart rejected"}, nil
				case "shutdown":
					shutdown++
					return &control.Response{OK: true}, nil
				default:
					t.Fatalf("unexpected control command: %s", req.Cmd)
					return nil, nil
				}
			}
			err := restartDaemonAfterEngineSwitch("launcher", "engine", true)
			if !errors.Is(err, tc.want) {
				t.Fatalf("fallback cause lost: %v, want %v", err, tc.want)
			}
			if graceful != 1 {
				t.Fatalf("graceful requests=%d, want 1", graceful)
			}
			if tc.want == nil {
				if shutdown != 1 || starts != 1 || exits != 1 {
					t.Fatalf("aware clean fallback lost: shutdown=%d starts=%d exits=%d", shutdown, starts, exits)
				}
			} else if shutdown != 0 || starts != 0 || exits != 0 {
				t.Fatalf("unproven fallback mutated live daemon: shutdown=%d starts=%d exits=%d", shutdown, starts, exits)
			}
			if tc.transport != nil && statusCalls != 2 {
				t.Fatalf("unknown transport retried status/lifecycle: statuses=%d", statusCalls)
			}
		})
	}
}

func targetLauncherUpgradeFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	updater := filepath.Join(root, "updater", launcherFileName())
	target := filepath.Join(root, "installed", launcherFileName())
	writeTestFile(t, updater, "current-aware updater")
	writeTestFile(t, updater+"~", "candidate engine from updater")
	writeTestFile(t, target, "old stable target")
	writeTestFile(t, target+"~", "unrelated target-side pending update")
	for _, launcher := range []string{updater, target} {
		engine := writeContentAddressedTestEngine(t, launcher, "previous engine for "+filepath.Base(filepath.Dir(launcher)))
		if err := writeActiveEngineUnderLock(launcher, engine); err != nil {
			t.Fatalf("seed active pointer: %v", err)
		}
	}
	return root, updater, target
}

func targetLauncherUpgradeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			entries[path] = "dir"
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries[path] = "file:" + string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot upgrade fixture: %v", err)
	}
	return entries
}

func assertTargetLauncherUpgradeSnapshot(t *testing.T, root string, want map[string]string) {
	t.Helper()
	if got := targetLauncherUpgradeSnapshot(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("upgrade changed unexpected fixture paths or bytes:\ngot: %#v\nwant: %#v", got, want)
	}
}

func stubTargetLauncherUpgradeLifecycle(t *testing.T) {
	t.Helper()
	oldRunning, oldSend := launcherIsDaemonRunning, launcherControlSendWithTimeout
	oldExit, oldWait, oldStart := launcherWaitForDaemonExit, launcherWaitForDaemon, launcherStartDaemonProcessFrom
	oldRestart := launcherRunRestartActive
	t.Cleanup(func() {
		launcherIsDaemonRunning, launcherControlSendWithTimeout = oldRunning, oldSend
		launcherWaitForDaemonExit, launcherWaitForDaemon, launcherStartDaemonProcessFrom = oldExit, oldWait, oldStart
		launcherRunRestartActive = oldRestart
	})
	launcherIsDaemonRunning = func(string) bool {
		t.Fatal("upgrade unexpectedly inspected daemon liveness")
		return false
	}
	launcherControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		t.Fatal("upgrade unexpectedly contacted daemon control")
		return nil, nil
	}
	launcherWaitForDaemonExit = func(string, string) { t.Fatal("upgrade closed or waited for the existing daemon") }
	launcherWaitForDaemon = func(string, time.Duration) error {
		t.Fatal("upgrade waited for a successor daemon")
		return nil
	}
	launcherStartDaemonProcessFrom = func(string, string) error {
		t.Fatal("upgrade started a successor daemon")
		return nil
	}
	launcherRunRestartActive = func(string, string, bool) int {
		t.Fatal("upgrade re-executed the old target launcher")
		return 1
	}
}

func runTargetLauncherUpgrade(t *testing.T, updater string, args []string) (int, string) {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "upgrade-stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = output
	defer func() {
		os.Stderr = oldStderr
		_ = output.Close()
	}()
	code := runLauncherUpgrade(updater, args)
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(data)
}

func TestRunLauncherUpgradeTargetLauncher(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit bool
		relative bool
		restart  bool
	}{
		{name: "absolute-target", explicit: true},
		{name: "relative-target", explicit: true, relative: true},
		{name: "default-self"},
		{name: "restart-preserves-live-transports", explicit: true, restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateMaintenanceActivation(t)
			stubTargetLauncherUpgradeLifecycle(t)
			root, updater, target := targetLauncherUpgradeFixture(t)
			before := targetLauncherUpgradeSnapshot(t, root)
			destination := updater
			var args []string
			if tc.explicit {
				destination = target
				targetArg := target
				if tc.relative {
					oldWd, err := os.Getwd()
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := os.Chdir(oldWd); err != nil {
							t.Fatalf("restore cwd: %v", err)
						}
					})
					if err := os.Chdir(root); err != nil {
						t.Fatal(err)
					}
					targetArg = filepath.Join("installed", launcherFileName())
				}
				args = append(args, "--target-launcher", targetArg)
			}
			if tc.restart {
				args = append(args, "--restart")
				launcherIsDaemonRunning = func(path string) bool {
					if path != serverid.DaemonControlPath("", engineName) {
						t.Fatalf("daemon control path = %q", path)
					}
					return true
				}
			}
			hash, err := fileHash(updater + "~")
			if err != nil {
				t.Fatal(err)
			}
			enginePath := filepath.Join(versionStoreDir(destination), hash[:12], engineFileName())
			pointer, err := filepath.Rel(versionStoreDir(destination), enginePath)
			if err != nil {
				t.Fatal(err)
			}
			want := make(map[string]string, len(before)+2)
			for path, content := range before {
				want[path] = content
			}
			delete(want, updater+"~")
			want[filepath.Dir(enginePath)] = "dir"
			want[enginePath] = "file:candidate engine from updater"
			want[activeEngineFile(destination)] = "file:" + pointer + "\n"
			statusCalls := 0
			launcherControlSendWithTimeout = func(path string, req control.Request, _ time.Duration) (*control.Response, error) {
				if path != serverid.DaemonControlPath("", engineName) || req.Cmd != "status" {
					t.Fatalf("upgrade sent lifecycle mutation: path=%q cmd=%q", path, req.Cmd)
				}
				competingLock, err := ipc.AcquireFileLock(serverid.DaemonLockPath("", engineName))
				if competingLock != nil {
					_ = competingLock.Close()
				}
				if !errors.Is(err, ipc.ErrFileLocked) {
					t.Fatalf("upgrade status ran outside namespace mutation lock: %v", err)
				}
				statusCalls++
				if statusCalls == 1 {
					assertTargetLauncherUpgradeSnapshot(t, root, before)
				} else {
					assertTargetLauncherUpgradeSnapshot(t, root, want)
				}
				return &control.Response{OK: true, Data: []byte(`{"maintenance":[],"servers":[{"session_count":1}]}`)}, nil
			}
			code, output := runTargetLauncherUpgrade(t, updater, args)
			if code != 0 {
				t.Fatalf("runLauncherUpgrade(%v) code = %d, want 0:\n%s", args, code, output)
			}
			wantStatusCalls := 1
			if tc.restart {
				wantStatusCalls++
				if !strings.Contains(output, "Daemon restart deferred") {
					t.Fatalf("live restart did not report deferral:\n%s", output)
				}
			}
			if statusCalls != wantStatusCalls {
				t.Fatalf("status calls = %d, want %d", statusCalls, wantStatusCalls)
			}
			assertTargetLauncherUpgradeSnapshot(t, root, want)
			active, ok := resolveActiveEngine(destination)
			if !ok || !filepath.IsAbs(active) || !samePath(active, enginePath) {
				t.Fatalf("active engine = %q ok=%v, want absolute target path %q", active, ok, enginePath)
			}
		})
	}
}

func TestRunLauncherUpgradeTargetLauncherUpdatePreservesIndependentPending(t *testing.T) {
	isolateMaintenanceActivation(t)
	stubTargetLauncherUpgradeLifecycle(t)
	_, updater, target := targetLauncherUpgradeFixture(t)
	launcherControlSendWithTimeout = func(path string, req control.Request, _ time.Duration) (*control.Response, error) {
		if path != serverid.DaemonControlPath("", engineName) || req.Cmd != "status" {
			t.Fatalf("launcher update sent lifecycle mutation: path=%q cmd=%q", path, req.Cmd)
		}
		return &control.Response{OK: true, Data: []byte(`{"maintenance":[]}`)}, nil
	}
	code, output := runTargetLauncherUpgrade(t, updater, []string{"--target-launcher", target, "--update-launcher"})
	if code != 0 {
		t.Fatalf("explicit target launcher update code = %d, want 0:\n%s", code, output)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "candidate engine from updater" {
		t.Fatalf("target launcher was not updated from updater pending: %q %v", content, err)
	}
	if content, err := os.ReadFile(target + "~"); err != nil || string(content) != "unrelated target-side pending update" {
		t.Fatalf("independent target-side pending update changed: %q %v", content, err)
	}
	if content, err := os.ReadFile(updater); err != nil || string(content) != "current-aware updater" {
		t.Fatalf("executing updater changed: %q %v", content, err)
	}
	if _, err := os.Stat(updater + "~"); !os.IsNotExist(err) {
		t.Fatalf("selected updater pending was not consumed: %v", err)
	}
	active, ok := resolveActiveEngine(target)
	if !ok {
		t.Fatal("target active engine pointer was not written")
	}
	if content, err := os.ReadFile(active); err != nil || string(content) != "candidate engine from updater" {
		t.Fatalf("target active engine was not installed from updater pending: %q %v", content, err)
	}
}

func TestRunLauncherUpgradeTargetLauncherMaintenanceRefusal(t *testing.T) {
	for _, name := range []string{"held", "namespace-lock"} {
		t.Run(name, func(t *testing.T) {
			isolateMaintenanceActivation(t)
			stubTargetLauncherUpgradeLifecycle(t)
			root, updater, target := targetLauncherUpgradeFixture(t)
			before := targetLauncherUpgradeSnapshot(t, root)
			wantError := ipc.ErrFileLocked.Error()
			statusCalls := 0
			if name == "held" {
				lease := control.MaintenanceResult{HoldID: "lease", ServerID: "owner", State: control.MaintenanceHeld, ExpiresAt: time.Now().Add(time.Minute).UTC(), DrainDeadline: time.Now().UTC(), TreesRetired: true}
				data, err := json.Marshal(map[string]any{"maintenance": []control.MaintenanceResult{lease}})
				if err != nil {
					t.Fatal(err)
				}
				wantError = string(control.ErrMaintenanceHeld.Code)
				launcherControlSendWithTimeout = func(path string, req control.Request, _ time.Duration) (*control.Response, error) {
					if path != serverid.DaemonControlPath("", engineName) || req.Cmd != "status" {
						t.Fatalf("held upgrade sent lifecycle mutation: path=%q cmd=%q", path, req.Cmd)
					}
					competingLock, err := ipc.AcquireFileLock(serverid.DaemonLockPath("", engineName))
					if competingLock != nil {
						_ = competingLock.Close()
					}
					if !errors.Is(err, ipc.ErrFileLocked) {
						t.Fatalf("maintenance refusal ran outside namespace lock: %v", err)
					}
					assertTargetLauncherUpgradeSnapshot(t, root, before)
					statusCalls++
					return &control.Response{OK: true, Data: data}, nil
				}
			} else {
				lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath("", engineName))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
			}
			code, output := runTargetLauncherUpgrade(t, updater, []string{"--target-launcher", target, "--update-launcher", "--restart"})
			if code != 1 || !strings.Contains(output, wantError) {
				t.Fatalf("upgrade refusal code = %d, want 1 and %q:\n%s", code, wantError, output)
			}
			if name == "held" && statusCalls != 1 {
				t.Fatalf("held status calls = %d, want 1", statusCalls)
			}
			assertTargetLauncherUpgradeSnapshot(t, root, before)
		})
	}
}

func TestRunLauncherUpgradeTargetLauncherRejectsInvalidBeforeMutation(t *testing.T) {
	for _, name := range []string{"missing-value", "missing-file", "directory", "invalid-path"} {
		t.Run(name, func(t *testing.T) {
			isolateMaintenanceActivation(t)
			stubTargetLauncherUpgradeLifecycle(t)
			root, updater, target := targetLauncherUpgradeFixture(t)
			args := []string{"--target-launcher"}
			switch name {
			case "missing-file":
				args = append(args, filepath.Join(root, "missing", launcherFileName()))
			case "directory":
				args = append(args, filepath.Dir(target))
			case "invalid-path":
				args = append(args, target+"\x00")
			}
			before := targetLauncherUpgradeSnapshot(t, root)
			code, output := runTargetLauncherUpgrade(t, updater, args)
			if code == 0 || strings.Contains(output, "flag provided but not defined") {
				t.Fatalf("target was not validated: code=%d:\n%s", code, output)
			}
			assertTargetLauncherUpgradeSnapshot(t, root, before)
		})
	}
}

func TestRunLauncherUpgradeTargetLauncherRequiresUpdaterPending(t *testing.T) {
	isolateMaintenanceActivation(t)
	stubTargetLauncherUpgradeLifecycle(t)
	root, updater, target := targetLauncherUpgradeFixture(t)
	if err := os.Remove(updater + "~"); err != nil {
		t.Fatal(err)
	}
	before := targetLauncherUpgradeSnapshot(t, root)
	launcherControlSendWithTimeout = func(path string, req control.Request, _ time.Duration) (*control.Response, error) {
		if path != serverid.DaemonControlPath("", engineName) || req.Cmd != "status" {
			t.Fatalf("missing source pending triggered lifecycle mutation: path=%q cmd=%q", path, req.Cmd)
		}
		return &control.Response{OK: true, Data: []byte(`{"maintenance":[]}`)}, nil
	}
	code, output := runTargetLauncherUpgrade(t, updater, []string{"--target-launcher", target})
	if code != 1 || !strings.Contains(output, updater+"~") {
		t.Fatalf("missing updater pending must not consume target pending: code=%d:\n%s", code, output)
	}
	assertTargetLauncherUpgradeSnapshot(t, root, before)
}
