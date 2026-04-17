package owner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// resilientTestLogger returns a logger writing to stderr for resilient client tests.
func resilientTestLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(os.Stderr, fmt.Sprintf("[rc-test/%s] ", t.Name()), log.LstdFlags|log.Lmicroseconds)
}

// echoServer is a test IPC echo server that tracks open connections so they
// can all be closed when the listener is closed.
type echoServer struct {
	ln       net.Listener
	received chan string
	mu       sync.Mutex
	conns    []net.Conn
}

// closeAll closes the listener and all accepted connections, forcing EOF on clients.
func (s *echoServer) closeAll() {
	s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

// startEchoIPCServer starts a simple IPC server at path that:
//   - For each client connection: reads JSON-RPC lines, echoes them back with the same id.
//   - Supports a special "initialize" method that echoes a valid initialize response.
//
// Returns the echoServer (call closeAll() to simulate daemon restart) and
// a channel that receives raw lines received by the server.
// The server runs until closeAll() is called.
func startEchoIPCServer(t *testing.T, path string) (srv *echoServer, received chan string) {
	t.Helper()
	received = make(chan string, 100)

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}

	srv = &echoServer{ln: ln, received: received}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			srv.mu.Lock()
			srv.conns = append(srv.conns, conn)
			srv.mu.Unlock()
			go handleEchoConn(conn, received)
		}
	}()

	t.Cleanup(func() {
		srv.closeAll()
		os.Remove(path)
	})

	return srv, received
}

// handleEchoConn handles one IPC server connection: reads JSON-RPC lines and echoes responses.
func handleEchoConn(conn net.Conn, received chan string) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		select {
		case received <- line:
		default:
		}

		// Parse id and method.
		var msg map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		idRaw, hasID := msg["id"]
		if !hasID {
			// Notification — no response.
			continue
		}

		methodRaw := msg["method"]
		method := strings.Trim(string(methodRaw), `"`)

		var resp string
		switch method {
		case "initialize":
			resp = fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"test-server","version":"1.0"},"capabilities":{}}}`,
				string(idRaw),
			)
		default:
			resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, string(idRaw))
		}

		if _, err := fmt.Fprintf(conn, "%s\n", resp); err != nil {
			return
		}
	}
}

// newTestIPCPath creates a temp socket path for tests.
func newTestIPCPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "rc-test-*.sock")
	if err != nil {
		t.Fatalf("create temp socket: %v", err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	return path
}

// TestResilientClient_ReconnectAfterIPCClose verifies that when the initial IPC
// connection is closed (simulating daemon restart), the client calls ReconnectFunc,
// reconnects transparently, and resumes proxying — RunResilientClient does NOT exit
// until CC stdin closes.
func TestResilientClient_ReconnectAfterIPCClose(t *testing.T) {
	path1 := newTestIPCPath(t)
	path2 := newTestIPCPath(t)

	srv1, recv1 := startEchoIPCServer(t, path1)
	_, recv2 := startEchoIPCServer(t, path2)

	// Set up CC stdin/stdout pipes.
	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()

	reconnectCalled := make(chan struct{}, 1)
	reconnectFn := func() (string, string, error) {
		select {
		case reconnectCalled <- struct{}{}:
		default:
		}
		return path2, "", nil
	}

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:             ccStdinR,
		Stdout:            ccStdoutW,
		InitialIPCPath:    path1,
		Reconnect:         reconnectFn,
		ReconnectTimeout:  10 * time.Second,
		KeepaliveInterval: 5 * time.Second,
		Logger:            resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(cfg)
	}()

	// Drain CC stdout (keepalives and list_changed notifications) so the client
	// does not block on writes.
	go func() {
		io.Copy(io.Discard, ccStdoutR)
	}()

	// Step 1: send initialize through first server.
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	fmt.Fprintf(ccStdinW, "%s\n", initReq)

	// Wait for init request on server 1.
	select {
	case <-recv1:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: server 1 did not receive initialize")
	}

	// Step 2: close first server to simulate daemon restart.
	srv1.closeAll()

	// Step 3: wait for ReconnectFunc to be called.
	select {
	case <-reconnectCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: ReconnectFunc was not called")
	}

	// Step 4: RunResilientClient must NOT have exited yet — proxy resumes after reconnect.
	select {
	case err := <-errCh:
		t.Fatalf("RunResilientClient exited prematurely after reconnect: %v", err)
	default:
		// Good — still running.
	}

	// Step 5: send a message after reconnect and verify server 2 receives it.
	// (server 2 will also receive the replayed initialize from replayInit)
	ping := `{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`
	fmt.Fprintf(ccStdinW, "%s\n", ping)

	// Drain recv2 until we see the ping (skip initialize replay).
	timeout := time.After(5 * time.Second)
	gotPing := false
	for !gotPing {
		select {
		case line := <-recv2:
			var msg map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			if method, ok := msg["method"]; ok {
				m := strings.Trim(string(method), `"`)
				if m == "ping" {
					gotPing = true
				}
			}
		case <-timeout:
			t.Fatal("timeout: server 2 did not receive ping after reconnect")
		}
	}

	// Step 6: close CC stdin — RunResilientClient should return nil.
	ccStdinW.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil on stdin close, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: RunResilientClient did not exit after CC stdin close")
	}
}

