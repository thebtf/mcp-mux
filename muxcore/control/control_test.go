package control

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
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

type fakeAddr string

func (a fakeAddr) Network() string { return "fake" }
func (a fakeAddr) String() string  { return string(a) }

type acceptErrorListener struct {
	mu      sync.Mutex
	closed  bool
	err     error
	accepts int
}

func (l *acceptErrorListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accepts++
	if l.closed {
		return nil, net.ErrClosed
	}
	return nil, l.err
}

func (l *acceptErrorListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func (l *acceptErrorListener) Addr() net.Addr { return fakeAddr("accept-error-listener") }

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
	spawnCalled   bool
	spawnReq      Request
	removeCalled  bool
	stopCalled    bool
	refreshCalled bool
	giveUpCalled  bool
	spawnErr      error
	removeErr     error
	stopErr       error
	refreshErr    error
	spawnIPCPath  string
	spawnSrvID    string
	removeArg     string
	stopReq       Request
	refreshArg    string
	giveUpArg     string
	spawnStarted  chan struct{}
	spawnRelease  <-chan struct{}
	rollbackCh    chan struct{}
	rollbackSID   string
	rollbackToken string
}

func (m *mockDaemonHandler) HandleSpawn(req Request) (string, string, string, error) {
	m.spawnReq = req
	m.spawnCalled = true
	if m.spawnStarted != nil {
		close(m.spawnStarted)
	}
	if m.spawnRelease != nil {
		<-m.spawnRelease
	}
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

func (m *mockDaemonHandler) HandleSpawnResponseFailure(serverID, token string) {
	m.rollbackSID = serverID
	m.rollbackToken = token
	if m.rollbackCh != nil {
		close(m.rollbackCh)
	}
}

func (m *mockDaemonHandler) HandleRemove(serverID string) error {
	m.removeCalled = true
	m.removeArg = serverID
	return m.removeErr
}

func (m *mockDaemonHandler) HandleStopOwner(req Request) (string, error) {
	m.stopCalled = true
	m.stopReq = req
	if m.stopErr != nil {
		return "", m.stopErr
	}
	return "stopped through daemon", nil
}

func (m *mockDaemonHandler) HandleGracefulRestart(drainTimeoutMs int) (string, func(), error) {
	return "/tmp/snapshot.json", nil, nil
}

func (m *mockDaemonHandler) HandleRefreshSessionToken(prevToken string) (string, error) {
	m.refreshCalled = true
	m.refreshArg = prevToken
	if m.refreshErr != nil {
		return "", m.refreshErr
	}
	return "refreshed-token", nil
}

type modernRefreshDaemonHandler struct {
	mockDaemonHandler
	protocolEra string
}

func (m *modernRefreshDaemonHandler) HandleRefreshSessionTokenWithProtocolEra(prevToken, protocolEra string) (string, error) {
	m.protocolEra = protocolEra
	if m.refreshErr != nil {
		return "", m.refreshErr
	}
	return "modern-refreshed-token", nil
}

func (m *mockDaemonHandler) HandleReconnectGiveUp(reason string) error {
	m.giveUpCalled = true
	m.giveUpArg = reason
	return nil
}

func (m *mockDaemonHandler) HandleListOwners(req Request) (ListOwnersResponse, error) {
	return ListOwnersResponse{}, nil
}

type mockGracefulOptionsHandler struct {
	mockDaemonHandler
	optsCalled bool
	opts       GracefulRestartOptions
}

func (m *mockGracefulOptionsHandler) HandleGracefulRestartWithOptions(opts GracefulRestartOptions) (string, func(), error) {
	m.optsCalled = true
	m.opts = opts
	return "/tmp/options-snapshot.json", nil, nil
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

func TestGracefulRestartPassesSuccessorExeToOptionsHandler(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockGracefulOptionsHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{
		Cmd:            "graceful-restart",
		DrainTimeoutMs: 1234,
		SuccessorExe:   "/tmp/new-engine",
	})
	if err != nil {
		t.Fatalf("Send graceful-restart: %v", err)
	}
	if !resp.OK {
		t.Fatalf("graceful-restart not OK: %s", resp.Message)
	}
	if !handler.optsCalled {
		t.Fatal("HandleGracefulRestartWithOptions was not called")
	}
	if handler.opts.DrainTimeoutMs != 1234 || handler.opts.SuccessorExe != "/tmp/new-engine" {
		t.Fatalf("options = %+v, want drain=1234 successor=/tmp/new-engine", handler.opts)
	}
	if resp.IPCPath != "/tmp/options-snapshot.json" {
		t.Fatalf("snapshot path = %q, want /tmp/options-snapshot.json", resp.IPCPath)
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

func TestRefreshTokenWithModernEraUsesOptionalHandlerAndEchoesEra(t *testing.T) {
	const modernProtocolEra = "2026-07-28"
	path := testSocketPath(t)
	handler := &modernRefreshDaemonHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "refresh-token", PrevToken: "prev-token", ProtocolEra: modernProtocolEra})
	if err != nil {
		t.Fatalf("Send modern refresh-token: %v", err)
	}
	if !resp.OK || resp.Token != "modern-refreshed-token" || resp.ProtocolEra != modernProtocolEra {
		t.Fatalf("modern refresh response = %+v", resp)
	}
	if handler.protocolEra != modernProtocolEra {
		t.Fatalf("modern refresh era = %q, want %q", handler.protocolEra, modernProtocolEra)
	}
	if handler.refreshCalled {
		t.Fatal("modern refresh fell through to legacy handler")
	}
}

func TestRefreshTokenRejectsUnsupportedEraWithoutLegacyFallback(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "refresh-token", PrevToken: "prev-token", ProtocolEra: "unknown-era"})
	if err != nil {
		t.Fatalf("Send unsupported refresh-token: %v", err)
	}
	if resp.OK {
		t.Fatalf("unsupported era refresh succeeded: %+v", resp)
	}
	if handler.refreshCalled {
		t.Fatal("unsupported era refresh fell through to legacy handler")
	}
}

