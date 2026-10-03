package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func maintenanceCLIResponse(t *testing.T, output *bytes.Buffer) control.Response {
	t.Helper()
	decoder := json.NewDecoder(output)
	var response control.Response
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("decode maintenance response: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("expected one JSON response, extra=%v error=%v", extra, err)
	}
	return response
}

func TestMaintenanceCLIInvalidBeforeEndpointAccess(t *testing.T) {
	dir := shortTempDir(t, "mi")
	t.Setenv("TMPDIR", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)
	for _, tc := range []struct {
		name, cmd string
		args      []string
	}{
		{"missing-target", "hold", []string{"--json"}},
		{"whitespace-id", "hold", []string{" owner", "--json"}},
		{"extra-target", "hold", []string{"owner", "other", "--json"}},
		{"zero-ttl", "hold", []string{"owner", "--ttl", "0", "--json"}},
		{"negative-ttl", "renew", []string{"lease", "--ttl", "-1s", "--json"}},
		{"excessive-ttl", "hold", []string{"owner", "--ttl", "1h1ms", "--json"}},
		{"ttl-overflow", "renew", []string{"lease", "--ttl", "9999999999999999999h", "--json"}},
		{"invalid-duration", "hold", []string{"owner", "--ttl", "private-invalid-input", "--json"}},
		{"negative-drain", "hold", []string{"owner", "--drain-timeout", "-1ms", "--json"}},
		{"drain-timeout-overflow", "hold", []string{"owner", "--drain-timeout", "2562047h47m16.854s", "--json"}},
		{"submillisecond-force", "hold", []string{"owner", "--drain-timeout", "1ns", "--json"}},
		{"submillisecond-ttl", "renew", []string{"lease", "--ttl", "1ns", "--json"}},
		{"resume-ttl", "resume", []string{"lease", "--ttl", "5m", "--json"}},
		{"missing-lease", "renew", []string{"--json"}},
		{"force-bypass", "hold", []string{"owner", "--force", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runMaintenanceCommand(tc.cmd, tc.args, &stdout, &stderr); code == 0 {
				t.Fatal("invalid maintenance input exited successfully")
			}
			response := maintenanceCLIResponse(t, &stdout)
			if !errors.Is(response.Err(), control.ErrMaintenanceInvalid) || response.Maintenance != nil {
				t.Fatalf("invalid request response=%+v", response)
			}
			if stderr.Len() != 0 || strings.Contains(response.Message, "private-invalid-input") {
				t.Fatalf("unsafe or duplicate output: response=%+v stderr=%q", response, stderr.String())
			}
		})
	}
}

func TestMaintenanceCLIUnsupportedPositionalFirst(t *testing.T) {
	dir := shortTempDir(t, "mu")
	t.Setenv("TMPDIR", dir)
	startFakeDaemon(t, dir, &refreshTestHandler{})
	for _, tc := range []struct {
		cmd  string
		args []string
	}{
		{"hold", []string{"exact-owner", "--ttl", "5m", "--drain-timeout", "10s", "--json"}},
		{"hold", []string{"exact-owner", "--ttl", "1h", "--drain-timeout", "0", "--json"}},
		{"hold", []string{"exact-owner", "--json"}},
		{"resume", []string{"exact-lease", "--json"}},
		{"renew", []string{"exact-lease", "--ttl", "5m", "--json"}},
	} {
		var stdout, stderr bytes.Buffer
		if code := runMaintenanceCommand(tc.cmd, tc.args, &stdout, &stderr); code == 0 {
			t.Fatal("unsupported endpoint exited successfully")
		}
		if response := maintenanceCLIResponse(t, &stdout); !errors.Is(response.Err(), control.ErrMaintenanceUnsupported) {
			t.Fatalf("%s %v response=%+v", tc.cmd, tc.args, response)
		}
	}
}

type maintenanceCLIExchangeHandler struct {
	refreshTestHandler
	result   control.MaintenanceResult
	requests chan control.Request
}

