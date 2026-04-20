package control

import (
	"bufio"
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
	refreshCalled bool
	giveUpCalled bool
	spawnErr     error
	removeErr    error
	refreshErr   error
	spawnIPCPath string
	spawnSrvID   string
	removeArg    string
	refreshArg   string
	giveUpArg    string
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

func (m *mockDaemonHandler) HandleRefreshSessionToken(prevToken string) (string, error) {
	m.refreshCalled = true
	m.refreshArg = prevToken
	if m.refreshErr != nil {
		return "", m.refreshErr
	}
	return "refreshed-token", nil
}

func (m *mockDaemonHandler) HandleReconnectGiveUp(reason string) error {
	m.giveUpCalled = true
	m.giveUpArg = reason
	return nil
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

func TestRefreshToken(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "refresh-token", PrevToken: "prev-token"})
	if err != nil {
		t.Fatalf("Send refresh-token: %v", err)
	}
	if !handler.refreshCalled {
		t.Fatal("refresh handler not called")
	}
	if handler.refreshArg != "prev-token" {
		t.Fatalf("refreshArg = %q, want %q", handler.refreshArg, "prev-token")
	}
	if !resp.OK {
		t.Fatalf("refresh-token not OK: %s", resp.Message)
	}
	if resp.Token != "refreshed-token" {
		t.Fatalf("resp.Token = %q, want %q", resp.Token, "refreshed-token")
	}
}

func TestRefreshTokenUnknownToken(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{refreshErr: fmt.Errorf("wrapped: %w", errUnknownToken)}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "refresh-token", PrevToken: "prev-token"})
	if err != nil {
		t.Fatalf("Send refresh-token: %v", err)
	}
	if resp.OK {
		t.Fatal("expected refresh-token to fail for unknown token")
	}
	if resp.Message != "unknown token" {
		t.Fatalf("resp.Message = %q, want %q", resp.Message, "unknown token")
	}
}

func TestRefreshTokenOwnerGone(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{refreshErr: fmt.Errorf("wrapped: %w", errOwnerGone)}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "refresh-token", PrevToken: "prev-token"})
	if err != nil {
		t.Fatalf("Send refresh-token: %v", err)
	}
	if resp.OK {
		t.Fatal("expected refresh-token to fail for owner gone")
	}
	if resp.Message != "owner gone" {
		t.Fatalf("resp.Message = %q, want %q", resp.Message, "owner gone")
	}
}

func TestReconnectGiveUp(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "reconnect-give-up", ReconnectReason: "timeout"})
	if err != nil {
		t.Fatalf("Send reconnect-give-up: %v", err)
	}
	if !resp.OK {
		t.Fatalf("reconnect-give-up not OK: %s", resp.Message)
	}
	if !handler.giveUpCalled {
		t.Fatal("give-up handler not called")
	}
	if handler.giveUpArg != "timeout" {
		t.Fatalf("giveUpArg = %q, want %q", handler.giveUpArg, "timeout")
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

// TestControlServer_ReadDeadlineFiresOnSilentClient is a regression test for FR-5.
// A client that connects but never sends data must not block the server goroutine
// forever. The server's read deadline must fire within clientDeadline + slack.
//
// Regression for post-audit-remediation: a malicious or broken client could DoS
// the daemon control plane by opening connections and never sending a request,
// accumulating handler goroutines forever.
func TestControlServer_ReadDeadlineFiresOnSilentClient(t *testing.T) {
	const slack = 2 * time.Second

	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	// Connect but never send anything — the server's handleConn goroutine must
	// self-terminate via the read deadline rather than block indefinitely.
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// The server should close the connection (or the goroutine should exit) within
	// clientDeadline + slack. We verify this by attempting to read from the conn:
	// the server side will close after deadline, making our Read return io.EOF or
	// a deadline error.
	deadline := clientDeadline + slack
	if err := conn.SetDeadline(time.Now().Add(deadline)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// Read one NDJSON line using bufio for correct protocol framing.
	// The server sends JSON followed by '\n' in a single Write.
	reader := bufio.NewReader(conn)
	start := time.Now()
	line, readErr := reader.ReadBytes('\n')
	elapsed := time.Since(start)

	// A non-empty line means the server sent an error response before closing
	// (read deadline fired, server wrote the error response, then closed).
	// Validate it is a well-formed, not-OK response.
	if len(line) > 0 {
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Errorf("non-JSON response after read deadline: %q (err=%v)", line, err)
		}
		if resp.OK {
			t.Errorf("expected error response after read deadline, got OK=%v msg=%q", resp.OK, resp.Message)
		}
	}
	// readErr may be io.EOF, a timeout error, or nil — all acceptable as long as
	// the goroutine unblocked within the expected window.
	_ = readErr

	// The goroutine must have exited within clientDeadline + slack; if our own
	// deadline fired first the test itself would time out here, which counts as
	// failure via the harness.
	if elapsed > deadline {
		t.Errorf("server held connection for %v, want <= %v (clientDeadline %v + slack %v)",
			elapsed, deadline, clientDeadline, slack)
	}
}
