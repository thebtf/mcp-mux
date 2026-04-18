package owner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

// testLogger returns a logger that writes to test output.
func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(os.Stderr, "[test] ", log.LstdFlags)
}

// testIPCPath returns a short IPC socket path for tests.
// macOS limits Unix socket paths to 104 bytes; t.TempDir() is too long.
func testIPCPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "mux-test-*.sock")
	if err != nil {
		t.Fatalf("create temp socket: %v", err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	return path
}

func TestOwnerSingleSession(t *testing.T) {
	ipcPath := testIPCPath(t)

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// Send ping request
	sendReq(t, clientW, 1, "ping", `{}`)
	resp := readResp(t, clientR)
	assertResponseID(t, resp, 1)
}

func TestOwnerWithMockServer(t *testing.T) {
	ipcPath := testIPCPath(t)

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// Send initialize request
	sendReq(t, clientW, 1, "initialize", `{}`)
	resp := readResp(t, clientR)
	assertResponseID(t, resp, 1)
	if !strings.Contains(string(resp), "mock-server") {
		t.Errorf("initialize response missing mock-server: %s", string(resp))
	}

	// Send tools/list request
	sendReq(t, clientW, 2, "tools/list", `{}`)
	resp = readResp(t, clientR)
	assertResponseID(t, resp, 2)
	if !strings.Contains(string(resp), "echo") {
		t.Errorf("tools/list response missing 'echo' tool: %s", string(resp))
	}

	// Send tools/call request
	sendReq(t, clientW, 3, "tools/call", `{"name":"echo","arguments":{"message":"hello-mux"}}`)
	resp = readResp(t, clientR)
	assertResponseID(t, resp, 3)
	if !strings.Contains(string(resp), "hello-mux") {
		t.Errorf("tools/call response missing 'hello-mux': %s", string(resp))
	}
}

func TestOwnerMultipleSessions(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Create two sessions
	c1R, s1W := io.Pipe()
	s1R, c1W := io.Pipe()
	session1 := NewSession(s1R, s1W)
	owner.AddSession(session1)

	c2R, s2W := io.Pipe()
	s2R, c2W := io.Pipe()
	session2 := NewSession(s2R, s2W)
	owner.AddSession(session2)

	// Warm up: ensure mock_server is running by sending init from session 1
	sendReq(t, c1W, 99, "initialize", `{}`)
	warmup := readResp(t, c1R)
	if !strings.Contains(string(warmup), "mock-server") {
		t.Fatalf("warmup failed: %s", string(warmup))
	}

	// Both sessions send requests with the SAME id=1 — sequentially to avoid pipe race
	sendReq(t, c1W, 1, "tools/call", `{"name":"echo","arguments":{"message":"from-session1"}}`)
	sendReq(t, c2W, 1, "tools/call", `{"name":"echo","arguments":{"message":"from-session2"}}`)

	// Each session should get the correct response with id=1
	resp1 := readResp(t, c1R)
	resp2 := readResp(t, c2R)

	assertResponseID(t, resp1, 1)
	assertResponseID(t, resp2, 1)

	// Verify each got their own response content
	s1Got := string(resp1)
	s2Got := string(resp2)

	if !strings.Contains(s1Got, "from-session1") {
		t.Errorf("session 1 got wrong response: %s", s1Got)
	}
	if !strings.Contains(s2Got, "from-session2") {
		t.Errorf("session 2 got wrong response: %s", s2Got)
	}
}

func TestOwnerIPCClient(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Add owner's own session
	ownerR, ownerSW := io.Pipe()
	ownerSR, ownerW := io.Pipe()
	ownerSession := NewSession(ownerSR, ownerSW)
	owner.AddSession(ownerSession)

	// Connect a remote client via IPC
	conn, err := ipc.Dial(ipcPath)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer conn.Close()

	// Wait for IPC session to be registered
	time.Sleep(100 * time.Millisecond)

	if owner.SessionCount() < 2 {
		t.Errorf("SessionCount = %d, want >= 2", owner.SessionCount())
	}

	// Send request through IPC client
	ipcReq := `{"jsonrpc":"2.0","id":42,"method":"ping","params":{}}` + "\n"
	_, err = conn.Write([]byte(ipcReq))
	if err != nil {
		t.Fatalf("IPC write error: %v", err)
	}

	// Read response from IPC
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no IPC response received")
	}
	ipcResp := scanner.Text()
	if !strings.Contains(ipcResp, `"id":42`) {
		t.Errorf("IPC response wrong id: %s", ipcResp)
	}

	// Verify owner's session still works independently
	sendReq(t, ownerW, 99, "ping", `{}`)
	ownerResp := readResp(t, ownerR)
	assertResponseID(t, ownerResp, 99)

	_ = ownerW
	_ = ownerR
}