func (h *maintenanceCLIExchangeHandler) HandleMaintenance(req control.Request) (control.MaintenanceResult, error) {
	h.requests <- req
	result := h.result
	switch req.Cmd {
	case "hold":
		time.Sleep(5200 * time.Millisecond)
	case "resume":
		result.State = control.MaintenanceReleased
	case "renew":
		result.ExpiresAt = time.Now().UTC().Add(time.Duration(*req.HoldTTLMS) * time.Millisecond)
	default:
		return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
	}
	return result, nil
}

func TestMaintenanceCLIDefaultBudgetReceivesDelayedHold(t *testing.T) {
	dir := shortTempDir(t, "mbudget")
	t.Setenv("TMPDIR", dir)
	now := time.Now().UTC()
	handler := &maintenanceCLIExchangeHandler{
		result: control.MaintenanceResult{
			HoldID: "exact-lease", ServerID: "exact-owner", State: control.MaintenanceHeld,
			ExpiresAt: now.Add(5 * time.Minute), DrainDeadline: now, TreesRetired: true,
		},
		requests: make(chan control.Request, 4),
	}
	startFakeDaemon(t, dir, handler)
	for _, tc := range []struct {
		cmd   string
		args  []string
		state control.MaintenanceState
	}{
		{"hold", []string{"exact-owner", "--drain-timeout", "0", "--json"}, control.MaintenanceHeld},
		{"renew", []string{"exact-lease", "--json"}, control.MaintenanceHeld},
		{"resume", []string{"exact-lease", "--json"}, control.MaintenanceReleased},
	} {
		var stdout, stderr bytes.Buffer
		if code := runMaintenanceCommand(tc.cmd, tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s failed to receive its original outcome: code=%d stdout=%q stderr=%q", tc.cmd, code, stdout.String(), stderr.String())
		}
		response := maintenanceCLIResponse(t, &stdout)
		if !response.OK || response.Maintenance == nil || response.Maintenance.HoldID != handler.result.HoldID || response.Maintenance.State != tc.state || !response.Maintenance.TreesRetired || stderr.Len() != 0 {
			t.Fatalf("%s returned the wrong lease outcome: %+v stderr=%q", tc.cmd, response, stderr.String())
		}
		if len(handler.requests) != 1 {
			t.Fatalf("%s submitted %d operations, want one", tc.cmd, len(handler.requests))
		}
		req := <-handler.requests
		if req.Cmd != tc.cmd || (tc.cmd == "hold" && (req.ServerID != "exact-owner" || req.DrainTimeoutMs != 0)) || (tc.cmd != "hold" && req.HoldID != "exact-lease") {
			t.Fatalf("%s changed the selected operation: %+v", tc.cmd, req)
		}
	}
}

func TestMaintenanceCLIEndpointLossIsNotHeld(t *testing.T) {
	dir := shortTempDir(t, "ml")
	t.Setenv("TMPDIR", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)
	var stdout, stderr bytes.Buffer
	if code := runMaintenanceCommand("resume", []string{"exact-lease", "--json"}, &stdout, &stderr); code == 0 {
		t.Fatal("missing endpoint exited successfully")
	}
	response := maintenanceCLIResponse(t, &stdout)
	if response.OK || response.ErrorCode != "" || response.Maintenance != nil || response.Message != "maintenance endpoint unavailable" {
		t.Fatalf("endpoint loss misclassified or leaked diagnostics: %+v", response)
	}
}

func TestMaintenanceStandaloneAdmissionUnsupported(t *testing.T) {
	for _, variable := range []string{"MCP_MUX_NO_DAEMON", "MCP_MUX_DAEMON"} {
		t.Run(variable, func(t *testing.T) {
			dir := shortTempDir(t, "mdirect")
			output, err := runHelperMain(t, dir, variable+"=1", "MCPMUX_DISABLE_LAUNCHER=1")
			if err == nil || !strings.Contains(output, "maintenance_unsupported") || strings.Contains(output, "becoming owner") {
				t.Fatalf("standalone CLI did not refuse before direct owner admission: err=%v stderr=%q", err, output)
			}
		})
	}
}

