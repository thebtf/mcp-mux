package owner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func newReconnectClient(t *testing.T, stdout io.Writer, logger *log.Logger) *resilientClient {
	t.Helper()
	if stdout == nil {
		stdout = io.Discard
	}
	if logger == nil {
		logger = resilientTestLogger(t)
	}
	return &resilientClient{
		cfg: ResilientClientConfig{
			Stdout:             stdout,
			ReconnectTimeout:   time.Second,
			MaxRefreshAttempts: 3,
		},
		msgFromCC:  make(chan []byte, msgFromCCBufferSize),
		msgFromIPC: make(chan []byte, msgFromIPCBufferSize),
		ipcEOF:     make(chan struct{}),
		stdoutDead: make(chan struct{}),
		startTime:  time.Now(),
		log:        logger,
	}
}

func tempReconnectSocketPath(t *testing.T, sid string) string {
	t.Helper()
	f, err := os.CreateTemp("", "mcp-mux-"+sid+"-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp() error: %v", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	t.Cleanup(func() { ipc.Cleanup(path) })
	return path
}

func closeReconnectConn(t *testing.T, conn interface {
	io.Reader
	io.Writer
	io.Closer
}) {
	t.Helper()
	if conn == nil {
		return
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close() error: %v", err)
	}
}

type testJSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type testJSONRPCResponse struct {
	ID     json.RawMessage            `json:"id"`
	Result map[string]json.RawMessage `json:"result,omitempty"`
	Error  *testJSONRPCError          `json:"error,omitempty"`
}

func parseJSONRPCResponsesWithID(t *testing.T, raw string) []testJSONRPCResponse {
	t.Helper()
	var responses []testJSONRPCResponse
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var resp testJSONRPCResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("unmarshal JSON-RPC line %q: %v", line, err)
		}
		if resp.ID != nil {
			responses = append(responses, resp)
		}
	}
	return responses
}

func parseJSONRPCResponseLine(t *testing.T, line string) testJSONRPCResponse {
	t.Helper()
	responses := parseJSONRPCResponsesWithID(t, line)
	if len(responses) != 1 {
		t.Fatalf("JSON-RPC response count = %d, want 1; line=%q", len(responses), line)
	}
	return responses[0]
}

func TestReconnect_RefreshesAndSucceeds(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	stdout := &bytes.Buffer{}
	rc := newReconnectClient(t, stdout, logger)
	rc.token = "prev-token"

	refreshPath := tempReconnectSocketPath(t, "abcdef123456")
	srv, _ := startEchoIPCServer(t, refreshPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	refreshCalls := 0
	reconnectCalls := 0
	rc.cfg.RefreshToken = func() (string, string, error) {
		refreshCalls++
		return refreshPath, "fresh-token", nil
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return "", "", errors.New("spawn should not be called")
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls)
	}
	if reconnectCalls != 0 {
		t.Fatalf("reconnectCalls = %d, want 0", reconnectCalls)
	}
	if rc.token != "fresh-token" {
		t.Fatalf("rc.token = %q, want %q", rc.token, "fresh-token")
	}
	logText := logs.String()
	if !strings.Contains(logText, "shim.reconnect.refresh_ok owner=abcdef12") {
		t.Fatalf("missing refresh_ok log, got %q", logText)
	}
	if strings.Contains(logText, "shim.reconnect.fallback_spawn") {
		t.Fatalf("unexpected fallback log, got %q", logText)
	}
}

func TestReconnect_DrainsAlreadySentInflightWithoutReplay(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	stdout := &bytes.Buffer{}
	rc := newReconnectClient(t, stdout, logger)
	rc.inflight.Store("99", true)

	refreshPath := tempReconnectSocketPath(t, "ab12cd34ef56")
	srv, recv := startEchoIPCServer(t, refreshPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	rc.cfg.RefreshToken = func() (string, string, error) {
		return refreshPath, "fresh-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	out := stdout.String()
	if !strings.Contains(out, `"id":99`) ||
		!strings.Contains(out, `request lost during reconnect`) {
		t.Fatalf("missing JSON-RPC error for already-sent in-flight request, stdout=%q", out)
	}

	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case line := <-recv:
			if strings.Contains(line, `"id":99`) {
				t.Fatalf("already-sent in-flight request was replayed to successor: %s", line)
			}
		case <-deadline:
			// Good: reconnect drains already-sent in-flight requests by ID instead
			// of replaying them blindly to the successor owner.
			return
		}
	}
}

