package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
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
	waitReplacement func(context.Context, string, daemonIdentity, time.Duration) (bool, error)
	identity        func(string) (daemonIdentity, error)
	prepareSocket   func(context.Context, string, time.Duration) error
	available       func(string) bool
	acquireLock     func(string) (daemonLock, error)
	maintenanceGate func(*MuxEngine) error
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
		waitReplacement: engineWaitForReplacement,
		identity:        engineDaemonIdentity,
		prepareSocket:   enginePrepareControlSocket,
		available:       engineControlSocketAvailable,
		acquireLock:     engineAcquireDaemonLock,
		maintenanceGate: engineCheckMaintenanceForActivation,
	}
}

func restoreUpdateSeams(t *testing.T) {
	t.Helper()
	orig := captureUpdateSeams()
	engineCheckMaintenanceForActivation = func(*MuxEngine) error { return nil }
	enginePrepareControlSocket = func(context.Context, string, time.Duration) error { return nil }
	engineDaemonIdentity = daemonIdentityFromStatus
	engineControlSend = func(string, control.Request) (*control.Response, error) { return updateAwareStatus(), nil }
	engineWaitForReplacement = func(context.Context, string, daemonIdentity, time.Duration) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() {
		engineUpgradeSwap = orig.swap
		engineCleanStale = orig.clean
		engineControlSend = orig.send
		engineControlSendWithTimeout = orig.sendWithTimeout
		engineIsDaemonRunning = orig.running
		engineStartDaemonExecutable = orig.start
		engineWaitForDaemonReady = orig.waitReady
		engineWaitForDaemonExit = orig.waitExit
		engineWaitForReplacement = orig.waitReplacement
		engineDaemonIdentity = orig.identity
		enginePrepareControlSocket = orig.prepareSocket
		engineControlSocketAvailable = orig.available
		engineAcquireDaemonLock = orig.acquireLock
		engineCheckMaintenanceForActivation = orig.maintenanceGate
	})
}

func updateAwareStatus() *control.Response {
	return &control.Response{OK: true, Data: []byte(`{"pid":1,"daemon_generation":"old","maintenance":[]}`)}
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
	engineWaitForReplacement = func(_ context.Context, _ string, _ daemonIdentity, timeout time.Duration) (bool, error) {
		gotShutdown = timeout
		return true, nil
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

func baseRestartWithSuccessorOptions() RestartWithSuccessorOptions {
	return RestartWithSuccessorOptions{
		SuccessorExe:    "next-engine.exe",
		DrainTimeout:    30 * time.Second,
		RestartTimeout:  time.Minute,
		ShutdownTimeout: time.Second,
		ReadyTimeout:    time.Second,
	}
}

func TestRestartWithSuccessor_ValidationErrors(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)

	_, err := eng.RestartWithSuccessor(context.Background(), RestartWithSuccessorOptions{})
	var updateErr *UpdateAndRestartError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want UpdateAndRestartError", err)
	}
	if updateErr.Phase != UpdatePhaseValidate || !strings.Contains(updateErr.Err.Error(), "SuccessorExe is required") {
		t.Fatalf("phase/err = %s/%v, want validate/SuccessorExe is required", updateErr.Phase, updateErr.Err)
	}
}