func TestMaintenanceSpawnAndRefreshKeepTypedRefusal(t *testing.T) {
	dir := shortTempDir(t, "mt")
	t.Setenv("TMPDIR", dir)
	refusal := fmt.Errorf("private diagnostic: %w", control.ErrMaintenanceHeld)
	startFakeDaemon(t, dir, &refreshTestHandler{spawnErr: refusal, refreshErr: refusal})
	logger := log.New(io.Discard, "", 0)
	_, _, _, spawnErr := spawnViaDaemon("unused-command", nil, dir, "global", nil, logger)
	_, refreshErr := refreshTokenViaDaemon("previous-token", "", logger)
	for _, err := range []error{spawnErr, refreshErr} {
		if !errors.Is(err, control.ErrMaintenanceHeld) || isTransientDaemonReconnectErr(err) || strings.Contains(err.Error(), "private diagnostic") {
			t.Fatalf("typed refusal lost, leaked, or made retryable: %v", err)
		}
	}
}

type maintenanceShutdownRefusal struct {
	refreshTestHandler
	fallback atomic.Int32
	attempts atomic.Int32
}

func (h *maintenanceShutdownRefusal) HandleShutdownWithError(int) (string, error) {
	h.attempts.Add(1)
	return "", control.ErrMaintenanceHeld
}

func (h *maintenanceShutdownRefusal) HandleShutdown(int) string {
	h.fallback.Add(1)
	return "unexpected fallback"
}

func TestMaintenanceStopRefusalPreventsLegacyFallback(t *testing.T) {
	dir := shortTempDir(t, "ms")
	t.Setenv("TMPDIR", dir)
	handler := &maintenanceShutdownRefusal{}
	startFakeDaemon(t, dir, handler)
	legacy := &maintenanceShutdownRefusal{}
	endpoint, err := control.NewServer(serverid.ControlPath(dir, engineName, "legacy-owner"), legacy, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endpoint.Close)
	if code := runStop(0, true); code == 0 {
		t.Fatal("force stop succeeded despite maintenance refusal")
	}
	if handler.fallback.Load() != 0 || legacy.fallback.Load() != 0 || legacy.attempts.Load() != 0 {
		t.Fatal("maintenance refusal reached legacy shutdown fallback")
	}
}

func TestMaintenanceLauncherRestartRefusalIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name            string
		directError     bool
		shutdownRefusal bool
	}{
		{"typed-response", false, false}, {"wrapped-error", true, false}, {"shutdown-refusal", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateMaintenanceActivation(t)
			dir := shortTempDir(t, "mr")
			t.Setenv("TMPDIR", dir)
			t.Setenv("TEMP", dir)
			t.Setenv("TMP", dir)
			oldRunning, oldSend := launcherIsDaemonRunning, launcherControlSendWithTimeout
			oldExit, oldWait, oldStart := launcherWaitForDaemonExit, launcherWaitForDaemon, launcherStartDaemonProcessFrom
			t.Cleanup(func() {
				launcherIsDaemonRunning, launcherControlSendWithTimeout = oldRunning, oldSend
				launcherWaitForDaemonExit, launcherWaitForDaemon, launcherStartDaemonProcessFrom = oldExit, oldWait, oldStart
			})
			launcherIsDaemonRunning = func(string) bool { return true }
			launcherWaitForDaemonExit = func(string, string) { t.Fatal("refusal waited for daemon exit") }
			launcherWaitForDaemon = func(string, time.Duration) error { t.Fatal("refusal waited for successor"); return nil }
			launcherStartDaemonProcessFrom = func(string, string) error { t.Fatal("refusal started successor"); return nil }
			launcherControlSendWithTimeout = func(_ string, req control.Request, _ time.Duration) (*control.Response, error) {
				if req.Cmd == "status" {
					return &control.Response{OK: true, Data: []byte(`{"maintenance":[]}`)}, nil
				}
				if req.Cmd == "shutdown" && !tc.shutdownRefusal {
					t.Fatal("maintenance refusal fell back to shutdown")
				}
				if tc.shutdownRefusal && req.Cmd == "graceful-restart" {
					return &control.Response{Message: "old endpoint lacks graceful restart"}, nil
				}
				if tc.directError {
					return nil, fmt.Errorf("wrapped: %w", control.ErrMaintenanceHeld)
				}
				return &control.Response{ErrorCode: control.ErrMaintenanceHeld.Code}, nil
			}
			if err := restartDaemonAfterEngineSwitch(filepath.Join(dir, "launcher"), filepath.Join(dir, "engine"), true); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("force restart refusal=%v", err)
			}
		})
	}
}