func TestReconnect_DrainExactOnceThenSuccessorResponse(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	stdout := &bytes.Buffer{}
	rc := newReconnectClient(t, stdout, logger)
	rc.inflight.Store("321", true)

	successorPath := tempReconnectSocketPath(t, "c0ffee123456")
	srv := startGenerationIPCServer(t, successorPath, "successor-gen-1")
	defer srv.closeAll()

	stdinDone := make(chan error)
	rc.cfg.RefreshToken = func() (string, string, error) {
		return successorPath, "fresh-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	out := stdout.String()
	drained := parseJSONRPCResponsesWithID(t, out)
	if len(drained) != 1 {
		t.Fatalf("drained JSON-RPC response count = %d, want 1; stdout=%q", len(drained), out)
	}
	if got := string(drained[0].ID); got != "321" {
		t.Fatalf("drained response ID = %s, want original numeric id 321; stdout=%q", got, out)
	}
	if drained[0].Error == nil || !strings.Contains(drained[0].Error.Message, "request lost during reconnect") {
		t.Fatalf("drained response missing reconnect loss error; response=%+v stdout=%q", drained[0], out)
	}
	if drained[0].Result != nil {
		t.Fatalf("drained response unexpectedly had result payload; response=%+v", drained[0])
	}
	if got := rc.countInflight(); got != 0 {
		t.Fatalf("inflight after drain = %d, want 0", got)
	}

	if _, err := fmt.Fprintf(conn, "%s\n", `{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"after-reconnect"}}`); err != nil {
		t.Fatalf("write successor request: %v", err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read successor response: %v", err)
	}
	successor := parseJSONRPCResponseLine(t, line)
	if got := string(successor.ID); got != "77" {
		t.Fatalf("successor response ID = %s, want 77; line=%q", got, line)
	}
	if successor.Result == nil {
		t.Fatalf("successor response missing result; line=%q", line)
	}
	var generation string
	if err := json.Unmarshal(successor.Result["successor_daemon_generation"], &generation); err != nil {
		t.Fatalf("successor generation evidence missing or invalid: %v; line=%q", err, line)
	}
	if generation != "successor-gen-1" {
		t.Fatalf("successor generation = %q, want successor-gen-1; line=%q", generation, line)
	}
}

func TestReconnect_ImmediateSpawnOnUnknownToken(t *testing.T) {
	// After daemon hard-kill + respawn, the new daemon has no record of the
	// old session token. Refresh must break early instead of retrying all
	// MaxRefreshAttempts times — every retry would fail identically since
	// the new daemon's session manager is empty for this token.
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	rc := newReconnectClient(t, io.Discard, logger)
	rc.token = "prev-token"

	spawnPath := tempReconnectSocketPath(t, "fedcba987654")
	srv, _ := startEchoIPCServer(t, spawnPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	refreshCalls := 0
	reconnectCalls := 0
	rc.cfg.RefreshToken = func() (string, string, error) {
		refreshCalls++
		return "", "", errors.New("unknown token")
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return spawnPath, "spawn-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1 (early break on unknown_token)", refreshCalls)
	}
	if reconnectCalls != 1 {
		t.Fatalf("reconnectCalls = %d, want 1", reconnectCalls)
	}
	if rc.token != "spawn-token" {
		t.Fatalf("rc.token = %q, want %q", rc.token, "spawn-token")
	}
	logText := logs.String()
	if strings.Count(logText, "shim.reconnect.refresh_fail reason=unknown_token") != 1 {
		t.Fatalf("refresh_fail logs = %d, want 1; logs=%q", strings.Count(logText, "shim.reconnect.refresh_fail reason=unknown_token"), logText)
	}
	if !strings.Contains(logText, "shim.reconnect.fallback_spawn reason=unknown_token") {
		t.Fatalf("missing fallback_spawn reason=unknown_token log, got %q", logText)
	}
}

func TestReconnect_RetriesRefreshDuringDaemonShutdown(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	rc := newReconnectClient(t, io.Discard, logger)
	rc.token = "prev-token"

	refreshPath := tempReconnectSocketPath(t, "aabbccddeeff")
	srv, _ := startEchoIPCServer(t, refreshPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	refreshCalls := 0
	reconnectCalls := 0
	rc.cfg.RefreshToken = func() (string, string, error) {
		refreshCalls++
		if refreshCalls == 1 {
			return "", "", errors.New("daemon shutting down")
		}
		return refreshPath, "fresh-token", nil
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return "", "", errors.New("unexpected fallback spawn")
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if refreshCalls != 2 {
		t.Fatalf("refreshCalls = %d, want 2", refreshCalls)
	}
	if reconnectCalls != 0 {
		t.Fatalf("reconnectCalls = %d, want 0", reconnectCalls)
	}
	if rc.token != "fresh-token" {
		t.Fatalf("rc.token = %q, want fresh-token", rc.token)
	}
	logText := logs.String()
	if !strings.Contains(logText, "shim.reconnect.refresh_fail reason=daemon_shutting_down") {
		t.Fatalf("missing transient refresh_fail log, got %q", logText)
	}
	if strings.Contains(logText, "shim.reconnect.fallback_spawn") {
		t.Fatalf("unexpected fallback spawn log, got %q", logText)
	}
}

func startGenerationIPCServer(t *testing.T, path, generation string) *echoServer {
	t.Helper()
	received := make(chan string, 100)
	ln, err := ipc.Listen(path)
	if err != nil {
		t.Fatalf("generation listen %s: %v", path, err)
	}
	srv := &echoServer{ln: ln, received: received}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.mu.Lock()
			srv.conns = append(srv.conns, conn)
			srv.mu.Unlock()
			go handleGenerationConn(conn, received, generation)
		}
	}()
	t.Cleanup(func() {
		srv.closeAll()
		ipc.Cleanup(path)
	})
	return srv
}

func handleGenerationConn(conn net.Conn, received chan string, generation string) {
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

		var msg map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		idRaw, hasID := msg["id"]
		if !hasID {
			continue
		}
		resp := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"result":{"successor_daemon_generation":%q}}`,
			string(idRaw),
			generation,
		)
		if _, err := fmt.Fprintf(conn, "%s\n", resp); err != nil {
			return
		}
	}
}

func TestReconnect_FallsBackToSpawnAfterNTransientFailures(t *testing.T) {
	// Generic transient errors (neither unknown_token nor owner_gone) must
	// retry up to MaxRefreshAttempts before falling back to spawn — this
	// preserves the original retry path for transient issues like network
	// blips or server-side temporary failures.
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	rc := newReconnectClient(t, io.Discard, logger)
	rc.token = "prev-token"

	spawnPath := tempReconnectSocketPath(t, "abcdef123456")
	srv, _ := startEchoIPCServer(t, spawnPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	refreshCalls := 0
	reconnectCalls := 0
	rc.cfg.RefreshToken = func() (string, string, error) {
		refreshCalls++
		return "", "", errors.New("transient: connection refused")
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return spawnPath, "spawn-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if refreshCalls != 3 {
		t.Fatalf("refreshCalls = %d, want 3 (transient errors must retry)", refreshCalls)
	}
	if reconnectCalls != 1 {
		t.Fatalf("reconnectCalls = %d, want 1", reconnectCalls)
	}
	if !strings.Contains(logs.String(), "shim.reconnect.fallback_spawn reason=N_refresh_fail") {
		t.Fatalf("missing fallback_spawn reason=N_refresh_fail log, got %q", logs.String())
	}
}

func TestReconnect_ImmediateSpawnOnOwnerGone(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	rc := newReconnectClient(t, io.Discard, logger)
	rc.token = "prev-token"

	spawnPath := tempReconnectSocketPath(t, "112233445566")
	srv, _ := startEchoIPCServer(t, spawnPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	refreshCalls := 0
	reconnectCalls := 0
	rc.cfg.RefreshToken = func() (string, string, error) {
		refreshCalls++
		return "", "", errors.New("owner gone")
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return spawnPath, "spawn-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls)
	}
	if reconnectCalls != 1 {
		t.Fatalf("reconnectCalls = %d, want 1", reconnectCalls)
	}
	logText := logs.String()
	if !strings.Contains(logText, "shim.reconnect.refresh_fail reason=owner_gone") {
		t.Fatalf("missing owner_gone refresh_fail log, got %q", logText)
	}
	if !strings.Contains(logText, "shim.reconnect.fallback_spawn reason=owner_gone") {
		t.Fatalf("missing owner_gone fallback log, got %q", logText)
	}
}

func TestReconnect_RefreshNilFallsBackImmediately(t *testing.T) {
	rc := newReconnectClient(t, io.Discard, resilientTestLogger(t))
	rc.token = "prev-token"

	spawnPath := tempReconnectSocketPath(t, "77889900aabb")
	srv, _ := startEchoIPCServer(t, spawnPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	reconnectCalls := 0
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return spawnPath, "spawn-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if reconnectCalls != 1 {
		t.Fatalf("reconnectCalls = %d, want 1", reconnectCalls)
	}
	if rc.token != "spawn-token" {
		t.Fatalf("rc.token = %q, want %q", rc.token, "spawn-token")
	}
}

func TestReconnect_NoGoroutineLeakOnRefreshTimeout(t *testing.T) {
	baseline := runtime.NumGoroutine()

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	rc := newReconnectClient(t, io.Discard, logger)
	rc.cfg.ReconnectTimeout = 100 * time.Millisecond
	rc.cfg.MaxRefreshAttempts = 1

	started := make(chan struct{})
	release := make(chan struct{})
	rc.cfg.RefreshToken = func() (string, string, error) {
		close(started)
		<-release
		return "", "", errors.New("unknown token")
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		return "", "", errors.New("spawn should not be called after timeout")
	}

	stdinDone := make(chan error)
	_, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err == nil || !strings.Contains(err.Error(), "reconnect timeout") {
		t.Fatalf("reconnect() error = %v, want timeout", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh callback never started")
	}
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("goroutine count did not settle: baseline=%d current=%d logs=%q", baseline, runtime.NumGoroutine(), logs.String())
}

func TestOwnerPrefixFromIPCPath(t *testing.T) {
	path := filepath.Join(os.TempDir(), "mcp-mux-1234567890abcdef.sock")
	// ownerPrefixFromIPCPath is now a method; build a minimal resilientClient
	// with default EnginePrefix (empty = "mcp-mux") to exercise the same path.
	rc := &resilientClient{
		cfg: ResilientClientConfig{EnginePrefix: ""},
		log: log.New(io.Discard, "", 0),
	}
	if got := rc.ownerPrefixFromIPCPath(path); got != "12345678" {
		t.Fatalf("ownerPrefixFromIPCPath() = %q, want %q", got, "12345678")
	}
}
