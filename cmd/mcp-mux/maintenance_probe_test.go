package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func maintenanceProbeLauncherFiles(t *testing.T) (string, string, string) {
	t.Helper()
	launcher := filepath.Join(t.TempDir(), launcherFileName())
	pending, pointer := launcher+"~", activeEngineFile(launcher)
	writeTestFile(t, launcher, "original launcher")
	writeTestFile(t, pending, "new engine")
	writeTestFile(t, pointer, "previous-engine-pointer\n")
	return launcher, pending, pointer
}

func assertMaintenanceProbeLauncherUntouched(t *testing.T, launcher, pending, pointer string) {
	t.Helper()
	for path, want := range map[string]string{launcher: "original launcher", pending: "new engine", pointer: "previous-engine-pointer\n"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("uncertain activation changed %s: %q %v", path, data, err)
		}
	}
	entries, err := os.ReadDir(versionStoreDir(launcher))
	if err != nil || len(entries) != 1 || entries[0].Name() != "active.txt" {
		t.Fatalf("uncertain activation changed engine layout: %v %v", entries, err)
	}
}

func TestMaintenanceMutationProbeErrorsPreserveLauncherAndPointer(t *testing.T) {
	for name, probeErr := range map[string]error{
		"dial-timeout":         &net.OpError{Op: "dial", Net: "unix", Err: context.DeadlineExceeded},
		"dial-busy":            &net.OpError{Op: "dial", Net: "unix", Err: syscall.EAGAIN},
		"read-missing":         &net.OpError{Op: "read", Net: "unix", Err: os.ErrNotExist},
		"write-refused":        &net.OpError{Op: "write", Net: "unix", Err: syscall.ECONNREFUSED},
		"read-nonsocket":       &net.OpError{Op: "read", Net: "unix", Err: syscall.ENOTSOCK},
		"path-read-missing":    &os.PathError{Op: "read", Path: "contacted endpoint", Err: os.ErrNotExist},
		"unclassified-missing": os.ErrNotExist,
	} {
		t.Run(name, func(t *testing.T) {
			isolateMaintenanceActivation(t)
			oldSend := launcherControlSendWithTimeout
			t.Cleanup(func() { launcherControlSendWithTimeout = oldSend })
			calls := 0
			launcherControlSendWithTimeout = func(_ string, req control.Request, _ time.Duration) (*control.Response, error) {
				calls++
				if req.Cmd != "status" {
					t.Fatalf("activation issued lifecycle command %s", req.Cmd)
				}
				lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath("", engineName))
				if lock != nil {
					_ = lock.Close()
				}
				if !errors.Is(err, ipc.ErrFileLocked) {
					t.Fatalf("probe ran outside namespace lock: %v", err)
				}
				return nil, probeErr
			}
			launcher, pending, pointer := maintenanceProbeLauncherFiles(t)
			_, _, _, err := installVersionedEngineWithOptions(launcher, pending, versionedEngineInstallOptions{UpdateLauncher: true})
			if !errors.Is(err, probeErr) {
				t.Fatalf("activation lost original transport error: %v, want %v", err, probeErr)
			}
			assertMaintenanceProbeLauncherUntouched(t, launcher, pending, pointer)
			if err := restartDaemonAfterEngineSwitch(launcher, pending, true); !errors.Is(err, probeErr) {
				t.Fatalf("restart lost original error: %v", err)
			}
			assertMaintenanceProbeLauncherUntouched(t, launcher, pending, pointer)
			if calls != 2 {
				t.Fatalf("uncertainty retried/fell back: %d status exchanges", calls)
			}
		})
	}
}

func TestMaintenanceMutationContactedReadTimeoutPreservesLauncher(t *testing.T) {
	isolateMaintenanceActivation(t)
	release := make(chan struct{})
	requests := startStopPingEndpoint(t, os.TempDir(), "", release)
	t.Cleanup(func() { close(release) })
	oldSend := launcherControlSendWithTimeout
	t.Cleanup(func() { launcherControlSendWithTimeout = oldSend })
	var probeErr error
	launcherControlSendWithTimeout = func(path string, req control.Request, _ time.Duration) (*control.Response, error) {
		response, err := control.SendWithTimeout(path, req, time.Second)
		probeErr = err
		return response, err
	}
	launcher, pending, pointer := maintenanceProbeLauncherFiles(t)
	_, _, _, err := installVersionedEngineWithOptions(launcher, pending, versionedEngineInstallOptions{UpdateLauncher: true})
	var timeout net.Error
	if probeErr == nil || !errors.As(probeErr, &timeout) || !timeout.Timeout() || !errors.Is(err, probeErr) {
		t.Fatalf("contacted timeout lost: activation=%v transport=%v", err, probeErr)
	}
	select {
	case request := <-requests:
		var req control.Request
		if json.Unmarshal([]byte(request), &req) != nil || req.Cmd != "status" {
			t.Fatalf("activation sent non-status request: %q", request)
		}
	default:
		t.Fatal("endpoint never received the status probe")
	}
	assertMaintenanceProbeLauncherUntouched(t, launcher, pending, pointer)
}

