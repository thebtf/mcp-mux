package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
		if timeout != 5*time.Second {
			t.Fatalf("timeout = %s, want 5s", timeout)
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