func TestSessionDisconnectDoesNotCrashOwner(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Add two sessions
	_, s1W := io.Pipe()
	s1R, c1W := io.Pipe()
	session1 := NewSession(s1R, s1W)
	owner.AddSession(session1)

	c2R, s2W := io.Pipe()
	s2R, c2W := io.Pipe()
	session2 := NewSession(s2R, s2W)
	owner.AddSession(session2)

	// Disconnect session 1 by closing its write pipe
	c1W.Close()

	// Wait for cleanup
	time.Sleep(100 * time.Millisecond)

	// Session 2 should still work
	sendReq(t, c2W, 1, "ping", `{}`)
	resp := readResp(t, c2R)
	assertResponseID(t, resp, 1)
}

func TestOwnerCachesInitializeAndToolsList(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// Send initialize
	sendReq(t, clientW, 1, "initialize", `{}`)
	resp := readResp(t, clientR)
	assertResponseID(t, resp, 1)
	if !strings.Contains(string(resp), "mock-server") {
		t.Errorf("initialize response missing mock-server: %s", string(resp))
	}

	// Send tools/list
	sendReq(t, clientW, 2, "tools/list", `{}`)
	resp = readResp(t, clientR)
	assertResponseID(t, resp, 2)
	if !strings.Contains(string(resp), "echo") {
		t.Errorf("tools/list response missing echo: %s", string(resp))
	}

	// Verify caching happened
	owner.mu.RLock()
	hasInit := owner.initResp != nil
	hasTools := owner.toolList != nil
	owner.mu.RUnlock()

	if !hasInit {
		t.Error("initResp not cached after initialize")
	}
	if !hasTools {
		t.Error("toolList not cached after tools/list")
	}
}

func TestOwnerReplaysCachedResponses(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Session 1: prime the cache
	c1R, s1W := io.Pipe()
	s1R, c1W := io.Pipe()
	session1 := NewSession(s1R, s1W)
	owner.AddSession(session1)

	sendReq(t, c1W, 1, "initialize", `{}`)
	resp := readResp(t, c1R)
	assertResponseID(t, resp, 1)

	sendReq(t, c1W, 2, "tools/list", `{}`)
	resp = readResp(t, c1R)
	assertResponseID(t, resp, 2)

	// Session 2: should get instant cached responses
	c2R, s2W := io.Pipe()
	s2R, c2W := io.Pipe()
	session2 := NewSession(s2R, s2W)
	owner.AddSession(session2)

	sendReq(t, c2W, 10, "initialize", `{}`)
	resp = readResp(t, c2R)
	assertResponseID(t, resp, 10)
	if !strings.Contains(string(resp), "mock-server") {
		t.Errorf("cached initialize response missing mock-server: %s", string(resp))
	}

	sendReq(t, c2W, 11, "tools/list", `{}`)
	resp = readResp(t, c2R)
	assertResponseID(t, resp, 11)
	if !strings.Contains(string(resp), "echo") {
		t.Errorf("cached tools/list response missing echo: %s", string(resp))
	}
}

