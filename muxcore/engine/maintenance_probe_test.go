package engine

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func maintenanceProbeEngine(t *testing.T) *MuxEngine {
	t.Helper()
	eng := newUpdateTestEngine(t)
	base, err := os.MkdirTemp(os.TempDir(), "mp*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	eng.cfg.BaseDir = base
	return eng
}

func TestMaintenanceActivationProbeErrorsPreserveUpdate(t *testing.T) {
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
			restoreUpdateSeams(t)
			eng := maintenanceProbeEngine(t)
			engineCheckMaintenanceForActivation = (*MuxEngine).checkMaintenanceForActivation
			calls, cleaned, started := 0, 0, 0
			engineControlSend = func(_ string, req control.Request) (*control.Response, error) {
				calls++
				if req.Cmd != "status" {
					t.Fatalf("uncertain activation issued %s", req.Cmd)
				}
				return nil, probeErr
			}
			engineCleanStale = func(string) int { cleaned++; return 0 }
			engineStartDaemonExecutable = func(string, string) error { started++; return nil }
			current, staged := maintenanceUpdateFiles(t)
			result, err := eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged, CleanStale: true, ProceedWithoutLock: true})
			if !errors.Is(err, probeErr) {
				t.Fatalf("activation lost original transport error: %v, want %v", err, probeErr)
			}
			assertMaintenanceUpdateUntouched(t, current, staged, result)
			result, err = eng.RestartWithSuccessor(context.Background(), RestartWithSuccessorOptions{SuccessorExe: staged, ProceedWithoutLock: true})
			if !errors.Is(err, probeErr) {
				t.Fatalf("successor lost original transport error: %v", err)
			}
			assertMaintenanceUpdateUntouched(t, current, staged, result)
			if calls != 2 || cleaned != 0 || started != 0 {
				t.Fatalf("uncertainty crossed lifecycle boundary: status=%d clean=%d start=%d", calls, cleaned, started)
			}
		})
	}
}

type maintenanceProbeStatusHandler struct {
	pingOnlyControlHandler
	data    map[string]interface{}
	entered chan struct{}
	release <-chan struct{}
	calls   atomic.Int32
}

func (h *maintenanceProbeStatusHandler) HandleStatus() map[string]interface{} {
	h.calls.Add(1)
	if h.entered != nil {
		h.entered <- struct{}{}
		<-h.release
	}
	return h.data
}

func TestMaintenanceActivationContactedReadTimeoutPreservesUpdate(t *testing.T) {
	restoreUpdateSeams(t)
	eng := maintenanceProbeEngine(t)
	engineCheckMaintenanceForActivation = (*MuxEngine).checkMaintenanceForActivation
	release := make(chan struct{})
	handler := &maintenanceProbeStatusHandler{entered: make(chan struct{}, 1), release: release}
	endpoint, err := control.NewServer(eng.ControlSocketPath(), handler, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(release); endpoint.Close() })
	var probeErr error
	engineControlSend = func(path string, req control.Request) (*control.Response, error) {
		if req.Cmd != "status" {
			t.Fatalf("contacted uncertainty issued %s", req.Cmd)
		}
		response, err := control.SendWithTimeout(path, req, time.Second)
		probeErr = err
		return response, err
	}
	current, staged := maintenanceUpdateFiles(t)
	result, err := eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged, CleanStale: true})
	var timeout net.Error
	if probeErr == nil || !errors.As(probeErr, &timeout) || !timeout.Timeout() || !errors.Is(err, probeErr) {
		t.Fatalf("real contacted timeout lost: activation=%v transport=%v", err, probeErr)
	}
	select {
	case <-handler.entered:
	default:
		t.Fatal("endpoint did not receive the original status probe")
	}
	assertMaintenanceUpdateUntouched(t, current, staged, result)
	if handler.calls.Load() != 1 {
		t.Fatal("uncertain status was retried")
	}
}

func TestMaintenanceActivationNativeStatusCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]interface{}
		want error
	}{
		{"old-clear", map[string]interface{}{"daemon": true}, nil},
		{"aware-clear", map[string]interface{}{"maintenance": []control.MaintenanceResult{}}, nil},
		{"writer-failed", map[string]interface{}{"maintenance": []control.MaintenanceResult{}, "maintenance_error_code": control.ErrMaintenancePersistenceFailed.Code}, control.ErrMaintenancePersistenceFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := maintenanceProbeEngine(t)
			handler := &maintenanceProbeStatusHandler{data: tc.data}
			endpoint, err := control.NewServer(eng.ControlSocketPath(), handler, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(endpoint.Close)
			lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath(eng.cfg.BaseDir, eng.cfg.Namespace))
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := eng.checkMaintenanceForActivation(); !errors.Is(err, tc.want) {
				t.Fatalf("native status %s: %v, want %v", tc.name, err, tc.want)
			}
			if handler.calls.Load() != 1 {
				t.Fatal("activation did not read exactly one live status")
			}
		})
	}
}

func TestMaintenanceActivationDefiniteAbsenceAllowsOfflineSwap(t *testing.T) {
	for _, regularFile := range []bool{false, true} {
		name := "missing"
		if regularFile {
			name = "regular-file"
		}
		t.Run(name, func(t *testing.T) {
			if regularFile && runtime.GOOS == "windows" {
				t.Skip("named pipes do not use the endpoint filesystem leaf")
			}
			eng := maintenanceProbeEngine(t)
			if regularFile {
				if err := os.WriteFile(eng.ControlSocketPath(), []byte("stale non-socket endpoint"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			current, staged := maintenanceUpdateFiles(t)
			result, err := eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged})
			if err != nil {
				t.Fatalf("persisted-clear offline activation: %v", err)
			}
			for path, want := range map[string]string{current: "new engine", result.OldPath: "original engine"} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Fatalf("offline swap %s: %q %v", path, data, err)
				}
			}
			if result.DaemonWasRunning || result.ReplacementStarted {
				t.Fatal("offline activation proactively started a daemon")
			}
			if regularFile {
				data, err := os.ReadFile(eng.ControlSocketPath())
				if err != nil || string(data) != "stale non-socket endpoint" {
					t.Fatalf("activation removed endpoint: %q %v", data, err)
				}
			}
			if _, err := os.Stat(serverid.DaemonLockPath(eng.cfg.BaseDir, eng.cfg.Namespace) + ".maintenance"); !os.IsNotExist(err) {
				t.Fatalf("read-only activation wrote authority: %v", err)
			}
			if err := daemon.CheckMaintenanceForActivation(eng.cfg.Namespace, eng.ControlSocketPath()); err != nil {
				t.Fatalf("offline persisted clear changed: %v", err)
			}
		})
	}
}

func TestMaintenanceActivationUntrustedResponsePreservesUpdate(t *testing.T) {
	for name, response := range map[string]*control.Response{
		"nil":           nil,
		"nonOK":         {Message: "status outcome unavailable"},
		"invalid-typed": {OK: true, ErrorCode: control.ErrMaintenanceHeld.Code},
		"writer-failed": {ErrorCode: control.ErrMaintenancePersistenceFailed.Code},
	} {
		t.Run(name, func(t *testing.T) {
			restoreUpdateSeams(t)
			eng := maintenanceProbeEngine(t)
			engineCheckMaintenanceForActivation = (*MuxEngine).checkMaintenanceForActivation
			engineControlSend = func(string, control.Request) (*control.Response, error) { return response, nil }
			current, staged := maintenanceUpdateFiles(t)
			result, err := eng.ApplyUpdateAndRestart(context.Background(), UpdateAndRestartOptions{CurrentExe: current, StagedExe: staged})
			if err == nil {
				t.Fatal("untrusted status permitted update")
			}
			if name == "writer-failed" && !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
				t.Fatalf("writer failure classification lost: %v", err)
			}
			assertMaintenanceUpdateUntouched(t, current, staged, result)
		})
	}
}
