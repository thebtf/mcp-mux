package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
)

type fakeDaemonLock struct {
	closed bool
}

func (l *fakeDaemonLock) Close() error {
	l.closed = true
	return nil
}

type updateSeams struct {
	swap            func(string, string) (string, error)
	clean           func(string) int
	send            func(string, control.Request) (*control.Response, error)
	sendWithTimeout func(string, control.Request, time.Duration) (*control.Response, error)
	running         func(string) bool
	start           func(string, string) error
	waitReady       func(context.Context, string, time.Duration) error
	waitExit        func(context.Context, string, time.Duration) error
	acquireLock     func(string) (daemonLock, error)
}

func captureUpdateSeams() updateSeams {
	return updateSeams{
		swap:            engineUpgradeSwap,
		clean:           engineCleanStale,
		send:            engineControlSend,
		sendWithTimeout: engineControlSendWithTimeout,
		running:         engineIsDaemonRunning,
		start:           engineStartDaemonExecutable,
		waitReady:       engineWaitForDaemonReady,
		waitExit:        engineWaitForDaemonExit,
		acquireLock:     engineAcquireDaemonLock,
	}
}

func restoreUpdateSeams(t *testing.T) {
	t.Helper()
	orig := captureUpdateSeams()
	t.Cleanup(func() {
		engineUpgradeSwap = orig.swap
		engineCleanStale = orig.clean
		engineControlSend = orig.send
		engineControlSendWithTimeout = orig.sendWithTimeout
		engineIsDaemonRunning = orig.running
		engineStartDaemonExecutable = orig.start
		engineWaitForDaemonReady = orig.waitReady
		engineWaitForDaemonExit = orig.waitExit
		engineAcquireDaemonLock = orig.acquireLock
	})
}

