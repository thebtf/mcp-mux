package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func TestRunStatusQueriesDaemonStatusDirectly(t *testing.T) {
	tempDir := shortTempDir(t, "status")
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	oldSend := statusControlSendWithTimeout
	t.Cleanup(func() { statusControlSendWithTimeout = oldSend })

	called := 0
	statusControlSendWithTimeout = func(path string, req control.Request, timeout time.Duration) (*control.Response, error) {
		called++
		if path != serverid.DaemonControlPath("", engineName) {
			t.Fatalf("status path = %q, want daemon control path", path)
		}
		if req.Cmd != "status" {
			t.Fatalf("request cmd = %q, want status", req.Cmd)
		}
		if timeout != statusDaemonControlTimeout {
			t.Fatalf("timeout = %s, want %s", timeout, statusDaemonControlTimeout)
		}
		data, err := json.Marshal(map[string]any{
			"daemon":      true,
			"owner_count": 3,
		})
		if err != nil {
			t.Fatalf("marshal status: %v", err)
		}
		return &control.Response{OK: true, Data: data}, nil
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if called != 1 {
		t.Fatalf("status sender called %d times, want exactly daemon status call", called)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `"daemon": true`) || !strings.Contains(out, `"owner_count": 3`) {
		t.Fatalf("stdout missing daemon status fields:\n%s", out)
	}
}

func TestRunStatusPreservesModernDaemonReadbackAndRedactsSensitiveDetails(t *testing.T) {
	tempDir := shortTempDir(t, "status-modern-direct")
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	oldSend := statusControlSendWithTimeout
	t.Cleanup(func() { statusControlSendWithTimeout = oldSend })

	called := 0
	statusControlSendWithTimeout = func(path string, req control.Request, timeout time.Duration) (*control.Response, error) {
		called++
		if path != serverid.DaemonControlPath("", engineName) {
			t.Fatalf("status path = %q, want daemon control path", path)
		}
		if req.Cmd != "status" {
			t.Fatalf("request cmd = %q, want status", req.Cmd)
		}
		if timeout != statusDaemonControlTimeout {
			t.Fatalf("timeout = %s, want %s", timeout, statusDaemonControlTimeout)
		}
		return &control.Response{OK: true, Data: mustMarshalCLIStatus(t, map[string]any{
			"daemon":      true,
			"owner_count": 1,
			"servers": []map[string]any{
				modernCLIStatusFixture("modern-direct-owner"),
			},
		})}, nil
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if called != 1 {
		t.Fatalf("status sender called %d times, want exactly daemon status call", called)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{`"daemon": true`, `"owner_count": 1`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	assertModernCLIStatusReadback(t, out, "modern-direct-owner")
}

func TestRunStatusCtlFallbackPreservesModernReadbackAndRedactsSensitiveDetails(t *testing.T) {
	tempDir := shortTempDir(t, "status-modern-ctl")
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	const ownerID = "modern-ctl-owner"
	ctlPath := filepath.Join(tempDir, ownSocketPrefix+ownerID+".ctl.sock")
	if err := os.WriteFile(ctlPath, []byte("control socket placeholder"), 0o600); err != nil {
		t.Fatalf("write control socket placeholder: %v", err)
	}

	oldSend := statusControlSendWithTimeout
	t.Cleanup(func() { statusControlSendWithTimeout = oldSend })
	daemonPath := serverid.DaemonControlPath("", engineName)
	called := 0
	statusControlSendWithTimeout = func(path string, req control.Request, timeout time.Duration) (*control.Response, error) {
		called++
		if req.Cmd != "status" {
			t.Fatalf("request cmd = %q, want status", req.Cmd)
		}
		switch path {
		case daemonPath:
			if timeout != statusDaemonControlTimeout {
				t.Fatalf("daemon timeout = %s, want %s", timeout, statusDaemonControlTimeout)
			}
			return nil, errors.New("forced unavailable")
		case ctlPath:
			if timeout != 5*time.Second {
				t.Fatalf("fallback timeout = %s, want 5s", timeout)
			}
			return &control.Response{OK: true, Data: mustMarshalCLIStatus(t, modernCLIStatusFixture(""))}, nil
		default:
			t.Fatalf("status path = %q, want daemon or owner control path", path)
			return nil, nil
		}
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if called != 2 {
		t.Fatalf("status sender called %d times, want daemon then owner control call", called)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var results []json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("fallback stdout is not a JSON array: %v\n%s", err, stdout.String())
	}
	if len(results) != 1 {
		t.Fatalf("fallback result count = %d, want 1: %s", len(results), stdout.String())
	}
	assertModernCLIStatusReadback(t, stdout.String(), ownerID)
}

func TestRunStatusLegacySocketFallbackKeepsExactLegacyShape(t *testing.T) {
	tempDir := shortTempDir(t, "status-legacy-socket")
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	const ownerID = "legacy-basic-owner"
	dataPath := filepath.Join(tempDir, ownSocketPrefix+ownerID+".sock")
	if err := os.WriteFile(dataPath, []byte("legacy data socket placeholder"), 0o600); err != nil {
		t.Fatalf("write legacy data socket placeholder: %v", err)
	}

	oldSend := statusControlSendWithTimeout
	t.Cleanup(func() { statusControlSendWithTimeout = oldSend })
	called := 0
	statusControlSendWithTimeout = func(path string, req control.Request, timeout time.Duration) (*control.Response, error) {
		called++
		if path != serverid.DaemonControlPath("", engineName) {
			t.Fatalf("status path = %q, want daemon control path", path)
		}
		if req.Cmd != "status" {
			t.Fatalf("request cmd = %q, want status", req.Cmd)
		}
		if timeout != statusDaemonControlTimeout {
			t.Fatalf("timeout = %s, want %s", timeout, statusDaemonControlTimeout)
		}
		return nil, errors.New("forced unavailable")
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if called != 1 {
		t.Fatalf("status sender called %d times, want only daemon status call", called)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var results []map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("legacy fallback stdout is not a JSON array: %v\n%s", err, stdout.String())
	}
	if len(results) != 1 {
		t.Fatalf("legacy fallback result count = %d, want 1: %s", len(results), stdout.String())
	}
	status := results[0]
	if len(status) != 4 {
		t.Fatalf("legacy fallback fields = %#v, want exactly server_id, ipc_path, active, legacy", status)
	}
	for _, key := range []string{"server_id", "ipc_path", "active", "legacy"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("legacy fallback missing %q: %#v", key, status)
		}
	}
	for _, key := range []string{"protocol_era", "sharing_policy", "cache_policy", "lifecycle_policy"} {
		if _, ok := status[key]; ok {
			t.Fatalf("legacy fallback fabricated modern %q: %#v", key, status)
		}
	}

	var gotID, gotPath string
	var active, legacy bool
	if err := json.Unmarshal(status["server_id"], &gotID); err != nil {
		t.Fatalf("decode server_id: %v", err)
	}
	if err := json.Unmarshal(status["ipc_path"], &gotPath); err != nil {
		t.Fatalf("decode ipc_path: %v", err)
	}
	if err := json.Unmarshal(status["active"], &active); err != nil {
		t.Fatalf("decode active: %v", err)
	}
	if err := json.Unmarshal(status["legacy"], &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if gotID != ownerID {
		t.Fatalf("server_id = %q, want %q", gotID, ownerID)
	}
	if gotPath != dataPath {
		t.Fatalf("ipc_path = %q, want %q", gotPath, dataPath)
	}
	if active {
		t.Fatalf("active = true for placeholder legacy socket %q, want false", dataPath)
	}
	if !legacy {
		t.Fatal("legacy = false, want true")
	}
}

func TestRunStatusReportsUnknownForInvalidSuccessfulDaemonJSON(t *testing.T) {
	tempDir := shortTempDir(t, "status-invalid-daemon-json")
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	oldSend := statusControlSendWithTimeout
	t.Cleanup(func() { statusControlSendWithTimeout = oldSend })
	called := 0
	statusControlSendWithTimeout = func(path string, req control.Request, timeout time.Duration) (*control.Response, error) {
		called++
		if path != serverid.DaemonControlPath("", engineName) {
			t.Fatalf("status path = %q, want daemon control path", path)
		}
		if req.Cmd != "status" {
			t.Fatalf("request cmd = %q, want status", req.Cmd)
		}
		if timeout != statusDaemonControlTimeout {
			t.Fatalf("timeout = %s, want %s", timeout, statusDaemonControlTimeout)
		}
		return &control.Response{OK: true, Data: json.RawMessage("not-json")}, nil
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if called != 1 {
		t.Fatalf("status sender called %d times, want exactly daemon status call", called)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "null") {
		t.Fatalf("stdout = %q, must never render invalid successful daemon data as null", out)
	}
	if strings.Contains(out, "No active mcp-mux instances found.") {
		t.Fatalf("stdout reported an empty active set for invalid successful daemon data:\n%s", out)
	}
	for _, want := range []string{"mcp-mux status unavailable", "Active state is unknown"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q for invalid successful daemon data:\n%s", want, out)
		}
	}
}

func TestRunStatusInvalidSuccessfulDaemonJSONDoesNotMergeLegacyFallback(t *testing.T) {
	tempDir := shortTempDir(t, "status-invalid-authoritative")
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)
	const ownerID = "legacy-must-not-mask-invalid-daemon"
	dataPath := filepath.Join(tempDir, ownSocketPrefix+ownerID+".sock")
	if err := os.WriteFile(dataPath, []byte("legacy socket placeholder"), 0o600); err != nil {
		t.Fatalf("write legacy socket placeholder: %v", err)
	}

	oldSend := statusControlSendWithTimeout
	t.Cleanup(func() { statusControlSendWithTimeout = oldSend })
	statusControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return &control.Response{OK: true, Data: json.RawMessage("not-json")}, nil
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	out := stdout.String()
	for _, want := range []string{"mcp-mux status unavailable", "Active state is unknown"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ownerID) || strings.Contains(out, `"legacy": true`) {
		t.Fatalf("invalid authoritative daemon status was masked by legacy fallback:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunStatusFallsBackWhenDaemonStatusUnavailable(t *testing.T) {
	tempDir := shortTempDir(t, "status-fallback")
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	oldSend := statusControlSendWithTimeout
	t.Cleanup(func() { statusControlSendWithTimeout = oldSend })
	called := 0
	statusControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		called++
		return nil, errors.New("forced unavailable")
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if called != 1 {
		t.Fatalf("status sender called %d times, want no retry for normal unavailable error", called)
	}
	if !strings.Contains(stdout.String(), "No active mcp-mux instances found.") {
		t.Fatalf("stdout = %q, want fallback empty message", stdout.String())
	}
}

func TestRunStatusReportsUnknownForAmbiguousDaemonFailureWithPipeHints(t *testing.T) {
	tempDir := shortTempDir(t, "status-unknown")
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	oldSend := statusControlSendWithTimeout
	oldPipeHints := statusPipeHints
	t.Cleanup(func() {
		statusControlSendWithTimeout = oldSend
		statusPipeHints = oldPipeHints
	})

	statusControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return nil, errors.New("control: read response: i/o timeout")
	}
	statusPipeHints = func() ([]string, error) {
		return []string{
			"mcp-mux-11111111111111111111111111111111",
			"mcp-mux-22222222222222222222222222222222",
		}, nil
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	out := stdout.String()
	if strings.Contains(out, "No active mcp-mux instances found.") {
		t.Fatalf("stdout reported empty active set for ambiguous daemon failure:\n%s", out)
	}
	for _, want := range []string{
		"mcp-mux status unavailable",
		"Active state is unknown",
		"Found 2 mcp-mux named-pipe endpoint(s)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunStatusRetriesDaemonStatusPipeBusyBeforeFallback(t *testing.T) {
	tempDir := shortTempDir(t, "status-retry")
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	oldSend := statusControlSendWithTimeout
	oldWindow := statusDaemonRetryWindow
	oldDelay := statusDaemonRetryDelay
	oldSleep := statusSleep
	t.Cleanup(func() {
		statusControlSendWithTimeout = oldSend
		statusDaemonRetryWindow = oldWindow
		statusDaemonRetryDelay = oldDelay
		statusSleep = oldSleep
	})

	statusDaemonRetryWindow = time.Second
	statusDaemonRetryDelay = time.Millisecond
	statusSleep = func(time.Duration) {}

	called := 0
	statusControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		called++
		if called < 3 {
			return nil, errors.New("control: dial pipe: All pipe instances are busy.")
		}
		data, err := json.Marshal(map[string]any{
			"daemon":      true,
			"owner_count": 7,
		})
		if err != nil {
			t.Fatalf("marshal status: %v", err)
		}
		return &control.Response{OK: true, Data: data}, nil
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if called != 3 {
		t.Fatalf("status sender called %d times, want retries until daemon status succeeds", called)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `"daemon": true`) || !strings.Contains(out, `"owner_count": 7`) {
		t.Fatalf("stdout missing daemon status after retry:\n%s", out)
	}
}

func TestRunStatusRetriesDaemonStatusAccessDeniedBeforeFallback(t *testing.T) {
	tempDir := shortTempDir(t, "status-access-denied")
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)

	oldSend := statusControlSendWithTimeout
	oldWindow := statusDaemonRetryWindow
	oldDelay := statusDaemonRetryDelay
	oldSleep := statusSleep
	t.Cleanup(func() {
		statusControlSendWithTimeout = oldSend
		statusDaemonRetryWindow = oldWindow
		statusDaemonRetryDelay = oldDelay
		statusSleep = oldSleep
	})

	statusDaemonRetryWindow = time.Second
	statusDaemonRetryDelay = time.Millisecond
	statusSleep = func(time.Duration) {}

	called := 0
	statusControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		called++
		if called < 3 {
			return nil, errors.New(`control: dial pipe: open \\.\pipe\mcp-mux-test: Access is denied.`)
		}
		data, err := json.Marshal(map[string]any{
			"daemon":      true,
			"owner_count": 11,
		})
		if err != nil {
			t.Fatalf("marshal status: %v", err)
		}
		return &control.Response{OK: true, Data: data}, nil
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if called != 3 {
		t.Fatalf("status sender called %d times, want retries until daemon status succeeds", called)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `"daemon": true`) || !strings.Contains(out, `"owner_count": 11`) {
		t.Fatalf("stdout missing daemon status after access-denied retry:\n%s", out)
	}
}

const cliStatusSensitiveSentinel = "private-cli-status-sentinel"

func modernCLIStatusFixture(serverID string) map[string]any {
	return map[string]any{
		"server_id":                  serverID,
		"command":                    "safe-status-command",
		"args":                       []string{"--safe-status-arg"},
		"cwd":                        "C:/safe-status-cwd",
		"cwd_set":                    []string{"C:/safe-status-cwd"},
		"upstream_pid":               4242,
		"auto_classification":        "isolated",
		"classification_source":      "explicit-policy",
		"classification_reason":      []string{"operator-selected"},
		"mux_version":                "test-version",
		"persistent":                 false,
		"cached_init":                false,
		"cached_tools":               false,
		"cached_prompts":             false,
		"cached_resources":           false,
		"cache_ready":                false,
		"upstream_live":              true,
		"session_count":              2,
		"pending_requests":           1,
		"materialization_state":      "direct",
		"materialization_policy":     "on-demand",
		"materialization_generation": 3,
		"protocol_era":               "2026-07-28",
		"sharing_policy":             "forced-isolated",
		"cache_policy":               "off",
		"lifecycle_policy":           "r1-quarantine",
		"sessions":                   []string{cliStatusSensitiveSentinel},
		"inflight": []map[string]any{
			{"route": cliStatusSensitiveSentinel},
		},
		"oldest_request_age_ms": 123,
		"finalization_error":    cliStatusSensitiveSentinel,
	}
}

func mustMarshalCLIStatus(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	return data
}

func assertModernCLIStatusReadback(t *testing.T, out, serverID string) {
	t.Helper()
	for _, want := range []string{
		`"server_id": "` + serverID + `"`,
		`"protocol_era": "2026-07-28"`,
		`"sharing_policy": "forced-isolated"`,
		`"cache_policy": "off"`,
		`"lifecycle_policy": "r1-quarantine"`,
		`"command": "safe-status-command"`,
		`"args": [`,
		`"--safe-status-arg"`,
		`"cwd": "C:/safe-status-cwd"`,
		`"upstream_pid": 4242`,
		`"auto_classification": "isolated"`,
		`"classification_source": "explicit-policy"`,
		`"mux_version": "test-version"`,
		`"persistent": false`,
		`"cached_init": false`,
		`"cached_tools": false`,
		`"cached_prompts": false`,
		`"cached_resources": false`,
		`"cache_ready": false`,
		`"upstream_live": true`,
		`"session_count": 2`,
		`"pending_requests": 1`,
		`"materialization_state": "direct"`,
		`"materialization_policy": "on-demand"`,
		`"materialization_generation": 3`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing preserved modern status field %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{
		cliStatusSensitiveSentinel,
		`"sessions"`,
		`"inflight"`,
		`"oldest_request_age_ms"`,
		`"finalization_error"`,
	} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("stdout exposed prohibited modern status detail %q:\n%s", forbidden, out)
		}
	}
}