func TestAcceptLoopBacksOffAfterTransientAcceptError(t *testing.T) {
	ln := &acceptErrorListener{err: errors.New("boom")}
	var logs bytes.Buffer
	srv := &Server{
		listener: ln,
		handler:  &mockHandler{},
		logger:   log.New(&logs, "", 0),
		done:     make(chan struct{}),
	}

	origSleep := sleepAfterAcceptError
	t.Cleanup(func() { sleepAfterAcceptError = origSleep })
	slept := make(chan time.Duration, 1)
	sleepAfterAcceptError = func(d time.Duration) {
		slept <- d
		srv.mu.Lock()
		srv.closed = true
		srv.mu.Unlock()
		_ = ln.Close()
	}

	done := make(chan struct{})
	go func() {
		srv.acceptLoop()
		close(done)
	}()

	select {
	case got := <-slept:
		if got != acceptErrorBackoff {
			t.Fatalf("accept error backoff = %v, want %v", got, acceptErrorBackoff)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acceptLoop did not back off after transient accept error")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acceptLoop did not exit after listener close")
	}
	if !strings.Contains(logs.String(), "control: accept error: boom") {
		t.Fatalf("missing accept error log, got %q", logs.String())
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
	if got := handler.spawnReq.ProtocolEra; got != "" {
		t.Errorf("legacy spawn forwarded protocol era = %q, want omitted", got)
	}
	if got := resp.ProtocolEra; got != "" {
		t.Errorf("legacy spawn response protocol era = %q, want omitted", got)
	}
	wire, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal legacy spawn response: %v", err)
	}
	if bytes.Contains(wire, []byte(`"protocol_era"`)) {
		t.Errorf("legacy spawn response wire = %s, want protocol_era omitted", wire)
	}
	if !handler.spawnCalled {
		t.Error("HandleSpawn was not called")
	}
	if handler.rollbackSID != "" || handler.rollbackToken != "" {
		t.Fatalf("successful spawn unexpectedly rolled back (%q, %q)", handler.rollbackSID, handler.rollbackToken)
	}
}

func TestSpawnWithDaemonHandlerEchoesExactModernProtocolEra(t *testing.T) {
	const modernProtocolEra = "2026-07-28"

	path := testSocketPath(t)
	handler := &mockDaemonHandler{
		spawnIPCPath: "/tmp/modern.sock",
		spawnSrvID:   "srv-modern",
	}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{
		Cmd:         "spawn",
		Command:     "myserver",
		Args:        []string{"--flag"},
		Mode:        "global",
		ProtocolEra: modernProtocolEra,
	})
	if err != nil {
		t.Fatalf("Send modern spawn: %v", err)
	}
	if !resp.OK {
		t.Fatalf("modern spawn not OK: %s", resp.Message)
	}
	if got := handler.spawnReq.ProtocolEra; got != modernProtocolEra {
		t.Errorf("HandleSpawn protocol era = %q, want exact %q", got, modernProtocolEra)
	}
	if got := resp.ProtocolEra; got != modernProtocolEra {
		t.Errorf("modern spawn response protocol era = %q, want exact %q", got, modernProtocolEra)
	}
}

func TestUndeliveredSpawnResponseRollsBackReservation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	rolledBack := make(chan struct{})
	handler := &mockDaemonHandler{
		spawnIPCPath: "/tmp/spawned.sock",
		spawnSrvID:   "srv-undelivered",
		spawnStarted: started,
		spawnRelease: release,
		rollbackCh:   rolledBack,
	}
	srv := &Server{handler: handler, logger: testLogger(t)}
	serverConn, clientConn := net.Pipe()
	srv.wg.Add(1)
	done := make(chan struct{})
	go func() {
		srv.handleConn(serverConn)
		close(done)
	}()

	request, err := json.Marshal(Request{Cmd: "spawn", Command: "fixture"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request = append(request, '\n')
	if _, err := clientConn.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("spawn handler was not entered")
	}
	_ = clientConn.Close()
	close(release)

	select {
	case <-rolledBack:
	case <-time.After(time.Second):
		t.Fatal("undelivered spawn response did not trigger rollback")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("control handler did not finish")
	}
	if handler.rollbackSID != "srv-undelivered" || handler.rollbackToken != "test-token" {
		t.Fatalf("rollback = (%q, %q), want exact spawn reservation", handler.rollbackSID, handler.rollbackToken)
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

func TestStopOwnerWithOptionalDaemonHandler(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockDaemonHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "stop_owner", ServerID: "srv-xyz", DrainTimeoutMs: 30000})
	if err != nil {
		t.Fatalf("Send stop_owner: %v", err)
	}
	if !resp.OK {
		t.Errorf("stop_owner not OK: %s", resp.Message)
	}
	if resp.Message != "stopped through daemon" {
		t.Errorf("stop_owner message = %q, want %q", resp.Message, "stopped through daemon")
	}
	if !handler.stopCalled {
		t.Error("HandleStopOwner was not called")
	}
	if handler.stopReq.ServerID != "srv-xyz" {
		t.Errorf("stopReq.ServerID = %q, want %q", handler.stopReq.ServerID, "srv-xyz")
	}
	if handler.stopReq.DrainTimeoutMs != 30000 {
		t.Errorf("stopReq.DrainTimeoutMs = %d, want 30000", handler.stopReq.DrainTimeoutMs)
	}
}