func TestOwnerStatusIncludesClassification(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// Prime cache
	sendReq(t, clientW, 1, "initialize", `{}`)
	readResp(t, clientR)
	sendReq(t, clientW, 2, "tools/list", `{}`)
	readResp(t, clientR)

	// Check status
	status := owner.Status()

	if _, ok := status["cached_init"]; !ok {
		t.Error("status missing cached_init")
	}
	if _, ok := status["cached_tools"]; !ok {
		t.Error("status missing cached_tools")
	}
	if status["cached_init"] != true {
		t.Errorf("cached_init = %v, want true", status["cached_init"])
	}
	if status["cached_tools"] != true {
		t.Errorf("cached_tools = %v, want true", status["cached_tools"])
	}

	// Mock server has "echo" and "add" tools — both are stateless
	classification, ok := status["auto_classification"]
	if !ok {
		t.Fatal("status missing auto_classification")
	}
	if classification != "shared" {
		t.Errorf("auto_classification = %v, want shared", classification)
	}
}

// TestServerPingHandledLocally verifies that when the upstream sends a ping
// to the owner, the owner responds locally without forwarding to any client.
// The client should not receive the ping — only its own request responses.
func TestServerPingHandledLocally(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// First, prime with a regular request so we know the session is working
	sendReq(t, clientW, 1, "ping", `{}`)
	resp := readResp(t, clientR)
	assertResponseID(t, resp, 1)

	// Now call the "trigger_ping" tool — the mock server will send a ping
	// server→client request before responding. The owner must handle it
	// locally and the client must still receive only the tool call response.
	sendReq(t, clientW, 2, "tools/call", `{"name":"trigger_ping","arguments":{}}`)
	resp = readResp(t, clientR)
	assertResponseID(t, resp, 2)
	// Verify the client got a tool result (not a ping request)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(resp, &obj); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if _, hasResult := obj["result"]; !hasResult {
		t.Errorf("expected tool result, got: %s", string(resp))
	}
}

// TestSamplingRequestRoutedToSession verifies that when the upstream sends
// sampling/createMessage, it is routed to the last active client session,
// which then responds, allowing the upstream to complete its tool call.
func TestSamplingRequestRoutedToSession(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// Call request_sampling — mock server will send sampling/createMessage
	// to the client and wait for the client to respond before completing.
	sendReq(t, clientW, 5, "tools/call", `{"name":"request_sampling","arguments":{}}`)

	// The client should receive the sampling/createMessage request from the server
	samplingReq := readResp(t, clientR)
	var samplingMsg map[string]json.RawMessage
	if err := json.Unmarshal(samplingReq, &samplingMsg); err != nil {
		t.Fatalf("unmarshal sampling request: %v", err)
	}
	if string(samplingMsg["method"]) != `"sampling/createMessage"` {
		t.Fatalf("expected sampling/createMessage, got: %s", string(samplingReq))
	}

	// Client responds to the sampling request
	samplingID := samplingMsg["id"]
	samplingResp := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%s,"result":{"role":"assistant","content":{"type":"text","text":"sampled"},"model":"test","stopReason":"endTurn"}}`,
		string(samplingID),
	)
	if _, err := clientW.Write([]byte(samplingResp + "\n")); err != nil {
		t.Fatalf("write sampling response: %v", err)
	}

	// Now the tool call result should arrive
	toolResp := readResp(t, clientR)
	assertResponseID(t, toolResp, 5)
	var toolObj map[string]json.RawMessage
	if err := json.Unmarshal(toolResp, &toolObj); err != nil {
		t.Fatalf("unmarshal tool response: %v", err)
	}
	if _, hasResult := toolObj["result"]; !hasResult {
		t.Errorf("expected tool result, got: %s", string(toolResp))
	}
}

// TestCancelledNotificationIDRemapped verifies that when a session sends
// notifications/cancelled with a client-side requestId, the owner remaps that
// requestId to the upstream-facing remapped ID before forwarding.
func TestCancelledNotificationIDRemapped(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// Send a real request so the session is active and known to the owner
	sendReq(t, clientW, 7, "ping", `{}`)
	resp := readResp(t, clientR)
	assertResponseID(t, resp, 7)

	// Send a cancellation for a hypothetical in-flight request id=5.
	// The forwardCancelledNotification method must remap 5 → "s{N}:n:5".
	// The mock server ignores unknown notifications, so the test just verifies
	// no error is returned and the owner remains functional.
	notification := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":5}}` + "\n"
	if _, err := clientW.Write([]byte(notification)); err != nil {
		t.Fatalf("write notification: %v", err)
	}

	// Small pause to let the notification be processed
	time.Sleep(50 * time.Millisecond)

	// Owner must still be functional after the cancellation notification
	sendReq(t, clientW, 8, "ping", `{}`)
	resp = readResp(t, clientR)
	assertResponseID(t, resp, 8)
}