func TestRestartWithSuccessor_GracefulSuccessUsesExplicitSuccessor(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	lock := &fakeDaemonLock{}
	var gracefulReq control.Request
	swapCalls := 0
	startCalls := 0

	engineUpgradeSwap = func(string, string) (string, error) {
		swapCalls++
		return "", errors.New("swap must not be called")
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

	got, err := eng.RestartWithSuccessor(context.Background(), baseRestartWithSuccessorOptions())
	if err != nil {
		t.Fatalf("RestartWithSuccessor: %v", err)
	}
	if swapCalls != 0 {
		t.Fatalf("swap calls = %d, want 0", swapCalls)
	}
	if gracefulReq.Cmd != "graceful-restart" || gracefulReq.SuccessorExe != "next-engine.exe" {
		t.Fatalf("graceful request = %+v, want successor next-engine.exe", gracefulReq)
	}
	if !got.DaemonWasRunning || !got.LockAcquired || !got.GracefulRestarted || !got.ReplacementReady {
		t.Fatalf("unexpected result flags: %+v", got)
	}
	if got.OldPath != "" || got.CleanedStale != 0 {
		t.Fatalf("unexpected swap/cleanup fields: %+v", got)
	}
	if startCalls != 0 {
		t.Fatalf("explicit start calls = %d, want 0 when graceful successor is ready", startCalls)
	}
	if !lock.closed {
		t.Fatal("daemon lock was not released")
	}
}

func TestRestartWithSuccessor_UsesNamespaceForDaemonLock(t *testing.T) {
	restoreUpdateSeams(t)
	baseDir := t.TempDir()
	eng, err := New(Config{
		Name:       "display-name",
		Namespace:  "transport-ns",
		Command:    "test-command",
		BaseDir:    baseDir,
		DaemonFlag: "--test-daemon",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var gotLockPath string
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(path string) (daemonLock, error) {
		gotLockPath = path
		return &fakeDaemonLock{}, nil
	}
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return nil }

	if _, err := eng.RestartWithSuccessor(context.Background(), baseRestartWithSuccessorOptions()); err != nil {
		t.Fatalf("RestartWithSuccessor: %v", err)
	}
	wantLockPath := serverid.DaemonLockPath(baseDir, "transport-ns")
	if gotLockPath != wantLockPath {
		t.Fatalf("lock path = %q, want %q", gotLockPath, wantLockPath)
	}
}

func TestRestartWithSuccessor_GracefulErrorFallbackStartsSuccessor(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	var sent []string
	var startedExe, startedFlag string

	engineUpgradeSwap = func(string, string) (string, error) {
		t.Fatal("swap must not be called")
		return "", nil
	}
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(_ string, req control.Request, _ time.Duration) (*control.Response, error) {
		sent = append(sent, req.Cmd)
		return &control.Response{Message: "graceful restart rejected"}, nil
	}
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		if req.Cmd == "status" {
			return updateAwareStatus(), nil
		}
		sent = append(sent, req.Cmd)
		return &control.Response{OK: true}, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineStartDaemonExecutable = func(exe, flag string) error {
		startedExe, startedFlag = exe, flag
		return nil
	}
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return nil }

	got, err := eng.RestartWithSuccessor(context.Background(), baseRestartWithSuccessorOptions())
	if err != nil {
		t.Fatalf("RestartWithSuccessor: %v", err)
	}
	if len(sent) != 2 || sent[0] != "graceful-restart" || sent[1] != "shutdown" {
		t.Fatalf("sent commands = %#v, want graceful-restart then shutdown", sent)
	}
	if !got.FallbackShutdown || !got.ReplacementStarted || !got.ReplacementReady {
		t.Fatalf("unexpected fallback result: %+v", got)
	}
	if startedExe != "next-engine.exe" || startedFlag != "--test-daemon" {
		t.Fatalf("started %q %q, want next-engine.exe --test-daemon", startedExe, startedFlag)
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
		return &control.Response{Message: "graceful restart rejected"}, nil
	}
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		if req.Cmd == "status" {
			return updateAwareStatus(), nil
		}
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

func TestRestartWithSuccessor_PreparesControlSocketBeforeCleanFallbackStart(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	var order []string
	readyCalls := 0

	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForReplacement = func(context.Context, string, daemonIdentity, time.Duration) (bool, error) {
		return false, nil
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error {
		order = append(order, "ready")
		readyCalls++
		if readyCalls == 1 {
			return errors.New("graceful successor not ready")
		}
		return nil
	}
	enginePrepareControlSocket = func(context.Context, string, time.Duration) error {
		order = append(order, "prepare")
		return nil
	}
	engineStartDaemonExecutable = func(string, string) error {
		order = append(order, "start")
		return nil
	}

	got, err := eng.RestartWithSuccessor(context.Background(), RestartWithSuccessorOptions{SuccessorExe: "next.exe"})
	if err != nil {
		t.Fatalf("RestartWithSuccessor: %v", err)
	}
	if !got.GracefulRestarted || !got.ReplacementStarted || !got.ReplacementReady {
		t.Fatalf("unexpected result: %+v", got)
	}
	want := []string{"prepare", "ready", "start", "ready"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %#v, want %#v", order, want)
	}
}

func TestRestartWithSuccessor_TreatsAlreadyBoundReplacementAsReady(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	identityCalls := 0
	prepareCalls := 0
	startCalls := 0

	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineDaemonIdentity = func(string) (daemonIdentity, error) {
		identityCalls++
		if identityCalls == 1 {
			return daemonIdentity{pid: 100, generation: "old"}, nil
		}
		return daemonIdentity{pid: 200, generation: "new"}, nil
	}
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return &control.Response{OK: true}, nil
	}
	engineWaitForReplacement = func(context.Context, string, daemonIdentity, time.Duration) (bool, error) {
		return false, nil
	}
	enginePrepareControlSocket = func(context.Context, string, time.Duration) error {
		prepareCalls++
		return errors.New("old socket still active")
	}
	engineStartDaemonExecutable = func(string, string) error {
		startCalls++
		return nil
	}
	engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return nil }

	got, err := eng.RestartWithSuccessor(context.Background(), RestartWithSuccessorOptions{SuccessorExe: "next.exe"})
	if err != nil {
		t.Fatalf("RestartWithSuccessor: %v", err)
	}
	if !got.GracefulRestarted || !got.ReplacementStarted || !got.ReplacementReady {
		t.Fatalf("unexpected result: %+v", got)
	}
	if prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0 when replacement identity is already active", prepareCalls)
	}
	if startCalls != 0 {
		t.Fatalf("start calls = %d, want 0 when replacement identity is already active", startCalls)
	}
	if identityCalls < 2 {
		t.Fatalf("identity calls = %d, want pre-restart and replacement checks", identityCalls)
	}
}