func isolateMaintenanceActivation(t *testing.T) {
	t.Helper()
	dir := shortTempDir(t, "ma")
	t.Setenv("TMPDIR", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)
	config := t.TempDir()
	t.Setenv("APPDATA", config)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("HOME", config)
}

func TestMaintenanceUpgradeRefusesBeforeActivation(t *testing.T) {
	lease := control.MaintenanceResult{HoldID: "lease", ServerID: "owner", State: control.MaintenanceHeld, ExpiresAt: time.Now().Add(time.Minute).UTC(), DrainDeadline: time.Now().UTC(), TreesRetired: true}
	heldData, err := json.Marshal(map[string]any{"maintenance": []control.MaintenanceResult{lease}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"active", heldData, control.ErrMaintenanceHeld},
		{"failed-authority", []byte(`{"maintenance":[],"maintenance_error_code":"maintenance_persistence_failed"}`), control.ErrMaintenancePersistenceFailed},
		{"unknown-authority-code", []byte(`{"maintenance":[],"maintenance_error_code":"new-unsafe-state"}`), control.ErrMaintenanceInvalid},
		{"null-authority-code", []byte(`{"maintenance":[],"maintenance_error_code":null}`), control.ErrMaintenanceInvalid},
		{"malformed-leases", []byte(`{"maintenance":null}`), control.ErrMaintenanceInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateMaintenanceActivation(t)
			oldSend := launcherControlSendWithTimeout
			t.Cleanup(func() { launcherControlSendWithTimeout = oldSend })
			launcherControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
				competingLock, err := ipc.AcquireFileLock(serverid.DaemonLockPath("", engineName))
				if competingLock != nil {
					_ = competingLock.Close()
				}
				if !errors.Is(err, ipc.ErrFileLocked) {
					t.Fatalf("activation status ran without namespace serialization: %v", err)
				}
				return &control.Response{OK: true, Data: tc.data}, nil
			}
			dir := t.TempDir()
			launcher := filepath.Join(dir, launcherFileName())
			pending := launcher + "~"
			writeTestFile(t, launcher, "original launcher")
			writeTestFile(t, pending, "new engine")
			_, _, _, err := installVersionedEngineWithOptions(launcher, pending, versionedEngineInstallOptions{UpdateLauncher: true})
			if !errors.Is(err, tc.want) {
				t.Fatalf("activation refusal=%v, want %v", err, tc.want)
			}
			if content, err := os.ReadFile(launcher); err != nil || string(content) != "original launcher" {
				t.Fatalf("refusal swapped launcher: %q %v", content, err)
			}
			if content, err := os.ReadFile(pending); err != nil || string(content) != "new engine" {
				t.Fatalf("refusal consumed pending update: %q %v", content, err)
			}
			if _, err := os.Stat(versionStoreDir(launcher)); !os.IsNotExist(err) {
				t.Fatalf("refusal mutated activation layout: %v", err)
			}
		})
	}
}

func TestMaintenanceUpgradeCannotBypassNamespaceLock(t *testing.T) {
	isolateMaintenanceActivation(t)
	lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath("", engineName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	dir := t.TempDir()
	launcher := filepath.Join(dir, launcherFileName())
	pending := launcher + "~"
	writeTestFile(t, launcher, "original")
	writeTestFile(t, pending, "new engine")
	if _, _, _, err := installVersionedEngine(launcher, pending); !errors.Is(err, ipc.ErrFileLocked) {
		t.Fatalf("namespace contention=%v", err)
	}
	if err := restartDaemonAfterEngineSwitch(launcher, "new engine", true); !errors.Is(err, ipc.ErrFileLocked) {
		t.Fatalf("force restart bypassed lock: %v", err)
	}
	if content, err := os.ReadFile(launcher); err != nil || string(content) != "original" {
		t.Fatalf("contended activation swapped launcher: %q %v", content, err)
	}
	if _, err := os.Stat(versionStoreDir(launcher)); !os.IsNotExist(err) {
		t.Fatalf("contended activation mutated layout: %v", err)
	}
}