// TestResilientClient_BufferDuringReconnect verifies that messages sent during
// the RECONNECTING state are buffered and flushed to the new IPC server after
// reconnect succeeds. RunResilientClient continues proxying (does not exit) until
// CC stdin closes.
func TestResilientClient_BufferDuringReconnect(t *testing.T) {
	path1 := newTestIPCPath(t)
	path2 := newTestIPCPath(t)

	srv1, _ := startEchoIPCServer(t, path1)
	_, recv2 := startEchoIPCServer(t, path2)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()

	// Reconnect delays 500ms to give us time to queue messages.
	reconnectDelay := 500 * time.Millisecond
	reconnectFn := func() (string, string, error) {
		time.Sleep(reconnectDelay)
		return path2, "", nil
	}

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:             ccStdinR,
		Stdout:            ccStdoutW,
		InitialIPCPath:    path1,
		Reconnect:         reconnectFn,
		ReconnectTimeout:  10 * time.Second,
		KeepaliveInterval: 10 * time.Second, // long interval — don't want noise
		Logger:            resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(cfg)
	}()

	// Drain CC stdout (keepalives and list_changed notifications) to prevent blocking.
	go func() {
		io.Copy(io.Discard, ccStdoutR)
	}()

	// Step 1: close first server to trigger RECONNECTING state.
	// Give the client a moment to connect first.
	time.Sleep(200 * time.Millisecond)
	srv1.closeAll()

	// Step 2: wait briefly then send 3 messages during RECONNECTING — they are buffered.
	time.Sleep(100 * time.Millisecond)

	msgs := []string{
		`{"jsonrpc":"2.0","id":10,"method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","id":11,"method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","id":12,"method":"ping","params":{}}`,
	}
	for _, m := range msgs {
		fmt.Fprintf(ccStdinW, "%s\n", m)
	}

	// Step 3: after reconnect succeeds, RunResilientClient must NOT exit —
	// verify server 2 received all 3 buffered messages.
	// No initialize was sent, so replayInit is a no-op; only the 3 pings arrive.
	timeout := time.After(5 * time.Second)
	receivedIDs := make(map[float64]bool)
	for len(receivedIDs) < 3 {
		select {
		case line := <-recv2:
			var msg map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			idRaw, ok := msg["id"]
			if !ok {
				continue
			}
			var id float64
			if err := json.Unmarshal(idRaw, &id); err != nil {
				continue
			}
			receivedIDs[id] = true
		case <-timeout:
			t.Fatalf("timeout: server 2 only received %d of 3 buffered messages", len(receivedIDs))
		}
	}
	t.Logf("server 2 received buffered message IDs: %v", receivedIDs)

	// Step 4: RunResilientClient must NOT have exited — proxy is still running.
	select {
	case err := <-errCh:
		t.Fatalf("RunResilientClient exited prematurely after reconnect: %v", err)
	default:
		// Good — still running.
	}

	// Step 5: close CC stdin — RunResilientClient should return nil.
	ccStdinW.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil on stdin close, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: RunResilientClient did not exit after CC stdin close")
	}
}