func TestPrepareControlSocketForReplacement_WaitsOnActiveSocketFalseNegative(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket pathname unlink race does not apply to Windows named pipes")
	}
	restoreUpdateSeams(t)
	baseDir, err := os.MkdirTemp("", "ctl*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })
	ctlPath := filepath.Join(baseDir, "control.sock")
	ln, err := net.Listen("unix", ctlPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
	})

	engineIsDaemonRunning = func(string) bool { return false }

	err = prepareControlSocketForReplacement(context.Background(), ctlPath, 50*time.Millisecond)
	if err == nil {
		t.Fatal("prepareControlSocketForReplacement returned nil while socket listener was still active")
	}
	conn, err := net.DialTimeout("unix", ctlPath, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("control socket path was unlinked; dial after prepare: %v", err)
	}
	_ = conn.Close()
}

func TestPrepareControlSocketForReplacement_ReturnsAfterEndpointUnavailable(t *testing.T) {
	restoreUpdateSeams(t)
	availableCalls := 0
	engineIsDaemonRunning = func(string) bool { return false }
	engineControlSocketAvailable = func(string) bool {
		availableCalls++
		return availableCalls < 3
	}

	if err := prepareControlSocketForReplacement(context.Background(), "control.sock", time.Second); err != nil {
		t.Fatalf("prepareControlSocketForReplacement: %v", err)
	}
	if availableCalls < 3 {
		t.Fatalf("available calls = %d, want polling until unavailable", availableCalls)
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
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		if req.Cmd == "status" {
			return updateAwareStatus(), nil
		}
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
		return &control.Response{Message: "graceful restart rejected"}, nil
	}
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		if req.Cmd == "status" {
			return updateAwareStatus(), nil
		}
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
	engineWaitForReplacement = func(context.Context, string, daemonIdentity, time.Duration) (bool, error) {
		return false, exitErr
	}
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
		return &control.Response{Message: "graceful restart rejected"}, nil
	}
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		if req.Cmd == "status" {
			return updateAwareStatus(), nil
		}
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
		return &control.Response{Message: "graceful restart rejected"}, nil
	}
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		if req.Cmd == "status" {
			return updateAwareStatus(), nil
		}
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
		return &control.Response{Message: "graceful restart rejected"}, nil
	}
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		if req.Cmd == "status" {
			return updateAwareStatus(), nil
		}
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

