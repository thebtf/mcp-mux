package control

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
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
	// Use short path — macOS limits Unix socket paths to 104 bytes.
	f, err := os.CreateTemp("", "mux-ctl-*.sock")
	if err != nil {
		t.Fatalf("create temp socket: %v", err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	return path
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

// mockDaemonHandler implements both CommandHandler and DaemonHandler for testing.
type mockDaemonHandler struct {
	mockHandler
	spawnCalled  bool
	removeCalled bool
	spawnErr     error
	removeErr    error
	spawnIPCPath string
	spawnSrvID   string
	removeArg    string
}

func (m *mockDaemonHandler) HandleSpawn(req Request) (string, string, string, error) {
	m.spawnCalled = true
	if m.spawnErr != nil {
		return "", "", "", m.spawnErr
	}
	ipcPath := m.spawnIPCPath
	if ipcPath == "" {
		ipcPath = "/tmp/fake.sock"
	}
	srvID := m.spawnSrvID
	if srvID == "" {
		srvID = "test-server-id"
	}
	return ipcPath, srvID, "test-token", nil
}

func (m *mockDaemonHandler) HandleRemove(serverID string) error {
	m.removeCalled = true
	m.removeArg = serverID
	return m.removeErr
}

func (m *mockDaemonHandler) HandleGracefulRestart(drainTimeoutMs int) (string, error) {
	return "/tmp/snapshot.json", nil
}

// TestSocketPath verifies SocketPath returns the address used at creation.
func TestSocketPath(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	got := srv.SocketPath()
	if got != path {
		t.Errorf("SocketPath = %q, want %q", got, path)
	}
}

// TestSpawnWithDaemonHandler verifies the spawn command dispatches to DaemonHandler.
func TestSpawnWithDaemonHandler(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{
		spawnIPCPath: "/tmp/spawned.sock",
		spawnSrvID:   "srv-abc",
	}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{
		Cmd:     "spawn",
		Command: "myserver",
		Args:    []string{"--flag"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Send spawn: %v", err)
	}
	if !resp.OK {
		t.Errorf("spawn not OK: %s", resp.Message)
	}
	if resp.Message != "spawned" {
		t.Errorf("spawn message = %q, want %q", resp.Message, "spawned")
	}
	if resp.IPCPath != "/tmp/spawned.sock" {
		t.Errorf("IPCPath = %q, want %q", resp.IPCPath, "/tmp/spawned.sock")
	}
	if resp.ServerID != "srv-abc" {
		t.Errorf("ServerID = %q, want %q", resp.ServerID, "srv-abc")
	}
	if !handler.spawnCalled {
		t.Error("HandleSpawn was not called")
	}
}

// TestSpawnWithoutDaemonHandler verifies spawn returns an error when handler is CommandHandler only.
func TestSpawnWithoutDaemonHandler(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "spawn", Command: "myserver"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.OK {
		t.Error("expected not OK when spawn not supported")
	}
	if resp.Message != "spawn not supported (not a daemon)" {
		t.Errorf("unexpected message: %s", resp.Message)
	}
}

// TestSpawnHandlerError verifies spawn error from DaemonHandler is propagated.
func TestSpawnHandlerError(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{spawnErr: fmt.Errorf("upstream unavailable")}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "spawn", Command: "myserver"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.OK {
		t.Error("expected not OK on spawn error")
	}
}

// TestRemoveWithDaemonHandler verifies the remove command dispatches to DaemonHandler.
func TestRemoveWithDaemonHandler(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "remove", Command: "srv-xyz"})
	if err != nil {
		t.Fatalf("Send remove: %v", err)
	}
	if !resp.OK {
		t.Errorf("remove not OK: %s", resp.Message)
	}
	if resp.Message != "removed" {
		t.Errorf("remove message = %q, want %q", resp.Message, "removed")
	}
	if !handler.removeCalled {
		t.Error("HandleRemove was not called")
	}
	if handler.removeArg != "srv-xyz" {
		t.Errorf("removeArg = %q, want %q", handler.removeArg, "srv-xyz")
	}
}

// TestRemoveWithoutDaemonHandler verifies remove returns an error when handler is CommandHandler only.
func TestRemoveWithoutDaemonHandler(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "remove", Command: "srv-xyz"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.OK {
		t.Error("expected not OK when remove not supported")
	}
	if resp.Message != "remove not supported (not a daemon)" {
		t.Errorf("unexpected message: %s", resp.Message)
	}
}

// TestRemoveHandlerError verifies remove error from DaemonHandler is propagated.
func TestRemoveHandlerError(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{removeErr: fmt.Errorf("not found")}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "remove", Command: "srv-xyz"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.OK {
		t.Error("expected not OK on remove error")
	}
}

// TestInvalidJSONRequest verifies that a malformed request produces an error response.
func TestInvalidJSONRequest(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	// Connect raw and send garbage JSON
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("not-valid-json\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	dec := json.NewDecoder(conn)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.OK {
		t.Error("expected not OK for invalid JSON request")
	}
}

// TestConcurrentConnections verifies the server handles multiple simultaneous clients correctly.
func TestConcurrentConnections(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	const numClients = 5
	var wg sync.WaitGroup
	errs := make([]error, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, sendErr := Send(path, Request{Cmd: "ping"})
			if sendErr != nil {
				errs[idx] = sendErr
				return
			}
			if !resp.OK || resp.Message != "pong" {
				errs[idx] = fmt.Errorf("client %d: unexpected response: %+v", idx, resp)
			}
		}(i)
	}

	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("client %d error: %v", i, e)
		}
	}
}

// TestSendWithTimeoutExpiry verifies that a very short timeout causes a timeout error.
func TestSendWithTimeoutExpiry(t *testing.T) {
	path := testSocketPath(t)

	// Server that accepts a connection but never responds (hangs after accept).
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		// Read the request but never write a response, so client times out.
		buf := make([]byte, 1024)
		conn.Read(buf) //nolint:errcheck
		time.Sleep(10 * time.Second)
		conn.Close()
	}()

	_, err = SendWithTimeout(path, Request{Cmd: "ping"}, 50*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

// TestCloseIdempotent verifies that calling Close twice does not panic or error.
func TestCloseIdempotent(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	srv.Close()
	srv.Close() // second close must be a no-op, not a panic
}