func TestStopOwnerWithoutOptionalHandler(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	resp, err := Send(path, Request{Cmd: "stop_owner", ServerID: "srv-xyz"})
	if err != nil {
		t.Fatalf("Send stop_owner: %v", err)
	}
	if resp.OK {
		t.Error("expected not OK when stop_owner is not supported")
	}
	if resp.Message != "stop_owner not supported (not a daemon)" {
		t.Errorf("unexpected message: %s", resp.Message)
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
	conn, err := ipc.DialTimeout(path, 5*time.Second)
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

func TestPausedServerDoesNotDispatchUntilStart(t *testing.T) {
	path := testSocketPath(t)
	srv, err := NewPausedServer(path, &mockHandler{}, testLogger(t))
	if err != nil {
		t.Fatalf("NewPausedServer: %v", err)
	}
	defer srv.Close()

	result := make(chan error, 1)
	go func() {
		resp, sendErr := SendWithTimeout(path, Request{Cmd: "status"}, 2*time.Second)
		if sendErr == nil && !resp.OK {
			sendErr = fmt.Errorf("status response not OK: %s", resp.Message)
		}
		result <- sendErr
	}()
	select {
	case err := <-result:
		t.Fatalf("paused server dispatched before Start: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	srv.Start()
	srv.Start()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("status after Start: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("paused server did not dispatch after Start")
	}
	srv.Close()
	srv.Close()
	srv.Start()
}

func TestCloseRemovesSocketPath(t *testing.T) {
	path := testSocketPath(t)
	handler := &mockHandler{}
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	srv.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket path after Close: stat err = %v, want not exist", err)
	}
}

type panicAcceptListener struct {
	value any
}

func (l panicAcceptListener) Accept() (net.Conn, error) {
	panic(l.value)
}

func (l panicAcceptListener) Close() error {
	return nil
}

func (l panicAcceptListener) Addr() net.Addr {
	return dummyAddr("panic-listener")
}

type dummyAddr string

func (a dummyAddr) Network() string {
	return "test"
}

func (a dummyAddr) String() string {
	return string(a)
}

func TestAcceptLoopClosedServerRecoversAcceptPanic(t *testing.T) {
	srv := &Server{
		listener: panicAcceptListener{value: "The handle is invalid."},
		logger:   testLogger(t),
		done:     make(chan struct{}),
	}
	srv.closed = true

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.acceptLoop()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("acceptLoop did not exit after closed-listener panic")
	}
}

func TestAcceptLoopUnexpectedPanicPropagates(t *testing.T) {
	srv := &Server{
		listener: panicAcceptListener{value: "unexpected accept panic"},
		logger:   testLogger(t),
		done:     make(chan struct{}),
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("acceptLoop swallowed unexpected panic")
		}
	}()

	srv.acceptLoop()
}

type errorAcceptListener struct {
	err error
}

func (l errorAcceptListener) Accept() (net.Conn, error) {
	return nil, l.err
}

func (l errorAcceptListener) Close() error {
	return nil
}

func (l errorAcceptListener) Addr() net.Addr {
	return dummyAddr("error-listener")
}

func TestAcceptLoopUnexpectedNetErrClosedLogs(t *testing.T) {
	var buf bytes.Buffer
	srv := &Server{
		listener: errorAcceptListener{err: net.ErrClosed},
		logger:   log.New(&buf, "", 0),
		done:     make(chan struct{}),
	}

	srv.acceptLoop()

	if got := buf.String(); !strings.Contains(got, "unexpected listener close") {
		t.Fatalf("missing unexpected close log, got %q", got)
	}
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
	conn, err := ipc.DialTimeout(path, 5*time.Second)
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

func TestProtocolEraJSONCompatibility(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "legacy request omits era",
			value: Request{Cmd: "spawn"},
			want:  `{"cmd":"spawn"}`,
		},
		{
			name:  "modern request writes exact era",
			value: Request{Cmd: "spawn", ProtocolEra: "2026-07-28"},
			want:  `{"cmd":"spawn","protocol_era":"2026-07-28"}`,
		},
		{
			name:  "legacy response omits era",
			value: Response{OK: true},
			want:  `{"ok":true}`,
		},
		{
			name:  "modern response writes exact era",
			value: Response{OK: true, ProtocolEra: "2026-07-28"},
			want:  `{"ok":true,"protocol_era":"2026-07-28"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatalf("json.Marshal(%T) error = %v", tt.value, err)
			}
			if string(got) != tt.want {
				t.Fatalf("json.Marshal(%T) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}
}

func TestProtocolEraUnknownJSONFieldsRemainTolerated(t *testing.T) {
	var request Request
	if err := json.Unmarshal([]byte(`{"cmd":"spawn","unknown_request_field":true}`), &request); err != nil {
		t.Fatalf("unmarshal request with unknown field: %v", err)
	}
	if request.Cmd != "spawn" {
		t.Fatalf("request command = %q, want spawn", request.Cmd)
	}

	var response Response
	if err := json.Unmarshal([]byte(`{"ok":true,"unknown_response_field":true}`), &response); err != nil {
		t.Fatalf("unmarshal response with unknown field: %v", err)
	}
	if !response.OK {
		t.Fatal("response OK = false, want true")
	}
}

type maintenanceBoundaryHandler struct {
	mockHandler
	calls  int
	handle func(Request) (MaintenanceResult, error)
}

func (m *maintenanceBoundaryHandler) HandleMaintenance(req Request) (MaintenanceResult, error) {
	m.calls++
	return m.handle(req)
}

func maintenanceTestResult() MaintenanceResult {
	now := time.Now().UTC()
	return MaintenanceResult{
		HoldID:        "lease-exact",
		ServerID:      "owner-exact",
		State:         MaintenanceHeld,
		ExpiresAt:     now.Add(5 * time.Minute),
		DrainDeadline: now,
		TreesRetired:  true,
	}
}

func TestMaintenanceTypedErrors(t *testing.T) {
	result := maintenanceTestResult()
	for _, sentinel := range []*MaintenanceError{
		ErrMaintenanceHeld, ErrMaintenanceConflict, ErrMaintenanceNotFound,
		ErrMaintenanceRetirementBlocked, ErrMaintenanceUnsupported,
		ErrMaintenancePersistenceFailed, ErrMaintenanceInvalid,
	} {
		t.Run(string(sentinel.Code), func(t *testing.T) {
			wrapped := fmt.Errorf("private context secret: %w", &MaintenanceError{Code: sentinel.Code, Result: &result})
			if !errors.Is(wrapped, sentinel) {
				t.Fatalf("wrapped refusal does not match %v", sentinel)
			}
			resp := errorResponse("spawn", wrapped)
			wire, err := json.Marshal(resp)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "private") || strings.Contains(string(wire), "secret") {
				t.Fatalf("internal reason escaped onto wire: %s", wire)
			}
			var decoded Response
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			err = fmt.Errorf("consumer: %w", decoded.Err())
			var typed *MaintenanceError
			if !errors.Is(err, sentinel) || !errors.As(err, &typed) || typed.Result == nil || typed.Result.HoldID != result.HoldID {
				t.Fatalf("typed refusal or safe lease lost: %v", err)
			}
			if errors.Is(err, &MaintenanceError{Code: "different"}) {
				t.Fatal("different refusal code matched")
			}
		})
	}
	for _, resp := range []*Response{
		nil,
		{OK: true, ErrorCode: ErrMaintenanceHeld.Code},
		{OK: true, ErrorCode: "future_secret_code"},
		{ErrorCode: "future_secret_code", Message: "private context"},
		{OK: true, Maintenance: &MaintenanceResult{HoldID: "lease-exact", State: MaintenanceHeld}},
	} {
		if err := resp.Err(); !errors.Is(err, ErrMaintenanceInvalid) {
			t.Fatalf("malformed response accepted: %+v, err=%v", resp, err)
		}
	}
	if err := (&Response{OK: true}).Err(); err != nil {
		t.Fatalf("legacy success refused: %v", err)
	}
	if err := (&Response{Message: "ordinary failure"}).Err(); err == nil || err.Error() != "ordinary failure" {
		t.Fatalf("legacy failure changed: %v", err)
	}
	if err := (&Response{}).Err(); err == nil {
		t.Fatal("empty unsuccessful response accepted")
	}
}

func TestMaintenanceInvalidInputsDoNotReachHandler(t *testing.T) {
	zero, negative, tooLong := int64(0), int64(-1), int64(3600001)
	requests := []Request{
		{Cmd: "hold"},
		{Cmd: "hold", Command: "owner-exact"},
		{Cmd: "hold", ServerID: "owner-exact", Command: "different"},
		{Cmd: "hold", ServerID: " owner-exact"},
		{Cmd: "hold", ServerID: "owner-exact", HoldID: "lease-exact"},
		{Cmd: "hold", ServerID: "owner-exact", HoldTTLMS: &zero},
		{Cmd: "hold", ServerID: "owner-exact", HoldTTLMS: &negative},
		{Cmd: "hold", ServerID: "owner-exact", HoldTTLMS: &tooLong},
		{Cmd: "hold", ServerID: "owner-exact", DrainTimeoutMs: -1},
		{Cmd: "resume"},
		{Cmd: "resume", HoldID: " "},
		{Cmd: "resume", HoldID: "lease-exact", ServerID: "owner-exact"},
		{Cmd: "resume", HoldID: "lease-exact", Command: "owner-exact"},
		{Cmd: "resume", HoldID: "lease-exact", DrainTimeoutMs: -1},
		{Cmd: "renew", ServerID: "owner-exact"},
		{Cmd: "renew", HoldID: "lease-exact", HoldTTLMS: &zero},
		{Cmd: "renew", HoldID: "lease-exact", HoldTTLMS: &tooLong},
	}
	handler := &maintenanceBoundaryHandler{handle: func(Request) (MaintenanceResult, error) {
		t.Fatal("invalid request reached maintenance mutation")
		return MaintenanceResult{}, nil
	}}
	srv := &Server{handler: handler}
	for i, req := range requests {
		resp, after := srv.dispatch(req)
		if !errors.Is(resp.Err(), ErrMaintenanceInvalid) || after != nil {
			t.Fatalf("request %d did not fail before mutation: %+v", i, resp)
		}
		if _, err := SendMaintenance("unused-invalid-input-endpoint", req, time.Second); !errors.Is(err, ErrMaintenanceInvalid) {
			t.Fatalf("request %d dialed before validation: %v", i, err)
		}
	}
	if _, err := SendMaintenance("unused-invalid-input-endpoint", Request{Cmd: "shutdown"}, time.Second); !errors.Is(err, ErrMaintenanceInvalid) {
		t.Fatalf("nonmaintenance command accepted: %v", err)
	}
	if handler.calls != 0 {
		t.Fatalf("invalid inputs mutated handler %d times", handler.calls)
	}
}

func TestMaintenanceDispatchAndCapabilityBoundary(t *testing.T) {
	base := maintenanceTestResult()
	accepted := time.Now().UTC()
	handler := &maintenanceBoundaryHandler{handle: func(req Request) (MaintenanceResult, error) {
		result := base
		if req.Cmd == "hold" && req.ServerID != base.ServerID {
			return MaintenanceResult{}, ErrMaintenanceNotFound
		}
		if req.Cmd != "hold" && req.HoldID != base.HoldID {
			return MaintenanceResult{}, ErrMaintenanceNotFound
		}
		if req.Cmd == "resume" {
			result.State = MaintenanceReleased
			result.ServerID = "" // The lease remains actionable after recovery.
		} else {
			if req.HoldTTLMS == nil {
				return MaintenanceResult{}, ErrMaintenanceInvalid
			}
			result.ExpiresAt = accepted.Add(time.Duration(*req.HoldTTLMS) * time.Millisecond)
			if req.Cmd == "renew" {
				result.State = MaintenanceRetirementBlocked
				result.TreesRetired = false
			}
		}
		return result, nil
	}}
	path := testSocketPath(t)
	srv, err := NewServer(path, handler, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	minTTL, maxTTL := int64(1), int64(3600000)
	for _, tc := range []struct {
		name       string
		req        Request
		wantState  MaintenanceState
		wantExpiry time.Time
		wantErr    *MaintenanceError
	}{
		{"hold default", Request{Cmd: "hold", ServerID: base.ServerID}, MaintenanceHeld, accepted.Add(5 * time.Minute), nil},
		{"renew blocked", Request{Cmd: "renew", HoldID: base.HoldID, HoldTTLMS: &maxTTL}, MaintenanceRetirementBlocked, accepted.Add(time.Hour), nil},
		{"resume recovered", Request{Cmd: "resume", HoldID: base.HoldID}, MaintenanceReleased, base.ExpiresAt, nil},
		{"exact target only", Request{Cmd: "hold", ServerID: "owner"}, "", time.Time{}, ErrMaintenanceNotFound},
		{"exact lease only", Request{Cmd: "renew", HoldID: "lease"}, "", time.Time{}, ErrMaintenanceNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := SendMaintenance(path, tc.req, time.Second)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || result != nil {
					t.Fatalf("exact lookup refusal lost: result=%+v err=%v", result, err)
				}
				return
			}
			if err != nil || result == nil || result.State != tc.wantState || !result.ExpiresAt.Equal(tc.wantExpiry) {
				t.Fatalf("lease outcome = %+v, err=%v", result, err)
			}
		})
	}
	// The inclusive lower bound is validated without a wall-clock expiry race.
	if _, err := prepareMaintenanceRequest(Request{Cmd: "hold", ServerID: base.ServerID, HoldTTLMS: &minTTL}); err != nil {
		t.Fatalf("minimum TTL refused: %v", err)
	}
	legacy := &mockDaemonHandler{}
	old := &Server{handler: legacy}
	for _, req := range []Request{
		{Cmd: "hold", ServerID: base.ServerID},
		{Cmd: "resume", HoldID: base.HoldID},
		{Cmd: "renew", HoldID: base.HoldID},
		{Cmd: "restart_owner", ServerID: base.ServerID},
	} {
		resp, after := old.dispatch(req)
		if !errors.Is(resp.Err(), ErrMaintenanceUnsupported) || after != nil {
			t.Fatalf("missing capability accepted %s: %+v", req.Cmd, resp)
		}
	}
	if legacy.shutdownCalled || legacy.spawnCalled || legacy.stopCalled || legacy.removeCalled {
		t.Fatal("missing maintenance capability used destructive legacy fallback")
	}
	blocked := base
	blocked.State, blocked.TreesRetired = MaintenanceRetirementBlocked, false
	refusing := &Server{handler: &maintenanceBoundaryHandler{handle: func(Request) (MaintenanceResult, error) {
		return blocked, fmt.Errorf("private context: %w", ErrMaintenanceRetirementBlocked)
	}}}
	resp, _ := refusing.dispatch(Request{Cmd: "hold", ServerID: base.ServerID})
	var typed *MaintenanceError
	if !errors.Is(resp.Err(), ErrMaintenanceRetirementBlocked) || !errors.As(resp.Err(), &typed) || typed.Result == nil || typed.Result.State != MaintenanceRetirementBlocked {
		t.Fatalf("blocked safe readback lost: %+v", resp)
	}
	untyped := &Server{handler: &maintenanceBoundaryHandler{handle: func(Request) (MaintenanceResult, error) {
		return MaintenanceResult{}, errors.New("upstream held for update; private context secret")
	}}}
	resp, _ = untyped.dispatch(Request{Cmd: "hold", ServerID: base.ServerID})
	if !errors.Is(resp.Err(), ErrMaintenanceUnsupported) || strings.Contains(resp.Message, "secret") {
		t.Fatalf("untyped handler classified by message or leaked reason: %+v", resp)
	}
}

func TestMaintenancePeerResponsesFailClosed(t *testing.T) {
	base := maintenanceTestResult()
	for _, tc := range []struct {
		name    string
		cmd     string
		change  func(*MaintenanceResult)
		code    MaintenanceErrorCode
		ok      bool
		missing bool
		raw     string
		want    *MaintenanceError
	}{
		{name: "hold complete", cmd: "hold", ok: true},
		{name: "resume complete", cmd: "resume", ok: true, change: func(r *MaintenanceResult) { r.State = MaintenanceReleased; r.ServerID = "" }},
		{name: "renew holding", cmd: "renew", ok: true, change: func(r *MaintenanceResult) { r.State = MaintenanceHolding; r.TreesRetired = false }},
		{name: "blocked typed", cmd: "hold", code: ErrMaintenanceRetirementBlocked.Code, change: func(r *MaintenanceResult) { r.State = MaintenanceRetirementBlocked; r.TreesRetired = false }, want: ErrMaintenanceRetirementBlocked},
		{name: "old failure text is not a code", cmd: "hold", missing: true, want: ErrMaintenanceUnsupported},
		{name: "success without capability result", cmd: "hold", ok: true, missing: true, want: ErrMaintenanceUnsupported},
		{name: "unknown failure code", cmd: "hold", code: "new_code", want: ErrMaintenanceInvalid},
		{name: "success plus refusal", cmd: "hold", ok: true, code: ErrMaintenanceHeld.Code, want: ErrMaintenanceInvalid},
		{name: "hold still holding", cmd: "hold", ok: true, change: func(r *MaintenanceResult) { r.State = MaintenanceHolding; r.TreesRetired = false }, want: ErrMaintenanceInvalid},
		{name: "hold not retired", cmd: "hold", ok: true, change: func(r *MaintenanceResult) { r.TreesRetired = false }, want: ErrMaintenanceInvalid},
		{name: "hold expired", cmd: "hold", ok: true, change: func(r *MaintenanceResult) { r.ExpiresAt = time.Now().Add(-time.Second) }, want: ErrMaintenanceInvalid},
		{name: "hold different target", cmd: "hold", ok: true, change: func(r *MaintenanceResult) { r.ServerID = "different-owner" }, want: ErrMaintenanceInvalid},
		{name: "hold missing lease", cmd: "hold", ok: true, change: func(r *MaintenanceResult) { r.HoldID = "" }, want: ErrMaintenanceInvalid},
		{name: "hold missing expiry", cmd: "hold", ok: true, change: func(r *MaintenanceResult) { r.ExpiresAt = time.Time{} }, want: ErrMaintenanceInvalid},
		{name: "hold missing drain", cmd: "hold", ok: true, change: func(r *MaintenanceResult) { r.DrainDeadline = time.Time{} }, want: ErrMaintenanceInvalid},
		{name: "unknown state", cmd: "renew", ok: true, change: func(r *MaintenanceResult) { r.State = "UNKNOWN" }, want: ErrMaintenanceInvalid},
		{name: "resume held", cmd: "resume", ok: true, want: ErrMaintenanceInvalid},
		{name: "resume not retired", cmd: "resume", ok: true, change: func(r *MaintenanceResult) { r.State = MaintenanceReleased; r.TreesRetired = false }, want: ErrMaintenanceInvalid},
		{name: "resume different lease", cmd: "resume", ok: true, change: func(r *MaintenanceResult) { r.State = MaintenanceReleased; r.HoldID = "other-lease" }, want: ErrMaintenanceInvalid},
		{name: "renew released", cmd: "renew", ok: true, change: func(r *MaintenanceResult) { r.State = MaintenanceReleased }, want: ErrMaintenanceInvalid},
		{name: "renew different lease", cmd: "renew", ok: true, change: func(r *MaintenanceResult) { r.HoldID = "other-lease" }, want: ErrMaintenanceInvalid},
		{name: "renew expired", cmd: "renew", ok: true, change: func(r *MaintenanceResult) { r.ExpiresAt = time.Now().Add(-time.Second) }, want: ErrMaintenanceInvalid},
		{name: "blocked claimed retired", cmd: "renew", ok: true, change: func(r *MaintenanceResult) { r.State = MaintenanceRetirementBlocked }, want: ErrMaintenanceInvalid},
		{name: "malformed JSON", cmd: "hold", raw: "not json\n", want: ErrMaintenanceInvalid},
		{name: "malformed timestamp", cmd: "hold", raw: `{"ok":true,"maintenance":{"expires_at":"not-a-time"}}`, want: ErrMaintenanceInvalid},
		{name: "malformed result type", cmd: "hold", raw: `{"ok":true,"maintenance":[]}`, want: ErrMaintenanceInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := base
			if tc.change != nil {
				tc.change(&result)
			}
			resp := Response{OK: tc.ok, Message: "upstream held for update; private reason", ErrorCode: tc.code}
			if !tc.missing {
				resp.Maintenance = &result
			}
			wire := tc.raw
			if wire == "" {
				data, err := json.Marshal(resp)
				if err != nil {
					t.Fatal(err)
				}
				wire = string(data) + "\n"
			}
			path := testSocketPath(t)
			ln, err := ipc.Listen(path)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan []Request, 1)
			go func() {
				var requests []Request
				for {
					conn, err := ln.Accept()
					if err != nil {
						done <- requests
						return
					}
					_ = conn.SetDeadline(time.Now().Add(time.Second))
					var req Request
					if json.NewDecoder(conn).Decode(&req) == nil {
						requests = append(requests, req)
						_, _ = conn.Write([]byte(wire))
					}
					_ = conn.Close()
				}
			}()
			req := Request{Cmd: tc.cmd, HoldID: base.HoldID}
			if tc.cmd == "hold" {
				req.HoldID, req.ServerID = "", base.ServerID
			}
			got, err := SendMaintenance(path, req, time.Second)
			_ = ln.Close()
			select {
			case requests := <-done:
				if len(requests) != 1 || requests[0].Cmd != req.Cmd {
					t.Fatalf("maintenance attempted fallback: %+v", requests)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("peer did not finish")
			}
			if tc.want == nil {
				if err != nil || got == nil || got.State != result.State {
					t.Fatalf("valid outcome refused: result=%+v err=%v", got, err)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("peer classification = %v, want %v", err, tc.want)
			} else if tc.want == ErrMaintenanceRetirementBlocked {
				var typed *MaintenanceError
				if got == nil || !errors.As(err, &typed) || typed.Result == nil || got.State != MaintenanceRetirementBlocked {
					t.Fatalf("safe error result lost: result=%+v err=%v", got, err)
				}
			} else if got != nil {
				t.Fatalf("malformed/unsupported peer yielded actionable result: %+v", got)
			}
		})
	}
}

type maintenanceLifecycleHandler struct {
	mockDaemonHandler
	refusal             error
	restartResponse     Response
	restartStarted      chan struct{}
	restartRelease      <-chan struct{}
	shutdownAwareCalled bool
	restartCalled       bool
	gracefulCalled      bool
}

func (m *maintenanceLifecycleHandler) HandleShutdownWithError(int) (string, error) {
	m.shutdownAwareCalled = true
	return "shutdown initiated", m.refusal
}

func (m *maintenanceLifecycleHandler) HandleGracefulRestart(int) (string, func(), error) {
	m.gracefulCalled = true
	return "snapshot", func() {}, m.refusal
}

func (m *maintenanceLifecycleHandler) HandleRestartOwner(Request) (Response, error) {
	m.restartCalled = true
	if m.restartStarted != nil {
		close(m.restartStarted)
	}
	if m.restartRelease != nil {
		<-m.restartRelease
	}
	return m.restartResponse, m.refusal
}

func TestMaintenanceLifecycleRefusals(t *testing.T) {
	result := maintenanceTestResult()
	refusal := fmt.Errorf("private context secret: %w", &MaintenanceError{Code: ErrMaintenanceHeld.Code, Result: &result})
	for _, req := range []Request{
		{Cmd: "spawn", Command: "fixture"},
		{Cmd: "refresh-token", PrevToken: "token"},
		{Cmd: "graceful-restart"},
		{Cmd: "shutdown"},
		{Cmd: "restart_owner", ServerID: result.ServerID},
	} {
		t.Run(req.Cmd, func(t *testing.T) {
			handler := &maintenanceLifecycleHandler{refusal: refusal}
			handler.spawnErr, handler.refreshErr = refusal, refusal
			resp, after := (&Server{handler: handler}).dispatch(req)
			if !errors.Is(resp.Err(), ErrMaintenanceHeld) || after != nil || resp.Maintenance == nil || strings.Contains(resp.Message, "secret") {
				t.Fatalf("lifecycle refusal lost or destructive callback retained: %+v, after=%v", resp, after != nil)
			}
			if handler.shutdownCalled {
				t.Fatal("typed shutdown refusal fell back to legacy shutdown")
			}
		})
	}
	for _, req := range []Request{
		{Cmd: "restart_owner"},
		{Cmd: "restart_owner", Command: result.ServerID},
		{Cmd: "restart_owner", ServerID: " "},
		{Cmd: "restart_owner", ServerID: result.ServerID, HoldID: result.HoldID},
		{Cmd: "restart_owner", ServerID: result.ServerID, DrainTimeoutMs: -1},
		{Cmd: "shutdown", DrainTimeoutMs: -1},
		{Cmd: "graceful-restart", DrainTimeoutMs: -1},
	} {
		handler := &maintenanceLifecycleHandler{}
		resp, _ := (&Server{handler: handler}).dispatch(req)
		if !errors.Is(resp.Err(), ErrMaintenanceInvalid) || handler.restartCalled || handler.shutdownAwareCalled || handler.gracefulCalled {
			t.Fatalf("invalid lifecycle inputs reached mutation: req=%+v resp=%+v", req, resp)
		}
	}
}

func TestMaintenanceRestartOwnerUndeliveredReservation(t *testing.T) {
	started, release, rolledBack := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := &maintenanceLifecycleHandler{
		mockDaemonHandler: mockDaemonHandler{rollbackCh: rolledBack},
		restartResponse:   Response{OK: true, IPCPath: "/tmp/restarted.sock", ServerID: "owner-restarted", Token: "reservation", ProtocolEra: "2026-07-28"},
		restartStarted:    started,
		restartRelease:    release,
	}
	srv := &Server{handler: handler, logger: testLogger(t)}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	srv.wg.Add(1)
	done := make(chan struct{})
	go func() {
		srv.handleConn(serverConn)
		close(done)
	}()
	if err := json.NewEncoder(clientConn).Encode(Request{Cmd: "restart_owner", ServerID: "owner-exact"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("restart handler did not start")
	}
	_ = clientConn.Close()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("undelivered restart handler did not finish")
	}
	select {
	case <-rolledBack:
	default:
		t.Fatal("restart reservation was not rolled back")
	}
	if handler.rollbackSID != "owner-restarted" || handler.rollbackToken != "reservation" || handler.spawnCalled || handler.stopCalled {
		t.Fatalf("restart leaked reservation or used legacy fallback: %+v", handler)
	}
}

func TestMaintenanceLegacyWireOmissionAndSafeProjection(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{Request{Cmd: "spawn"}, `{"cmd":"spawn"}`},
		{Response{OK: true, Token: "token"}, `{"ok":true,"token":"token"}`},
		{Response{Message: "ordinary failure"}, `{"ok":false,"message":"ordinary failure"}`},
	} {
		wire, err := json.Marshal(tc.value)
		if err != nil || string(wire) != tc.want {
			t.Fatalf("legacy wire changed: %s err=%v, want %s", wire, err, tc.want)
		}
	}
	ownerWire, err := json.Marshal(OwnerInfo{})
	if err != nil || strings.Contains(string(ownerWire), "maintenance") {
		t.Fatalf("legacy owner gained maintenance field: %s err=%v", ownerWire, err)
	}
	var resp Response
	if err := json.Unmarshal([]byte(`{"ok":false,"error_code":"maintenance_held","maintenance":{"hold_id":"lease-exact","server_id":"owner-exact","state":"HELD","expires_at":"2099-01-01T00:05:00Z","drain_deadline":"2099-01-01T00:00:00Z","trees_retired":true,"context_keys":["secret"],"env":{"secret":"credential"}},"reason":"private context"}`), &resp); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(resp)
	if err != nil || !errors.Is(resp.Err(), ErrMaintenanceHeld) || strings.Contains(string(wire), "secret") || strings.Contains(string(wire), "private") || strings.Contains(string(wire), "context_keys") {
		t.Fatalf("unsafe fields entered maintenance projection: %s err=%v", wire, err)
	}
}

func TestMaintenanceRestartOwnerResponseValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resp     Response
		want     *MaintenanceError
		rollback bool
	}{
		{"legacy era", Response{OK: true, IPCPath: "/tmp/restarted.sock", ServerID: "owner-exact", Token: "reservation"}, nil, false},
		{"modern era", Response{OK: true, IPCPath: "/tmp/restarted.sock", ServerID: "owner-exact", Token: "reservation", ProtocolEra: "2026-07-28"}, nil, false},
		{"unknown era", Response{OK: true, IPCPath: "/tmp/restarted.sock", ServerID: "owner-exact", Token: "reservation", ProtocolEra: "unknown"}, ErrMaintenanceInvalid, true},
		{"missing endpoint", Response{OK: true, ServerID: "owner-exact", Token: "reservation"}, ErrMaintenanceInvalid, true},
		{"success plus refusal", Response{OK: true, IPCPath: "/tmp/restarted.sock", ServerID: "owner-exact", Token: "reservation", ErrorCode: ErrMaintenanceHeld.Code}, ErrMaintenanceInvalid, true},
		{"unknown refusal", Response{ErrorCode: "future_code"}, ErrMaintenanceInvalid, false},
		{"typed refusal", Response{ErrorCode: ErrMaintenanceHeld.Code}, ErrMaintenanceHeld, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &maintenanceLifecycleHandler{restartResponse: tc.resp}
			resp, after := (&Server{handler: handler, logger: testLogger(t)}).dispatch(Request{Cmd: "restart_owner", ServerID: "owner-exact"})
			if after != nil || handler.spawnCalled || handler.stopCalled || handler.shutdownCalled {
				t.Fatal("managed restart used destructive legacy fallback")
			}
			if tc.want == nil {
				if err := resp.Err(); err != nil || resp.ProtocolEra != tc.resp.ProtocolEra || resp.Token != tc.resp.Token {
					t.Fatalf("daemon-selected restart reservation rejected: %+v, err=%v", resp, err)
				}
			} else if !errors.Is(resp.Err(), tc.want) {
				t.Fatalf("restart result = %+v, want %v", resp, tc.want)
			}
			if tc.rollback {
				if handler.rollbackSID != tc.resp.ServerID || handler.rollbackToken != tc.resp.Token {
					t.Fatalf("malformed restart response leaked reservation: %+v", handler)
				}
			} else if handler.rollbackSID != "" || handler.rollbackToken != "" {
				t.Fatal("delivered or refused restart incorrectly revoked a reservation")
			}
		})
	}
}