func TestUpdateRestartUncertainGracefulOutcomePreservesCause(t *testing.T) {
	for _, apply := range []bool{false, true} {
		for name, cause := range map[string]error{
			"timeout":      context.DeadlineExceeded,
			"eof":          io.EOF,
			"unclassified": errors.New("graceful outcome unavailable"),
		} {
			t.Run(fmt.Sprintf("apply=%t/%s", apply, name), func(t *testing.T) {
				restoreUpdateSeams(t)
				eng := newUpdateTestEngine(t)
				engineUpgradeSwap = func(string, string) (string, error) { return "old", nil }
				engineCleanStale = func(string) int { return 7 }
				engineIsDaemonRunning = func(string) bool { return true }
				engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
				engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) { return nil, cause }
				engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
					if req.Cmd == "status" {
						return updateAwareStatus(), nil
					}
					t.Fatal("uncertain outcome sent shutdown")
					return nil, nil
				}
				engineWaitForDaemonExit = func(context.Context, string, time.Duration) error {
					t.Fatal("uncertain outcome waited for exit")
					return nil
				}
				engineStartDaemonExecutable = func(string, string) error { t.Fatal("uncertain outcome started successor"); return nil }
				var result UpdateAndRestartResult
				var err error
				if apply {
					result, err = eng.ApplyUpdateAndRestart(context.Background(), baseUpdateOptions())
				} else {
					result, err = eng.RestartWithSuccessor(context.Background(), baseRestartWithSuccessorOptions())
				}
				var updateErr *UpdateAndRestartError
				if !errors.Is(err, cause) || !errors.As(err, &updateErr) || updateErr.Phase != UpdatePhaseRestart {
					t.Fatalf("original uncertainty/phase lost: %v", err)
				}
				if !result.LockAcquired || !result.DaemonWasRunning || result.FallbackShutdown || result.GracefulRestarted || result.ReplacementStarted || result.ReplacementReady {
					t.Fatalf("uncertainty crossed lifecycle boundary: %+v", result)
				}
				if apply && (result.OldPath != "old" || result.CleanedStale != 7 || updateErr.Result.OldPath != "old" || updateErr.Result.CleanedStale != 7) {
					t.Fatalf("completed swap/cleanup missing from partial result: %+v / %+v", result, updateErr.Result)
				}
			})
		}
	}
}

func TestUpdateRestartFallbackRequiresSameAwareGeneration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    string
		statusErr error
		want      error
	}{
		{"old-endpoint", `{"pid":1,"daemon_generation":"old"}`, nil, control.ErrMaintenanceUnsupported},
		{"new-generation", `{"pid":1,"daemon_generation":"new","maintenance":[]}`, nil, control.ErrMaintenanceInvalid},
		{"missing-generation", `{"pid":1,"maintenance":[]}`, nil, control.ErrMaintenanceInvalid},
		{"status-uncertain", "", io.EOF, io.EOF},
		{"status-held", "", control.ErrMaintenanceHeld, control.ErrMaintenanceHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreUpdateSeams(t)
			eng := newUpdateTestEngine(t)
			engineIsDaemonRunning = func(string) bool { return true }
			engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
			reads := 0
			engineDaemonIdentity = func(path string) (daemonIdentity, error) {
				reads++
				if reads == 1 {
					return daemonIdentity{pid: 1, generation: "old"}, nil
				}
				if tc.statusErr != nil {
					return daemonIdentity{}, tc.statusErr
				}
				return daemonIdentityFromStatus(path)
			}
			engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
				return &control.Response{Message: "graceful restart rejected"}, nil
			}
			engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
				if req.Cmd == "status" {
					return &control.Response{OK: true, Data: []byte(tc.status)}, nil
				}
				t.Fatal("unproven fallback sent shutdown")
				return nil, nil
			}
			engineStartDaemonExecutable = func(string, string) error { t.Fatal("unproven fallback started successor"); return nil }
			result, err := eng.RestartWithSuccessor(context.Background(), baseRestartWithSuccessorOptions())
			if !errors.Is(err, tc.want) || result.FallbackShutdown || result.ReplacementStarted || reads != 2 {
				t.Fatalf("fallback proof accepted or cause lost: %+v %v reads=%d", result, err, reads)
			}
		})
	}
}