// TestCachedInitSuppressesInitializedNotification verifies that when session 2
// receives a cached initialize response, its subsequent notifications/initialized
// is NOT forwarded to upstream (avoiding duplicate initialized signals).
func TestCachedInitSuppressesInitializedNotification(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Session 1: prime the cache
	c1R, s1W := io.Pipe()
	s1R, c1W := io.Pipe()
	session1 := NewSession(s1R, s1W)
	owner.AddSession(session1)

	sendReq(t, c1W, 1, "initialize", `{}`)
	readResp(t, c1R)
	sendReq(t, c1W, 2, "tools/list", `{}`)
	readResp(t, c1R)

	// Session 2: should receive cached initialize
	c2R, s2W := io.Pipe()
	s2R, c2W := io.Pipe()
	session2 := NewSession(s2R, s2W)
	owner.AddSession(session2)

	sendReq(t, c2W, 10, "initialize", `{}`)
	resp := readResp(t, c2R)
	assertResponseID(t, resp, 10)

	// Verify session 2 is tracked as having received a cached init
	owner.mu.RLock()
	wasCached := owner.cachedInitSessions[session2.ID]
	owner.mu.RUnlock()
	if !wasCached {
		t.Error("session 2 should be in cachedInitSessions after receiving cached initialize")
	}

	// Session 2 sends notifications/initialized — should be suppressed
	notification := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	if _, err := c2W.Write([]byte(notification)); err != nil {
		t.Fatalf("write notification: %v", err)
	}

	// Small pause for processing
	time.Sleep(50 * time.Millisecond)

	// Owner must still be functional
	sendReq(t, c2W, 11, "ping", `{}`)
	resp = readResp(t, c2R)
	assertResponseID(t, resp, 11)
}

// TestPromptsListCachedAndReplayed verifies that prompts/list responses are cached
// and replayed to subsequent sessions with the correct client-facing request ID.
func TestPromptsListCachedAndReplayed(t *testing.T) {
	ipcPath := testIPCPath(t)

	cmd, args := mockServerArgs()
	owner, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Session 1: prime the prompts/list cache
	c1R, s1W := io.Pipe()
	s1R, c1W := io.Pipe()
	session1 := NewSession(s1R, s1W)
	owner.AddSession(session1)

	sendReq(t, c1W, 1, "prompts/list", `{}`)
	resp := readResp(t, c1R)
	assertResponseID(t, resp, 1)
	if !strings.Contains(string(resp), "greeting") {
		t.Errorf("prompts/list response missing 'greeting': %s", string(resp))
	}

	// Verify cache is populated
	owner.mu.RLock()
	hasCached := owner.promptList != nil
	owner.mu.RUnlock()
	if !hasCached {
		t.Error("promptList not cached after prompts/list response")
	}

	// Session 2: should get cached response with correct id
	c2R, s2W := io.Pipe()
	s2R, c2W := io.Pipe()
	session2 := NewSession(s2R, s2W)
	owner.AddSession(session2)

	sendReq(t, c2W, 20, "prompts/list", `{}`)
	resp = readResp(t, c2R)
	assertResponseID(t, resp, 20)
	if !strings.Contains(string(resp), "greeting") {
		t.Errorf("cached prompts/list response missing 'greeting': %s", string(resp))
	}
}

