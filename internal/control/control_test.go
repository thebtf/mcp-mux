package control

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockHandler implements CommandHandler for testing.
type mockHandler struct {
	shutdownCalled bool
	drainTimeout   int
}

func (m *mockHandler) HandleShutdown(drainTimeoutMs int) string {
	m.shutdownCalled = true
	m.drainTimeout = drainTimeoutMs
	return "shutdown initiated"
}

func (m *mockHandler) HandleStatus() map[string]any {
	return map[string]any{
		"upstream_pid":     1234,
		"session_count":    2,
		"pending_requests": 0,
	}
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test-ctl.sock")
}

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(os.Stderr, "[control-test] ", log.LstdFlags)
}

func TestPing(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "ping"})
	if err != nil {
		t.Fatalf("Send ping: %v", err)
	}
	if !resp.OK {
		t.Errorf("ping not OK: %s", resp.Message)
	}
	if resp.Message != "pong" {
		t.Errorf("ping message = %q, want %q", resp.Message, "pong")
	}
}

func TestStatus(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "status"})
	if err != nil {
		t.Fatalf("Send status: %v", err)
	}
	if !resp.OK {
		t.Errorf("status not OK: %s", resp.Message)
	}

	var data map[string]any
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("unmarshal status data: %v", err)
	}

	pid, ok := data["upstream_pid"]
	if !ok {
		t.Error("status missing upstream_pid")
	}
	if pid != float64(1234) {
		t.Errorf("upstream_pid = %v, want 1234", pid)
	}
}

func TestShutdown(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "shutdown", DrainTimeoutMs: 5000})
	if err != nil {
		t.Fatalf("Send shutdown: %v", err)
	}
	if !resp.OK {
		t.Errorf("shutdown not OK: %s", resp.Message)
	}
	if !handler.shutdownCalled {
		t.Error("shutdown handler not called")
	}
	if handler.drainTimeout != 5000 {
		t.Errorf("drain timeout = %d, want 5000", handler.drainTimeout)
	}
}

func TestUnknownCommand(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "nonexistent"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.OK {
		t.Error("expected not OK for unknown command")
	}
}

func TestClientTimeout(t *testing.T) {
	path := testSocketPath(t)
	// No server listening — connection should fail
	_, err := Send(path, Request{Cmd: "ping"})
	if err == nil {
		t.Error("expected error connecting to non-existent socket")
	}
}

func TestSendWithTimeout(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := SendWithTimeout(path, Request{Cmd: "ping"}, 10*time.Second)
	if err != nil {
		t.Fatalf("SendWithTimeout: %v", err)
	}
	if !resp.OK || resp.Message != "pong" {
		t.Errorf("unexpected response: %+v", resp)
	}
}
