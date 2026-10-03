package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func maintenanceUpdateFiles(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	current, staged := filepath.Join(dir, "current"), filepath.Join(dir, "staged")
	if err := os.WriteFile(current, []byte("original engine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new engine"), 0o600); err != nil {
		t.Fatal(err)
	}
	return current, staged
}

func assertMaintenanceUpdateUntouched(t *testing.T, current, staged string, result UpdateAndRestartResult) {
	t.Helper()
	for path, want := range map[string]string{current: "original engine", staged: "new engine"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("guard mutated %s: %q %v", path, data, err)
		}
	}
	if result.OldPath != "" || result.GracefulRestarted || result.FallbackShutdown || result.ReplacementStarted || result.ReplacementReady || result.CleanedStale != 0 {
		t.Fatalf("refusal crossed activation/lifecycle boundary: %+v", result)
	}
}

func TestMaintenanceNamespaceLockFailureIsTerminalEvenWithProceedFlag(t *testing.T) {
	eng := newUpdateTestEngine(t)
	current, staged := maintenanceUpdateFiles(t)
	lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath(eng.cfg.BaseDir, eng.cfg.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	result, err := eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged, ProceedWithoutLock: true, CleanStale: true})
	if !errors.Is(err, ipc.ErrFileLocked) {
		t.Fatalf("lock contention: %v", err)
	}
	assertMaintenanceUpdateUntouched(t, current, staged, result)
	result, err = eng.RestartWithSuccessor(context.Background(), RestartWithSuccessorOptions{SuccessorExe: staged, ProceedWithoutLock: true})
	if !errors.Is(err, ipc.ErrFileLocked) {
		t.Fatalf("successor lock contention: %v", err)
	}
	assertMaintenanceUpdateUntouched(t, current, staged, result)
}

func TestMaintenanceDurableFencePrecedesSwapAndSuccessor(t *testing.T) {
	config := t.TempDir()
	t.Setenv("APPDATA", config)
	t.Setenv("XDG_CONFIG_HOME", config)
	eng := newUpdateTestEngine(t)
	// Avoid t.TempDir's long test-name component while retaining the owned TMP root.
	baseDir, err := os.MkdirTemp(os.TempDir(), "mu*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })
	eng.cfg.BaseDir = baseDir
	d, err := daemon.New(daemon.Config{ControlPath: eng.ControlSocketPath(), Namespace: eng.cfg.Namespace, SkipSnapshot: true, SessionHandler: updateDummySessionHandler{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	_, sid, _, err := d.Spawn(control.Request{Command: "owned-in-process-context", Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: hold.HoldID}) })
	current, staged := maintenanceUpdateFiles(t)
	result, err := eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged, CleanStale: true})
	if !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("held apply: %v", err)
	}
	assertMaintenanceUpdateUntouched(t, current, staged, result)
	result, err = eng.RestartWithSuccessor(context.Background(), RestartWithSuccessorOptions{SuccessorExe: staged})
	if !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("held successor: %v", err)
	}
	assertMaintenanceUpdateUntouched(t, current, staged, result)
	select {
	case <-d.Done():
		t.Fatal("refusal shut down predecessor")
	default:
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: hold.HoldID}); err != nil {
		t.Fatal(err)
	}
	d.Shutdown()
	result, err = eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged})
	if err != nil {
		t.Fatalf("ordinary clear offline update: %v", err)
	}
	data, err := os.ReadFile(current)
	if err != nil || string(data) != "new engine" {
		t.Fatalf("clear offline replacement not installed: %q %v", data, err)
	}
	old, err := os.ReadFile(result.OldPath)
	if err != nil || string(old) != "original engine" {
		t.Fatalf("original binary not retained: %q %v", old, err)
	}
	if result.DaemonWasRunning || result.ReplacementStarted {
		t.Fatal("offline update proactively started daemon")
	}
}

func TestMaintenanceLiveFailclosedStatePreventsActivation(t *testing.T) {
	for _, data := range []string{
		`{"maintenance_error_code":"maintenance_persistence_failed","maintenance":[]}`,
		`{"maintenance_error_code":"future_unknown_code","maintenance":[]}`,
		`{"maintenance":null}`,
	} {
		t.Run(data, func(t *testing.T) {
			restoreUpdateSeams(t)
			eng := newUpdateTestEngine(t)
			engineCheckMaintenanceForActivation = (*MuxEngine).checkMaintenanceForActivation
			engineIsDaemonRunning = func(string) bool { return true }
			engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
				if req.Cmd != "status" {
					t.Fatalf("guard issued lifecycle command: %s", req.Cmd)
				}
				return &control.Response{OK: true, Data: json.RawMessage(data)}, nil
			}
			current, staged := maintenanceUpdateFiles(t)
			result, err := eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged, CleanStale: true})
			if !isMaintenanceError(err) {
				t.Fatalf("live failclosed/malformed authority accepted: %v", err)
			}
			assertMaintenanceUpdateUntouched(t, current, staged, result)
		})
	}
}

func TestMaintenanceTypedLifecycleRefusalNeverFallsBack(t *testing.T) {
	for _, atShutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "graceful", true: "shutdown"}[atShutdown], func(t *testing.T) {
			restoreUpdateSeams(t)
			eng := newUpdateTestEngine(t)
			var sent []string
			engineIsDaemonRunning = func(string) bool { return true }
			engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
			engineControlSendWithTimeout = func(_ string, req control.Request, _ time.Duration) (*control.Response, error) {
				sent = append(sent, req.Cmd)
				if atShutdown {
					return nil, errors.New("legacy graceful endpoint unavailable")
				}
				return &control.Response{ErrorCode: control.ErrMaintenanceHeld.Code}, nil
			}
			engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
				sent = append(sent, req.Cmd)
				return &control.Response{ErrorCode: control.ErrMaintenanceRetirementBlocked.Code}, nil
			}
			engineWaitForDaemonExit = func(context.Context, string, time.Duration) error {
				t.Fatal("typed refusal entered wait-exit/force fallback")
				return nil
			}
			engineStartDaemonExecutable = func(string, string) error { t.Fatal("typed refusal started successor"); return nil }
			result, err := eng.RestartWithSuccessor(context.Background(), baseRestartWithSuccessorOptions())
			if !isMaintenanceError(err) || result.ReplacementStarted || result.GracefulRestarted {
				t.Fatalf("typed refusal escaped: %+v %v", result, err)
			}
			want := 1
			if atShutdown {
				want = 2
			}
			if len(sent) != want {
				t.Fatalf("terminal refusal issued extra lifecycle operations: %v", sent)
			}
		})
	}
}