// TestResilientClient_TimeoutExits verifies that RunResilientClient returns an
// error when the ReconnectFunc always fails and the timeout expires.
func TestResilientClient_TimeoutExits(t *testing.T) {
	path1 := newTestIPCPath(t)

	srv1, _ := startEchoIPCServer(t, path1)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()

	reconnectFn := func() (string, string, error) {
		return "", "", fmt.Errorf("daemon not available")
	}

	const shortTimeout = 2 * time.Second

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:             ccStdinR,
		Stdout:            ccStdoutW,
		InitialIPCPath:    path1,
		Reconnect:         reconnectFn,
		ReconnectTimeout:  shortTimeout,
		KeepaliveInterval: 10 * time.Second,
		Logger:            resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(cfg)
	}()

	// Drain stdout.
	go func() {
		io.Copy(io.Discard, ccStdoutR)
	}()

	// Let client connect then close the IPC server.
	time.Sleep(200 * time.Millisecond)
	srv1.closeAll()

	// Expect RunResilientClient to return an error within timeout + grace.
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected error from timeout, got nil")
		} else {
			t.Logf("got expected error: %v", err)
		}
	case <-time.After(shortTimeout + 3*time.Second):
		t.Fatal("RunResilientClient did not exit after timeout")
	}

	_ = ccStdinW
}

// TestResilientClient_NoMuxReconnectKeepalive is a regression test for the
// "Received a progress notification for an unknown token" transport tear-down
// symptom. Earlier revisions of resilient_client.reconnect emitted a synthetic
// notifications/progress with progressToken="mux-reconnect" every few seconds
// during the RECONNECTING state. The MCP spec requires progressTokens to
// reference a _meta.progressToken previously issued by the client — Claude
// Code enforces this by closing the stdio transport as soon as it sees an
// unknown token. The keepalive therefore guaranteed that every real reconnect
// window killed the very transport it was trying to preserve.
//
// No progress notification with progressToken="mux-reconnect" (or any other
// shim-invented token) MUST ever be written to CC stdout — not during
// reconnect, not after, not ever. CC stdio stays healthy via request timeouts,
// so drainOrphanedInflight below is the spec-compliant substitute.
func TestResilientClient_NoMuxReconnectKeepalive(t *testing.T) {
	path1 := newTestIPCPath(t)
	path2 := newTestIPCPath(t)

	srv1, _ := startEchoIPCServer(t, path1)
	_, _ = startEchoIPCServer(t, path2)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()

	// Reconnect takes long enough that the OLD keepalive ticker would have
	// fired several times — so if any keepalive logic sneaks back in, the
	// test catches it.
	reconnectDelay := 3 * time.Second
	reconnectFn := func() (string, string, error) {
		time.Sleep(reconnectDelay)
		return path2, "", nil
	}

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:            ccStdinR,
		Stdout:           ccStdoutW,
		InitialIPCPath:   path1,
		Reconnect:        reconnectFn,
		ReconnectTimeout: 15 * time.Second,
		// Non-zero KeepaliveInterval to prove it is genuinely a no-op:
		// a consumer on v0.19.x muxcore can still pass this value without
		// triggering spec-violating output.
		KeepaliveInterval: 200 * time.Millisecond,
		Logger:            resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(cfg)
	}()

	var mu sync.Mutex
	var stdoutLines []string
	go func() {
		scanner := bufio.NewScanner(ccStdoutR)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			stdoutLines = append(stdoutLines, line)
			mu.Unlock()
		}
	}()

	// Let the client connect, then kill the IPC to trigger reconnect.
	time.Sleep(200 * time.Millisecond)
	srv1.closeAll()

	// Wait past the point where legacy keepalives would have fired multiple
	// times (reconnectDelay + generous grace for post-reconnect stabilisation).
	time.Sleep(reconnectDelay + 2*time.Second)

	mu.Lock()
	lines := make([]string, len(stdoutLines))
	copy(lines, stdoutLines)
	mu.Unlock()

	for _, line := range lines {
		if strings.Contains(line, `"progressToken":"mux-reconnect"`) ||
			strings.Contains(line, `"mux-reconnect"`) {
			t.Errorf("shim emitted forbidden mux-reconnect keepalive: %q", line)
		}
	}

	ccStdinW.Close()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("RunResilientClient did not exit")
	}
}