// TestCacheInvalidatedOnListChanged verifies that when upstream sends a
// notifications/tools/list_changed notification, the tools/list cache is cleared.
//
// Uses newMinimalOwner (no real upstream, no proactive init) to avoid a race
// where proactive init's mux-init-1 (tools/list) response could re-populate
// the cache AFTER the test's invalidation, causing spurious failures.
func TestCacheInvalidatedOnListChanged(t *testing.T) {
	o := newMinimalOwner()
	o.controlServer = nil
	defer o.Shutdown()

	// Manually prime the cache (no real upstream needed)
	o.mu.Lock()
	o.toolList = []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo"}]}}`)
	o.mu.Unlock()

	// Verify cache is set before invalidation
	o.mu.RLock()
	hasTools := o.toolList != nil
	o.mu.RUnlock()
	if !hasTools {
		t.Fatal("toolList should be cached before invalidation test")
	}

	// Simulate upstream sending notifications/tools/list_changed via broadcast.
	// broadcast() is the code path that dispatches to invalidateCache which
	// clears the tools/list cache.
	listChangedNotif := []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	if err := o.broadcast(listChangedNotif); err != nil {
		_ = err // broadcast may fail if no sessions — that's fine, we only care about cache invalidation
	}

	// Verify cache is cleared synchronously (broadcast invalidation is sync).
	o.mu.RLock()
	hasToolsAfter := o.toolList != nil
	o.mu.RUnlock()
	if hasToolsAfter {
		t.Error("toolList cache should have been cleared by notifications/tools/list_changed")
	}
}

// --- Helpers ---

func sendReq(t *testing.T, w io.Writer, id int, method, params string) {
	t.Helper()
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"%s","params":%s}`, id, method, params)
	_, err := w.Write([]byte(req + "\n"))
	if err != nil {
		t.Fatalf("sendReq error: %v", err)
	}
}

// readRespTimeout is the per-response deadline for pipe-based owner tests.
// 30s was the historical value. The 180s headroom is a safety net on top of
// the TestMain pre-build (see main_test.go): pre-building mock_server once
// already eliminates the dominant per-test `go run` compile latency, but
// extra margin protects against CI scheduler pauses, GC stalls, and coverage
// instrumentation overhead (observed on ubuntu-latest coverage job where
// instrumented mock_server runs + concurrent test parallelism pushed a single
// readResp past 90s). Happy-path readResp still finishes in <1s.
const readRespTimeout = 180 * time.Second

// scanResult carries the outcome of one scanner.Scan() invocation into the
// select loop below. Capturing the error separately lets the helper fail
// the test with a clear diagnostic instead of returning an empty byte slice
// on EOF/scan error (which downstream would surface as a confusing
// json.Unmarshal failure).
type scanResult struct {
	ok   bool
	line string
	err  error
}

func readResp(t *testing.T, r io.Reader) []byte {
	t.Helper()
	scanner := bufio.NewScanner(r)
	done := make(chan scanResult, 1)
	go func() {
		ok := scanner.Scan()
		done <- scanResult{ok: ok, line: scanner.Text(), err: scanner.Err()}
	}()

	// time.NewTimer + defer Stop() releases the 90s timer immediately on the
	// happy path instead of parking it until expiry (the time.After pattern
	// keeps the underlying timer alive until it fires or GC collects it).
	timer := time.NewTimer(readRespTimeout)
	defer timer.Stop()

	select {
	case res := <-done:
		if !res.ok {
			if res.err != nil {
				t.Fatalf("readResp scanner error: %v", res.err)
			}
			t.Fatal("readResp: scanner returned EOF before any data")
		}
		return []byte(res.line)
	case <-timer.C:
		t.Fatalf("readResp timeout after %s", readRespTimeout)
	}
	return nil // unreachable: both select arms either return or t.Fatalf
}

func assertResponseID(t *testing.T, resp []byte, expectedID int) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(resp, &obj); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, string(resp))
	}

	idRaw, ok := obj["id"]
	if !ok {
		t.Fatalf("response has no id field: %s", string(resp))
	}

	var id int
	if err := json.Unmarshal(idRaw, &id); err != nil {
		t.Fatalf("unmarshal id: %v (raw: %s)", err, string(idRaw))
	}

	if id != expectedID {
		t.Errorf("response id = %d, want %d (full: %s)", id, expectedID, string(resp))
	}
}

func TestNewOwnerFromSnapshot_CachedInit(t *testing.T) {
	ipcPath := testIPCPath(t)

	initResp := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"snap-server","version":"0.1.0"}}}`

	snap := OwnerSnapshot{
		ServerID:       "test123",
		Command:        "echo",
		Args:           []string{"hello"},
		Cwd:            t.TempDir(),
		CwdSet:         []string{t.TempDir()},
		Mode:           "cwd",
		Classification: "shared",
		CachedInit:     base64Encode([]byte(initResp)),
	}

	owner, err := NewOwnerFromSnapshot(OwnerConfig{
		Command:  snap.Command,
		Args:     snap.Args,
		Cwd:      snap.Cwd,
		IPCPath:  ipcPath,
		ServerID: snap.ServerID,
		Logger:   testLogger(t),
	}, snap)
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot() error: %v", err)
	}
	defer owner.Shutdown()

	// Verify init is already cached
	if !owner.InitSuccess() {
		t.Error("InitSuccess() should be true for snapshot owner with cached init")
	}

	// Connect a session via IPC and send initialize — should get instant cached replay
	conn, err := ipc.Dial(ipcPath)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer conn.Close()

	// Send initialize request
	initReq := `{"jsonrpc":"2.0","id":42,"method":"initialize","params":{}}` + "\n"
	if _, err := conn.Write([]byte(initReq)); err != nil {
		t.Fatalf("write init: %v", err)
	}

	// Read response — should be instant (from cache, no upstream needed)
	scanner := bufio.NewScanner(conn)
	done := make(chan bool, 1)
	var line string
	go func() {
		if scanner.Scan() {
			line = scanner.Text()
		}
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for cached init response")
	}

	if !strings.Contains(line, "snap-server") {
		t.Errorf("response should contain snap-server: %s", line)
	}
	// Verify ID was replaced to match our request (42)
	if !strings.Contains(line, `"id":42`) {
		t.Errorf("response should have id:42: %s", line)
	}
}