func newUpdateTestEngine(t *testing.T) *MuxEngine {
	t.Helper()
	eng, err := New(Config{
		Name:       "update-test",
		Command:    "test-command",
		BaseDir:    t.TempDir(),
		DaemonFlag: "--test-daemon",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

func baseUpdateOptions() UpdateAndRestartOptions {
	return UpdateAndRestartOptions{
		CurrentExe:      "current.exe",
		StagedExe:       "current.exe~",
		DrainTimeout:    30 * time.Second,
		RestartTimeout:  time.Minute,
		ShutdownTimeout: time.Second,
		ReadyTimeout:    time.Second,
		CleanStale:      true,
	}
}

func TestApplyUpdateAndRestart_ValidationErrors(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)

	for _, tc := range []struct {
		name string
		opts UpdateAndRestartOptions
		want string
	}{
		{name: "missing current", opts: UpdateAndRestartOptions{StagedExe: "next.exe"}, want: "CurrentExe is required"},
		{name: "missing staged", opts: UpdateAndRestartOptions{CurrentExe: "current.exe"}, want: "StagedExe is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.ApplyUpdateAndRestart(context.Background(), tc.opts)
			var updateErr *UpdateAndRestartError
			if !errors.As(err, &updateErr) {
				t.Fatalf("error = %v, want UpdateAndRestartError", err)
			}
			if updateErr.Phase != UpdatePhaseValidate || !strings.Contains(updateErr.Err.Error(), tc.want) {
				t.Fatalf("phase/err = %s/%v, want validate/%q", updateErr.Phase, updateErr.Err, tc.want)
			}
		})
	}
}

func TestApplyUpdateAndRestart_CanceledContextStopsBeforeSwap(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	swapCalls := 0
	engineUpgradeSwap = func(string, string) (string, error) {
		swapCalls++
		return "old", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := eng.ApplyUpdateAndRestart(ctx, baseUpdateOptions())
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Phase != UpdatePhaseValidate || !errors.Is(updateErr.Err, context.Canceled) {
		t.Fatalf("phase/err = %s/%v, want validate/context canceled", updateErr.Phase, updateErr.Err)
	}
	if swapCalls != 0 {
		t.Fatalf("swap calls = %d, want 0 after canceled context", swapCalls)
	}
}

func TestApplyUpdateAndRestart_DefaultTimeouts(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	var gotDrainMs int
	var gotRestart, gotShutdown, gotReady time.Duration

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(_ string, req control.Request, timeout time.Duration) (*control.Response, error) {
		gotDrainMs = req.DrainTimeoutMs
		gotRestart = timeout
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(_ context.Context, _ string, timeout time.Duration) error {
		gotShutdown = timeout
		return nil
	}
	engineWaitForDaemonReady = func(_ context.Context, _ string, timeout time.Duration) error {
		gotReady = timeout
		return nil
	}

	opts := UpdateAndRestartOptions{
		CurrentExe: "current.exe",
		StagedExe:  "current.exe~",
	}
	got, err := eng.ApplyUpdateAndRestart(context.Background(), opts)
	if err != nil {
		t.Fatalf("ApplyUpdateAndRestart: %v", err)
	}
	if !got.GracefulRestarted || !got.ReplacementReady {
		t.Fatalf("unexpected result flags: %+v", got)
	}
	if gotDrainMs != int(defaultUpdateDrainTimeout/time.Millisecond) {
		t.Fatalf("drain timeout ms = %d, want %d", gotDrainMs, int(defaultUpdateDrainTimeout/time.Millisecond))
	}
	if gotRestart != defaultUpdateRestartTimeout || gotShutdown != defaultUpdateShutdownTimeout || gotReady != defaultUpdateReadyTimeout {
		t.Fatalf("timeouts restart/shutdown/ready = %s/%s/%s, want %s/%s/%s",
			gotRestart, gotShutdown, gotReady,
			defaultUpdateRestartTimeout, defaultUpdateShutdownTimeout, defaultUpdateReadyTimeout)
	}
}

func TestApplyUpdateAndRestart_GracefulSuccessUsesSuccessorExe(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	lock := &fakeDaemonLock{}
	var gracefulReq control.Request
	startCalls := 0

	engineUpgradeSwap = func(current, staged string) (string, error) {
		if current != "current.exe" || staged != "current.exe~" {
			t.Fatalf("Swap(%q, %q), want current/staged", current, staged)
		}
		return "current.exe.old.1", nil
	}
	engineCleanStale = func(path string) int {
		if path != "current.exe" {
			t.Fatalf("CleanStale(%q), want current.exe", path)
		}
		return 2
	}
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return lock, nil }
	engineControlSendWithTimeout = func(_ string, req control.Request, timeout time.Duration) (*control.Response, error) {
		gracefulReq = req
		if timeout != time.Minute {
			t.Fatalf("restart timeout = %s, want 1m", timeout)
		}
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return nil }
	engineStartDaemonExecutable = func(string, string) error {
		startCalls++
		return nil
	}

	got, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	if err != nil {
		t.Fatalf("ApplyUpdateAndRestart: %v", err)
	}
	if gracefulReq.Cmd != "graceful-restart" {
		t.Fatalf("graceful cmd = %q, want graceful-restart", gracefulReq.Cmd)
	}
	if gracefulReq.SuccessorExe != "current.exe" {
		t.Fatalf("SuccessorExe = %q, want current.exe", gracefulReq.SuccessorExe)
	}
	if !got.DaemonWasRunning || !got.LockAcquired || !got.GracefulRestarted || !got.ReplacementReady {
		t.Fatalf("unexpected result flags: %+v", got)
	}
	if got.OldPath != "current.exe.old.1" || got.CleanedStale != 2 {
		t.Fatalf("unexpected swap/cleanup result: %+v", got)
	}
	if startCalls != 0 {
		t.Fatalf("explicit start calls = %d, want 0 when graceful successor is ready", startCalls)
	}
	if !lock.closed {
		t.Fatal("daemon lock was not released")
	}
}

func TestApplyUpdateAndRestart_DaemonNotRunningOnlySwaps(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	lockCalls, controlCalls, startCalls := 0, 0, 0

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 1 }
	engineIsDaemonRunning = func(string) bool { return false }
	engineAcquireDaemonLock = func(string) (daemonLock, error) {
		lockCalls++
		return &fakeDaemonLock{}, nil
	}
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		controlCalls++
		return &control.Response{OK: true}, nil
	}
	engineStartDaemonExecutable = func(string, string) error {
		startCalls++
		return nil
	}

	got, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	if err != nil {
		t.Fatalf("ApplyUpdateAndRestart: %v", err)
	}
	if got.DaemonWasRunning || got.ReplacementStarted || got.ReplacementReady {
		t.Fatalf("unexpected daemon flags when daemon is stopped: %+v", got)
	}
	if lockCalls != 0 || controlCalls != 0 || startCalls != 0 {
		t.Fatalf("side effects lock/control/start = %d/%d/%d, want all zero", lockCalls, controlCalls, startCalls)
	}
	if got.CleanedStale != 1 {
		t.Fatalf("CleanedStale = %d, want 1", got.CleanedStale)
	}
}