// TestResilientClient_InitReplayOnReconnect verifies that when the client
// reconnects to a new IPC server, it replays the cached initialize request
// so the new server receives it before any other messages.
func TestResilientClient_InitReplayOnReconnect(t *testing.T) {
	path1 := newTestIPCPath(t)
	path2 := newTestIPCPath(t)

	srv1, _ := startEchoIPCServer(t, path1)
	_, recv2 := startEchoIPCServer(t, path2)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()

	reconnectFn := func() (string, string, error) {
		return path2, "", nil
	}

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:             ccStdinR,
		Stdout:            ccStdoutW,
		InitialIPCPath:    path1,
		Reconnect:         reconnectFn,
		ReconnectTimeout:  10 * time.Second,
		KeepaliveInterval: 10 * time.Second,
		Logger:            resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(cfg)
	}()

	// Drain CC stdout (we only care about what server 2 receives).
	go func() {
		io.Copy(io.Discard, ccStdoutR)
	}()

	// Step 1: send initialize through server 1 to populate the cache.
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	fmt.Fprintf(ccStdinW, "%s\n", initReq)

	// Wait a moment for server 1 to process and respond.
	time.Sleep(300 * time.Millisecond)

	// Step 2: close server 1 to trigger reconnect.
	srv1.closeAll()

	// Step 3: wait for server 2 to receive the replayed initialize request.
	timeout := time.After(10 * time.Second)
	gotInit := false
	for !gotInit {
		select {
		case line := <-recv2:
			t.Logf("server2 received: %s", line)
			var msg map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			methodRaw, ok := msg["method"]
			if !ok {
				continue
			}
			method := strings.Trim(string(methodRaw), `"`)
			if method == "initialize" {
				gotInit = true
			}
		case <-timeout:
			t.Fatal("timeout: server 2 did not receive initialize replay")
		}
	}

	ccStdinW.Close()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("RunResilientClient did not exit")
	}
}

// failingWriter fails every Write with a configurable error.
type failingWriter struct {
	err   error
	count int
	mu    sync.Mutex
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.count++
	return 0, w.err
}

// capturingLogger is an io.Writer that buffers log output for inspection.
type capturingLogger struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *capturingLogger) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturingLogger) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// TestResilientClient_DrainOrphanedInflight_LogsWriteErrors verifies FR-7 +
// Gemini's PR #57 review finding: when drainOrphanedInflight fails to write
// the first error response to CC stdout (e.g. pipe closed during reconnect):
//   - exactly ONE stdout-broken log line is emitted (no log spam)
//   - subsequent writes are skipped (count == 1, not N)
//   - the stdoutDead channel is closed so the main loop exits gracefully
//   - the inflight map is still fully drained (no leaked tracking state)
//
// Regression for two issues:
//  1. Originally fmt.Fprintf's error was discarded — CC hang with no log.
//  2. A naive "log per failed write" fix (PR #57 initial) produced log spam
//     proportional to inflight queue depth and did not signal stdoutDead.
func TestResilientClient_DrainOrphanedInflight_LogsWriteErrors(t *testing.T) {
	sink := &capturingLogger{}
	logger := log.New(sink, "", 0)
	stdout := &failingWriter{err: io.ErrClosedPipe}

	rc := &resilientClient{
		cfg: ResilientClientConfig{
			Stdout: stdout,
		},
		log:        logger,
		stdoutDead: make(chan struct{}),
	}

	// Seed inflight with 3 orphaned IDs: one numeric, two string.
	rc.inflight.Store("42", true)
	rc.inflight.Store(`"req-xyz"`, true)
	rc.inflight.Store(`"req-hang"`, true)

	var stdoutMu sync.Mutex
	rc.drainOrphanedInflight(&stdoutMu)

	// All inflight entries must be cleared regardless of write success,
	// otherwise they'd accumulate on every reconnect.
	remaining := 0
	rc.inflight.Range(func(_, _ any) bool { remaining++; return true })
	if remaining != 0 {
		t.Errorf("inflight not cleared after drain: %d entries remain", remaining)
	}

	// With the stdout-broken fast-path, only the first write should be
	// attempted; the remaining orphaned IDs must be skipped to prevent spam.
	if stdout.count != 1 {
		t.Errorf("expected exactly 1 write attempt (fast-skip after first error), got %d", stdout.count)
	}

	logs := sink.String()
	t.Logf("captured logs:\n%s", logs)

	if !strings.Contains(logs, "sending error responses for 3 orphaned in-flight requests") {
		t.Errorf("log missing initial summary: %q", logs)
	}

	// EXACTLY ONE stdout-broken notice — the core of the log-spam fix.
	brokenCount := strings.Count(logs, "stdout broken while draining orphaned in-flight requests")
	if brokenCount != 1 {
		t.Errorf("expected exactly 1 stdout-broken log line, got %d (logs: %q)", brokenCount, logs)
	}

	// Per-id error lines from the OLD code must be gone.
	if strings.Contains(logs, "failed to write orphaned-inflight error response for id=") {
		t.Errorf("unexpected per-id error log (log-spam regression): %q", logs)
	}

	if !strings.Contains(logs, "CC may hang on the remaining orphaned requests") {
		t.Errorf("log missing operator-visible hang warning: %q", logs)
	}

	// stdoutDead MUST be signaled so the main client loop exits gracefully.
	select {
	case <-rc.stdoutDead:
		// good — stdoutOnce.Do fired
	default:
		t.Error("stdoutDead not closed after stdout write failure — client will not exit gracefully")
	}
}

