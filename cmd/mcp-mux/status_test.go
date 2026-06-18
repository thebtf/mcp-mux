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
	statusControlSendWithTimeout = func(string, control.Request, time.Duration) (*control.Response, error) {
		return nil, errors.New("forced unavailable")
	}

	var stdout, stderr bytes.Buffer
	runStatusWithWriters(&stdout, &stderr)
	if !strings.Contains(stdout.String(), "No active mcp-mux instances found.") {
		t.Fatalf("stdout = %q, want fallback empty message", stdout.String())
	}
}