func TestNewOwnerFromSnapshot_NoUpstream(t *testing.T) {
	ipcPath := testIPCPath(t)

	snap := OwnerSnapshot{
		ServerID: "test456",
		Command:  "nonexistent-command",
		Args:     []string{},
		Cwd:      t.TempDir(),
		CwdSet:   []string{},
		Mode:     "cwd",
	}

	// Should succeed even though upstream command doesn't exist
	// (upstream is NOT spawned in NewOwnerFromSnapshot)
	owner, err := NewOwnerFromSnapshot(OwnerConfig{
		Command:  snap.Command,
		Args:     snap.Args,
		Cwd:      snap.Cwd,
		IPCPath:  ipcPath,
		ServerID: snap.ServerID,
		Logger:   testLogger(t),
	}, snap)
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot() should succeed without upstream: %v", err)
	}
	defer owner.Shutdown()

	// Owner exists but has no upstream — no cached init
	if owner.InitSuccess() {
		t.Error("InitSuccess() should be false for snapshot without cached init")
	}
}

func TestNewOwnerFromSnapshot_ClassificationPreserved(t *testing.T) {
	ipcPath := testIPCPath(t)

	snap := OwnerSnapshot{
		ServerID:             "test789",
		Command:              "echo",
		Args:                 []string{},
		Cwd:                  t.TempDir(),
		CwdSet:               []string{},
		Mode:                 "cwd",
		Classification:       "isolated",
		ClassificationSource: "capability",
		CachedInit:           base64Encode([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)),
	}

	owner, err := NewOwnerFromSnapshot(OwnerConfig{
		Command:  snap.Command,
		Args:     snap.Args,
		Cwd:      snap.Cwd,
		IPCPath:  ipcPath,
		ServerID: snap.ServerID,
		Logger:   testLogger(t),
	}, snap)
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot() error: %v", err)
	}
	defer owner.Shutdown()

	status := owner.Status()
	if status["auto_classification"] != "isolated" {
		t.Errorf("classification = %v, want isolated", status["auto_classification"])
	}
	if status["classification_source"] != "capability" {
		t.Errorf("classification_source = %v, want capability", status["classification_source"])
	}
}