func TestApplyUpdateAndRestart_SwapFailureStopsBeforeDaemonCalls(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	swapErr := errors.New("new binary not found")
	daemonChecks := 0

	engineUpgradeSwap = func(string, string) (string, error) { return "", swapErr }
	engineIsDaemonRunning = func(string) bool {
		daemonChecks++
		return true
	}

	_, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Phase != UpdatePhaseSwap || !errors.Is(updateErr.Err, swapErr) {
		t.Fatalf("phase/err = %s/%v, want swap/new binary not found", updateErr.Phase, updateErr.Err)
	}
	if daemonChecks != 0 {
		t.Fatalf("daemon checks = %d, want 0 before successful swap", daemonChecks)
	}
}

func TestApplyUpdateAndRestart_GracefulErrorFallsBackToShutdownAndStart(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	var sent []string
	var startedExe, startedFlag string

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(_ string, req control.Request, _ time.Duration) (*control.Response, error) {
		sent = append(sent, req.Cmd)
		return nil, errors.New("dial failed")
	}
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		sent = append(sent, req.Cmd)
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineStartDaemonExecutable = func(exe, flag string) error {
		startedExe, startedFlag = exe, flag
		return nil
	}
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return nil }

	got, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	if err != nil {
		t.Fatalf("ApplyUpdateAndRestart: %v", err)
	}
	if len(sent) != 2 || sent[0] != "graceful-restart" || sent[1] != "shutdown" {
		t.Fatalf("sent commands = %#v, want graceful-restart then shutdown", sent)
	}
	if !got.FallbackShutdown || !got.ReplacementStarted || !got.ReplacementReady {
		t.Fatalf("unexpected fallback result: %+v", got)
	}
	if startedExe != "current.exe" || startedFlag != "--test-daemon" {
		t.Fatalf("started %q %q, want current.exe --test-daemon", startedExe, startedFlag)
	}
}

func TestApplyUpdateAndRestart_LockFailureIsPhaseError(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	lockErr := errors.New("locked")

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return nil, lockErr }

	_, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Phase != UpdatePhaseLock || !errors.Is(updateErr.Err, lockErr) {
		t.Fatalf("phase/err = %s/%v, want lock/locked", updateErr.Phase, updateErr.Err)
	}
}

func TestApplyUpdateAndRestart_ProceedWithoutLockContinuesWithWarning(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	lockErr := errors.New("locked by another updater")

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return nil, lockErr }
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return nil, errors.New("dial failed")
	}
	engineControlSend = func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineStartDaemonExecutable = func(string, string) error { return nil }
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return nil }

	opts := baseUpdateOptions()
	opts.ProceedWithoutLock = true
	got, err := eng.ApplyUpdateAndRestart(context.Background(), opts)
	if err != nil {
		t.Fatalf("ApplyUpdateAndRestart: %v", err)
	}
	if got.LockAcquired {
		t.Fatalf("LockAcquired = true, want false")
	}
	if !got.FallbackShutdown || !got.ReplacementReady {
		t.Fatalf("unexpected result flags: %+v", got)
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "could not acquire daemon lock") {
		t.Fatalf("warnings = %#v, want daemon lock warning", got.Warnings)
	}
}

func TestApplyUpdateAndRestart_FallbackExitTimeoutIsPhaseError(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	exitErr := errors.New("still running")

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return &control.Response{OK: false, Message: "nope"}, nil
	}
	engineControlSend = func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return exitErr }

	_, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Phase != UpdatePhaseWaitExit || !errors.Is(updateErr.Err, exitErr) {
		t.Fatalf("phase/err = %s/%v, want wait_exit/still running", updateErr.Phase, updateErr.Err)
	}
	if !updateErr.Result.FallbackShutdown {
		t.Fatalf("partial result did not record fallback shutdown: %+v", updateErr.Result)
	}
}