// TestResilientClient_DrainOrphanedInflight_NoErrorsOnSuccess verifies that
// drainOrphanedInflight does NOT log any failure lines, does NOT close
// stdoutDead, and writes every response on the happy path.
func TestResilientClient_DrainOrphanedInflight_NoErrorsOnSuccess(t *testing.T) {
	sink := &capturingLogger{}
	logger := log.New(sink, "", 0)
	var buf strings.Builder

	rc := &resilientClient{
		cfg: ResilientClientConfig{
			Stdout: &buf,
		},
		log:        logger,
		stdoutDead: make(chan struct{}),
	}
	rc.inflight.Store("1", true)
	rc.inflight.Store("2", true)

	var stdoutMu sync.Mutex
	rc.drainOrphanedInflight(&stdoutMu)

	logs := sink.String()
	if strings.Contains(logs, "stdout broken") {
		t.Errorf("unexpected stdout-broken log on success path: %q", logs)
	}
	if strings.Contains(logs, "CC may hang") {
		t.Errorf("unexpected hang warning on success path: %q", logs)
	}

	// Sanity: both error responses were actually written.
	out := buf.String()
	if !strings.Contains(out, `"id":1`) || !strings.Contains(out, `"id":2`) {
		t.Errorf("expected both error responses on stdout, got: %q", out)
	}

	// stdoutDead must still be open on the happy path.
	select {
	case <-rc.stdoutDead:
		t.Error("stdoutDead closed on happy path — would cause false-positive graceful exit")
	default:
		// good
	}
}

// TestResilientClient_DrainOrphanedInflight_FailAfterSuccess verifies that a
// mid-iteration write failure triggers the fast-skip correctly: earlier writes
// in the same drain cycle that succeeded remain observable on stdout, while
// the failing and subsequent writes are dropped and logged exactly once.
func TestResilientClient_DrainOrphanedInflight_FailAfterSuccess(t *testing.T) {
	sink := &capturingLogger{}
	logger := log.New(sink, "", 0)

	// failAfterNWriter: succeeds for `passes` writes, then fails every subsequent write.
	writer := &failAfterNWriter{passes: 1, err: io.ErrClosedPipe}

	rc := &resilientClient{
		cfg: ResilientClientConfig{
			Stdout: writer,
		},
		log:        logger,
		stdoutDead: make(chan struct{}),
	}

	// 4 orphaned IDs: first should succeed, rest should fast-skip after failure.
	rc.inflight.Store("1", true)
	rc.inflight.Store("2", true)
	rc.inflight.Store("3", true)
	rc.inflight.Store("4", true)

	var stdoutMu sync.Mutex
	rc.drainOrphanedInflight(&stdoutMu)

	// Inflight fully drained.
	remaining := 0
	rc.inflight.Range(func(_, _ any) bool { remaining++; return true })
	if remaining != 0 {
		t.Errorf("inflight not cleared: %d entries remain", remaining)
	}

	// Exactly 2 write attempts: the first success + the first failure.
	// The remaining 2 orphaned IDs must NOT have triggered writes.
	if writer.attempts != 2 {
		t.Errorf("expected 2 write attempts (1 success + 1 failure), got %d", writer.attempts)
	}

	// stdoutDead signaled.
	select {
	case <-rc.stdoutDead:
		// good
	default:
		t.Error("stdoutDead not closed after mid-iteration failure")
	}

	// Exactly one per-failure log line. Use the specific per-failure marker,
	// not "stdout broken" alone, because the final summary line also contains
	// that phrase.
	logs := sink.String()
	perFailureCount := strings.Count(logs, "stdout broken while draining orphaned in-flight requests")
	if perFailureCount != 1 {
		t.Errorf("expected 1 per-failure log line, got %d: %q", perFailureCount, logs)
	}
}

// failAfterNWriter succeeds `passes` times, then every subsequent Write
// returns `err`. Used to test mid-iteration failure handling.
type failAfterNWriter struct {
	passes   int
	err      error
	mu       sync.Mutex
	attempts int
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attempts++
	if w.attempts <= w.passes {
		return len(p), nil
	}
	return 0, w.err
}