func TestMaintenanceMutationRejectsUntrustedResponses(t *testing.T) {
	for name, response := range map[string]*control.Response{
		"nil":           nil,
		"nonOK":         {Message: "status outcome unavailable"},
		"invalid-typed": {OK: true, ErrorCode: control.ErrMaintenanceHeld.Code},
		"writer-failed": {ErrorCode: control.ErrMaintenancePersistenceFailed.Code},
	} {
		t.Run(name, func(t *testing.T) {
			isolateMaintenanceActivation(t)
			oldSend := launcherControlSendWithTimeout
			t.Cleanup(func() { launcherControlSendWithTimeout = oldSend })
			launcherControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) { return response, nil }
			launcher, pending, pointer := maintenanceProbeLauncherFiles(t)
			_, _, _, err := installVersionedEngineWithOptions(launcher, pending, versionedEngineInstallOptions{UpdateLauncher: true})
			if err == nil {
				t.Fatal("untrusted status response permitted activation")
			}
			if name == "writer-failed" && !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
				t.Fatalf("writer failure classification lost: %v", err)
			}
			assertMaintenanceProbeLauncherUntouched(t, launcher, pending, pointer)
		})
	}
}

func TestMaintenanceMutationDefiniteAbsenceAllowsOfflineInstall(t *testing.T) {
	for _, regularFile := range []bool{false, true} {
		name := "missing"
		if regularFile {
			name = "regular-file"
		}
		t.Run(name, func(t *testing.T) {
			if regularFile && runtime.GOOS == "windows" {
				t.Skip("named pipes do not use the endpoint filesystem leaf")
			}
			isolateMaintenanceActivation(t)
			endpoint := serverid.DaemonControlPath("", engineName)
			if regularFile {
				if err := os.WriteFile(endpoint, []byte("stale non-socket endpoint"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, rawErr := control.SendWithTimeout(endpoint, control.Request{Cmd: "status"}, time.Second)
				var dialErr *net.OpError
				if !errors.As(rawErr, &dialErr) || dialErr.Op != "dial" {
					t.Fatalf("not a native dial failure: %v", rawErr)
				}
				if runtime.GOOS == "darwin" && !errors.Is(rawErr, syscall.ENOTSOCK) {
					t.Fatalf("Darwin regular endpoint did not produce ENOTSOCK: %v", rawErr)
				}
			}
			launcher := filepath.Join(t.TempDir(), launcherFileName())
			pending := launcher + "~"
			writeTestFile(t, launcher, "original launcher")
			writeTestFile(t, pending, "new engine")
			enginePath, installed, _, err := installVersionedEngineWithOptions(launcher, pending, versionedEngineInstallOptions{})
			if err != nil || !installed {
				t.Fatalf("ordinary persisted-clear offline install: installed=%t err=%v", installed, err)
			}
			data, err := os.ReadFile(enginePath)
			if err != nil || string(data) != "new engine" {
				t.Fatalf("installed engine: %q %v", data, err)
			}
			active, ok := resolveActiveEngine(launcher)
			if !ok || !samePath(active, enginePath) {
				t.Fatalf("offline active pointer=%q ok=%t", active, ok)
			}
			if regularFile {
				data, err := os.ReadFile(endpoint)
				if err != nil || string(data) != "stale non-socket endpoint" {
					t.Fatalf("activation removed stale leaf: %q %v", data, err)
				}
			}
			if _, err := os.Stat(serverid.DaemonLockPath("", engineName) + ".maintenance"); !os.IsNotExist(err) {
				t.Fatalf("read-only guard wrote authority: %v", err)
			}
		})
	}
}

func TestMaintenanceMutationOldClearEndpointAllowsInstall(t *testing.T) {
	isolateMaintenanceActivation(t)
	startFakeDaemon(t, os.TempDir(), &refreshTestHandler{})
	launcher := filepath.Join(t.TempDir(), launcherFileName())
	pending := launcher + "~"
	writeTestFile(t, launcher, "original launcher")
	writeTestFile(t, pending, "new engine")
	enginePath, installed, _, err := installVersionedEngineWithOptions(launcher, pending, versionedEngineInstallOptions{})
	if err != nil || !installed {
		t.Fatalf("old clear endpoint activation: installed=%t err=%v", installed, err)
	}
	if active, ok := resolveActiveEngine(launcher); !ok || !samePath(active, enginePath) {
		t.Fatalf("old endpoint pointer=%q ok=%t", active, ok)
	}
	if _, err := os.Stat(serverid.DaemonLockPath("", engineName) + ".maintenance"); !os.IsNotExist(err) {
		t.Fatalf("old status wrote maintenance authority: %v", err)
	}
}