func TestApplyUpdateAndRestart_ErrorResultIncludesCleanStaleCount(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	readyErr := errors.New("not ready")

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 7 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return nil, errors.New("dial failed")
	}
	engineControlSend = func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineStartDaemonExecutable = func(string, string) error { return nil }
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return readyErr }

	_, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Result.CleanedStale != 7 {
		t.Fatalf("CleanedStale in error result = %d, want 7", updateErr.Result.CleanedStale)
	}
}

func TestApplyUpdateAndRestart_GracefulExitTimeoutIsPhaseError(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	exitErr := errors.New("old daemon still responding")
	readyChecks := 0

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return exitErr }
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error {
		readyChecks++
		return nil
	}

	_, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Phase != UpdatePhaseWaitExit || !errors.Is(updateErr.Err, exitErr) {
		t.Fatalf("phase/err = %s/%v, want wait_exit/old daemon still responding", updateErr.Phase, updateErr.Err)
	}
	if !updateErr.Result.GracefulRestarted {
		t.Fatalf("partial result did not record graceful restart: %+v", updateErr.Result)
	}
	if readyChecks != 0 {
		t.Fatalf("ready checks = %d, want 0 when old daemon did not exit", readyChecks)
	}
}

func TestApplyUpdateAndRestart_ReadyTimeoutIsPhaseError(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	readyErr := errors.New("not ready")

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return nil, errors.New("dial failed")
	}
	engineControlSend = func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineStartDaemonExecutable = func(string, string) error { return nil }
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return readyErr }

	_, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Phase != UpdatePhaseReady || !errors.Is(updateErr.Err, readyErr) {
		t.Fatalf("phase/err = %s/%v, want wait_ready/not ready", updateErr.Phase, updateErr.Err)
	}
	if !updateErr.Result.ReplacementStarted {
		t.Fatalf("partial result did not record replacement start: %+v", updateErr.Result)
	}
}

func TestApplyUpdateAndRestart_StartFailureIsPhaseError(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	startErr := errors.New("spawn denied")

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return nil, errors.New("dial failed")
	}
	engineControlSend = func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineStartDaemonExecutable = func(string, string) error { return startErr }

	_, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Phase != UpdatePhaseStart || !errors.Is(updateErr.Err, startErr) {
		t.Fatalf("phase/err = %s/%v, want start_replacement/spawn denied", updateErr.Phase, updateErr.Err)
	}
	if !updateErr.Result.FallbackShutdown {
		t.Fatalf("partial result did not record fallback shutdown: %+v", updateErr.Result)
	}
}

type updateDummySessionHandler struct{}

func (updateDummySessionHandler) HandleRequest(context.Context, muxcore.ProjectContext, []byte) ([]byte, error) {
	return []byte(`{"jsonrpc":"2.0","result":null,"id":1}`), nil
}

func TestApplyUpdateAndRestart_SessionHandlerConsumerUsesEngineNamespace(t *testing.T) {
	restoreUpdateSeams(t)
	eng, err := New(Config{
		Name:           "aimux-like",
		SessionHandler: updateDummySessionHandler{},
		Handler:        func(context.Context, io.Reader, io.Writer) error { return nil },
		BaseDir:        t.TempDir(),
		DaemonFlag:     "--aimux-daemon",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var sawLockPath, sawControlPath, sawDaemonFlag string

	engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
	engineCleanStale = func(string) int { return 0 }
	engineIsDaemonRunning = func(path string) bool {
		sawControlPath = path
		return true
	}
	engineAcquireDaemonLock = func(path string) (daemonLock, error) {
		sawLockPath = path
		return &fakeDaemonLock{}, nil
	}
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return nil, errors.New("force fallback")
	}
	engineControlSend = func(string, control.Request) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineStartDaemonExecutable = func(_, flag string) error {
		sawDaemonFlag = flag
		return nil
	}
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return nil }

	if _, err := eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions()); err != nil {
		t.Fatalf("ApplyUpdateAndRestart: %v", err)
	}
	if sawControlPath == "" || sawLockPath == "" {
		t.Fatalf("namespace paths not used: control=%q lock=%q", sawControlPath, sawLockPath)
	}
	if sawDaemonFlag != "--aimux-daemon" {
		t.Fatalf("daemon flag = %q, want --aimux-daemon", sawDaemonFlag)
	}
}