func TestRestartWithSuccessor_ShutdownOutcomeUnknownStopsBeforeReplacement(t *testing.T) {
	restoreUpdateSeams(t)
	eng := newUpdateTestEngine(t)
	engineIsDaemonRunning = func(string) bool { return true }
	engineAcquireDaemonLock = func(string) (daemonLock, error) { return &fakeDaemonLock{}, nil }
	engineControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return &control.Response{Message: "graceful restart rejected"}, nil
	}
	engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
		if req.Cmd == "status" {
			return updateAwareStatus(), nil
		}
		return nil, io.EOF
	}
	engineWaitForDaemonExit = func(context.Context, string, time.Duration) error {
		t.Fatal("uncertain shutdown waited for exit")
		return nil
	}
	engineStartDaemonExecutable = func(string, string) error { t.Fatal("uncertain shutdown started successor"); return nil }
	result, err := eng.RestartWithSuccessor(context.Background(), baseRestartWithSuccessorOptions())
	if !errors.Is(err, io.EOF) || !result.FallbackShutdown || result.ReplacementStarted || result.ReplacementReady {
		t.Fatalf("unknown shutdown lost cause or partial result: %+v %v", result, err)
	}
}

type updateFallbackEndpoint struct {
	control.DaemonHandler
	aware    bool
	entered  chan struct{}
	release  <-chan struct{}
	graceful atomic.Int32
	shutdown atomic.Int32
}

func (h *updateFallbackEndpoint) HandleStatus() map[string]interface{} {
	status := h.DaemonHandler.HandleStatus()
	if !h.aware {
		delete(status, "maintenance")
		delete(status, "maintenance_error_code")
	}
	return status
}

func (h *updateFallbackEndpoint) HandleGracefulRestart(int) (string, func(), error) {
	h.graceful.Add(1)
	if h.release != nil {
		h.entered <- struct{}{}
		<-h.release
	}
	return "", nil, errors.New("graceful restart rejected")
}

func (h *updateFallbackEndpoint) HandleShutdown(ms int) string {
	h.shutdown.Add(1)
	return h.DaemonHandler.HandleShutdown(ms)
}

type updateLiveSessionHandler struct{}

func (updateLiveSessionHandler) HandleRequest(_ context.Context, _ muxcore.ProjectContext, raw []byte) ([]byte, error) {
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", request.ID, nil})
}

func TestUpdateRestartLiveEndpointFailurePreservesOwnerAndHost(t *testing.T) {
	for _, apply := range []bool{false, true} {
		for _, uncertain := range []bool{false, true} {
			t.Run(fmt.Sprintf("apply=%t/uncertain=%t", apply, uncertain), func(t *testing.T) {
				restoreUpdateSeams(t)
				config := t.TempDir()
				t.Setenv("APPDATA", config)
				t.Setenv("XDG_CONFIG_HOME", config)
				eng := maintenanceProbeEngine(t)
				engineCheckMaintenanceForActivation = (*MuxEngine).checkMaintenanceForActivation
				engineDaemonIdentity = daemonIdentityFromStatus
				engineControlSend = control.Send
				d, err := daemon.New(daemon.Config{ControlPath: filepath.Join(eng.cfg.BaseDir, "backing.sock"), Namespace: eng.cfg.Namespace, SkipSnapshot: true, SessionHandler: updateLiveSessionHandler{}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(d.Shutdown)
				handler := &updateFallbackEndpoint{DaemonHandler: d, aware: uncertain, entered: make(chan struct{}, 1)}
				var release chan struct{}
				if uncertain {
					release = make(chan struct{})
					handler.release = release
				}
				endpoint, err := control.NewServer(eng.ControlSocketPath(), handler, log.New(io.Discard, "", 0))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(endpoint.Close)
				if uncertain {
					t.Cleanup(func() { close(release) })
				}
				path, sid, token, err := d.Spawn(control.Request{Command: "live-update-consumer", Mode: "isolated", Cwd: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				entry := d.Entry(sid)
				conn, err := ipc.Dial(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
				if _, err := fmt.Fprintln(conn, token); err != nil {
					t.Fatal(err)
				}
				scanner := bufio.NewScanner(conn)
				probe := func() {
					t.Helper()
					if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Fatal(err)
					}
					if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"update/probe"}`); err != nil {
						t.Fatal(err)
					}
					if !scanner.Scan() || !strings.Contains(scanner.Text(), `"id":1`) || !strings.Contains(scanner.Text(), `"result":null`) {
						t.Fatalf("original host cannot use live owner: frame=%q err=%v", scanner.Text(), scanner.Err())
					}
				}
				probe()
				starts := 0
				engineStartDaemonExecutable = func(string, string) error { starts++; return nil }
				engineWaitForDaemonExit = func(context.Context, string, time.Duration) error { return nil }
				engineWaitForDaemonReady = func(context.Context, string, time.Duration) error { return nil }
				var exchangeErr error
				engineControlSendWithTimeout = func(path string, req control.Request, timeout time.Duration) (*control.Response, error) {
					response, err := control.SendWithTimeout(path, req, timeout)
					exchangeErr = err
					return response, err
				}
				current, staged := maintenanceUpdateFiles(t)
				var result UpdateAndRestartResult
				if apply {
					result, err = eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged, RestartTimeout: 250 * time.Millisecond})
				} else {
					result, err = eng.RestartWithSuccessor(context.Background(), RestartWithSuccessorOptions{SuccessorExe: staged, RestartTimeout: 250 * time.Millisecond})
				}
				if uncertain {
					var timeout net.Error
					if !errors.As(exchangeErr, &timeout) || !timeout.Timeout() || !errors.Is(err, exchangeErr) {
						t.Fatalf("contacted uncertainty lost: %v / %v shutdown=%d starts=%d", err, exchangeErr, handler.shutdown.Load(), starts)
					}
					select {
					case <-handler.entered:
					default:
						t.Fatal("graceful request never contacted endpoint")
					}
				} else if !errors.Is(err, control.ErrMaintenanceUnsupported) {
					t.Fatalf("old endpoint not terminal unsupported: %v shutdown=%d starts=%d", err, handler.shutdown.Load(), starts)
				}
				var updateErr *UpdateAndRestartError
				if !errors.As(err, &updateErr) || updateErr.Phase != UpdatePhaseRestart || !result.DaemonWasRunning || result.FallbackShutdown || result.GracefulRestarted || result.ReplacementStarted || result.ReplacementReady {
					t.Fatalf("live failure crossed lifecycle boundary: %+v %v", result, err)
				}
				if apply && (result.OldPath == "" || updateErr.Result.OldPath != result.OldPath) {
					t.Fatal("completed swap omitted from partial error")
				}
				if starts != 0 || handler.shutdown.Load() != 0 || handler.graceful.Load() != 1 || d.Entry(sid) != entry || d.OwnerCount() != 1 {
					t.Fatalf("failure replaced live owner: starts=%d shutdown=%d graceful=%d owners=%d", starts, handler.shutdown.Load(), handler.graceful.Load(), d.OwnerCount())
				}
				select {
				case <-d.Done():
					t.Fatal("failure shut down live daemon")
				default:
				}
				probe()
			})
		}
	}
}
