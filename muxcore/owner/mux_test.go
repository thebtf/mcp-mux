package owner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
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

	// Each session should get the correct response with id=1.
	// Read both pipes in parallel so a routing race between the two sessions
	// cannot hang the test for readRespTimeout (180s) on a single blocked
	// read. Each goroutine owns its pipe and reports via channel; main
	// goroutine waits for both with an outer timeout.
	type readOut struct {
		b   []byte
		err error
	}
	r1Ch := make(chan readOut, 1)
	r2Ch := make(chan readOut, 1)
	go func() { r1Ch <- readOut{b: readResp(t, c1R)} }()
	go func() { r2Ch <- readOut{b: readResp(t, c2R)} }()
	var resp1, resp2 []byte
	outer := time.NewTimer(30 * time.Second)
	defer outer.Stop()
	for i := 0; i < 2; i++ {
		select {
		case r := <-r1Ch:
			resp1 = r.b
		case r := <-r2Ch:
			resp2 = r.b
		case <-outer.C:
			t.Fatalf("response timeout after 30s; resp1=%q resp2=%q", resp1, resp2)
		}
	}

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

	// The client should receive the sampling/createMessage request from the
	// server. Under -race on slower CI runners, messages can arrive in
	// unexpected order (mock_server's scanner.Scan may time out before the
	// test sends the response, causing the mock to emit the tool result
	// before the sampling request is routed to the client). Read until we
	// find the sampling request or time out via readResp's own deadline.
	var samplingReq []byte
	var samplingMsg map[string]json.RawMessage
	var stashedToolResp []byte
	for attempt := 0; attempt < 3; attempt++ {
		msg := readResp(t, clientR)
		var peek map[string]json.RawMessage
		if err := json.Unmarshal(msg, &peek); err != nil {
			t.Fatalf("unmarshal peek: %v", err)
		}
		if string(peek["method"]) == `"sampling/createMessage"` {
			samplingReq = msg
			samplingMsg = peek
			break
		}
		// Not sampling — must be an early tool response (id=5); stash for later.
		stashedToolResp = msg
	}
	if samplingReq == nil {
		t.Fatalf("never received sampling/createMessage (last message stashed: %s)", stashedToolResp)
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

	// Now the tool call result should arrive (or has already arrived out of order above)
	var toolResp []byte
	if stashedToolResp != nil {
		toolResp = stashedToolResp
	} else {
		toolResp = readResp(t, clientR)
	}
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

func TestSnapshotOwnerServesCachedToolsDuringBackgroundRefresh(t *testing.T) {
	ipcPath := testIPCPath(t)
	cwd := t.TempDir()

	initResp := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"snap-server","version":"0.1.0"}}}`
	toolsResp := `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"snapshot-tool"}]}}`

	snap := OwnerSnapshot{
		ServerID:       "test-background-refresh-cache",
		Command:        "in-process-handler",
		Cwd:            cwd,
		CwdSet:         []string{cwd},
		Mode:           "isolated",
		Classification: "isolated",
		CachedInit:     base64Encode([]byte(initResp)),
		CachedTools:    base64Encode([]byte(toolsResp)),
	}

	handlerStarted := make(chan struct{})
	initializeReceived := make(chan struct{})
	allowInitializeResponse := make(chan struct{})
	initializeReleased := false
	defer func() {
		if !initializeReleased {
			close(allowInitializeResponse)
		}
	}()
	handlerFunc := func(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
		_ = ctx
		close(handlerStarted)
		scanner := bufio.NewScanner(stdin)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		for scanner.Scan() {
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			if req.Method == "initialize" {
				break
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		if req.Method != "initialize" {
			return io.ErrUnexpectedEOF
		}
		close(initializeReceived)
		<-allowInitializeResponse
		if _, err := fmt.Fprintf(stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"snap-server","version":"0.1.0"}}}`+"\n", req.ID); err != nil {
			return err
		}
		if !scanner.Scan() {
			return scanner.Err()
		}
		_, _ = io.Copy(io.Discard, stdin)
		return nil
	}

	owner, err := NewOwnerFromSnapshot(OwnerConfig{
		Command:     snap.Command,
		Cwd:         snap.Cwd,
		IPCPath:     ipcPath,
		ServerID:    snap.ServerID,
		HandlerFunc: handlerFunc,
		Logger:      testLogger(t),
	}, snap)
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot() error: %v", err)
	}
	defer owner.Shutdown()

	bgCh := owner.backgroundSpawnCh
	if bgCh == nil {
		t.Fatal("snapshot owner should expose backgroundSpawnCh before background spawn")
	}
	owner.SpawnUpstreamBackground()
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background handler did not start")
	}
	select {
	case <-initializeReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("background handler did not receive proactive initialize")
	}
	select {
	case <-bgCh:
		t.Fatal("background spawn became publicly ready before proactive initialization")
	default:
	}

	// A late initialize response from a dead predecessor must not release this
	// generation's lifecycle gate or overwrite its public snapshot cache.
	staleDone := make(chan struct{})
	close(staleDone)
	staleID := strconv.Quote(owner.proactiveNamespace + "-stale-0")
	owner.registerProactive(staleID, proactiveRequest{method: "initialize", upstreamDone: staleDone})
	if err := owner.handleUpstreamMessage(parseMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"stale-server"}}}`, staleID)))); err != nil {
		t.Fatalf("handle stale proactive initialize response: %v", err)
	}
	if cached := owner.getCachedResponse("initialize"); !strings.Contains(string(cached), "snap-server") {
		t.Fatalf("stale generation response replaced public cached initialize: %s", cached)
	}
	select {
	case <-bgCh:
		t.Fatal("stale generation response released the current background spawn")
	default:
	}

	// Session admission emits roots/listChanged synchronously. Discard that
	// unrelated notification while this test deliberately holds initialize.
	owner.mu.Lock()
	owner.upstreamWriter = io.Discard
	owner.mu.Unlock()

	conn, err := ipc.Dial(ipcPath)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	readLineWithin := func(timeout time.Duration) string {
		t.Helper()
		done := make(chan scanResult, 1)
		go func() {
			ok := scanner.Scan()
			done <- scanResult{ok: ok, line: scanner.Text(), err: scanner.Err()}
		}()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case res := <-done:
			if !res.ok {
				if res.err != nil {
					t.Fatalf("scanner error: %v", res.err)
				}
				t.Fatal("scanner returned EOF before any data")
			}
			return res.line
		case <-timer.C:
			t.Fatalf("timeout waiting for cached response after %s", timeout)
		}
		return ""
	}

	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":42,"method":"initialize","params":{}}` + "\n")); err != nil {
		t.Fatalf("write init: %v", err)
	}
	initLine := readLineWithin(500 * time.Millisecond)
	if !strings.Contains(initLine, `"id":42`) || !strings.Contains(initLine, "snap-server") {
		t.Fatalf("unexpected cached initialize response: %s", initLine)
	}

	owner.mu.Lock()
	owner.upstreamWriter = nil
	owner.mu.Unlock()

	// The snapshot's cached initialize remains public readiness while the new
	// generation-local lifecycle handshake is intentionally still pending.
	close(allowInitializeResponse)
	initializeReleased = true
	select {
	case <-bgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("background spawn did not finish after proactive initialization")
	}

	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatalf("write initialized notification: %v", err)
	}
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":43,"method":"tools/list","params":{}}` + "\n")); err != nil {
		t.Fatalf("write tools/list: %v", err)
	}
	toolsLine := readLineWithin(500 * time.Millisecond)
	if !strings.Contains(toolsLine, `"id":43`) || !strings.Contains(toolsLine, "snapshot-tool") {
		t.Fatalf("unexpected cached tools/list response: %s", toolsLine)
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

func TestProactiveRegistryDrainsDeadGenerationAndSwallowsStaleResponses(t *testing.T) {
	o := newMinimalOwner()
	o.proactiveNamespace = nextProactiveNamespace()
	o.initReady = make(chan struct{})
	o.classified = make(chan struct{})

	deadDone := make(chan struct{})
	close(deadDone)
	currentDone := make(chan struct{})
	staleID := strconv.Quote(o.proactiveNamespace + "-old-0")
	currentID := strconv.Quote(o.proactiveNamespace + "-new-0")
	currentInit := make(chan struct{})
	o.registerProactive(staleID, proactiveRequest{method: "initialize", upstreamDone: deadDone})
	o.drainProactiveRequests(nil)
	o.drainProactiveRequests(nil)
	if got := o.PendingRequests(); got != 0 {
		t.Fatalf("pending after repeated dead-generation drain = %d, want 0", got)
	}
	o.registerProactive(currentID, proactiveRequest{method: "initialize", initResponse: currentInit, upstreamDone: currentDone})
	if got := o.PendingRequests(); got != 1 {
		t.Fatalf("pending after current-generation registration = %d, want 1", got)
	}
	closedDone := make(chan struct{})
	close(closedDone)
	if o.registerProactive(strconv.Quote(o.proactiveNamespace+"-closed-0"), proactiveRequest{method: "tools/list", upstreamDone: closedDone}) {
		t.Fatal("registerProactive accepted an already-dead generation")
	}
	if got := o.PendingRequests(); got != 1 {
		t.Fatalf("pending after rejected registration = %d, want 1", got)
	}

	oldProc := &upstream.Process{}
	currentProc := &upstream.Process{}
	o.mu.Lock()
	o.upstream = currentProc
	o.mu.Unlock()
	oldID := strconv.Quote(o.proactiveNamespace + "-old-live-0")
	oldWait := make(chan struct{})
	o.registerProactive(oldID, proactiveRequest{method: "initialize", initResponse: oldWait, upstream: oldProc, upstreamDone: currentDone})
	oldResponse := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"old-live"}}}`, oldID))
	if err := o.handleUpstreamMessageFrom(oldProc, parseMessage(oldResponse)); err != nil {
		t.Fatalf("handle stale live-generation response: %v", err)
	}
	select {
	case <-oldWait:
		t.Fatal("stale live-generation response released its init waiter")
	default:
	}
	if cached := o.getCachedResponse("initialize"); cached != nil {
		t.Fatalf("stale live-generation response populated cache: %s", cached)
	}
	o.drainProactiveRequests(oldProc)
	o.mu.Lock()
	o.upstream = nil
	o.mu.Unlock()

	staleInit := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"stale"}}}`, staleID))
	if err := o.handleUpstreamMessage(parseMessage(staleInit)); err != nil {
		t.Fatalf("handle stale initialize response: %v", err)
	}
	if got := o.PendingRequests(); got != 1 {
		t.Fatalf("pending after stale initialize response = %d, want 1", got)
	}
	if cached := o.getCachedResponse("initialize"); cached != nil {
		t.Fatalf("stale initialize response populated cache: %s", cached)
	}
	select {
	case <-currentInit:
		t.Fatal("stale initialize response released newer init waiter")
	default:
	}

	toolsID := strconv.Quote(o.proactiveNamespace + "-new-1")
	o.registerProactive(toolsID, proactiveRequest{method: "tools/list", upstreamDone: currentDone})
	freshTools := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"fresh"}]}}`, toolsID))
	if err := o.handleUpstreamMessage(parseMessage(freshTools)); err != nil {
		t.Fatalf("handle fresh tools response: %v", err)
	}
	if got := o.PendingRequests(); got != 1 {
		t.Fatalf("pending after fresh tools response = %d, want 1", got)
	}
	staleTools := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"stale"}]}}`, toolsID))
	if err := o.handleUpstreamMessage(parseMessage(staleTools)); err != nil {
		t.Fatalf("handle duplicate tools response: %v", err)
	}
	if got := o.PendingRequests(); got != 1 {
		t.Fatalf("pending after duplicate tools response = %d, want 1", got)
	}
	if cached := o.getCachedResponse("tools/list"); !strings.Contains(string(cached), "fresh") || strings.Contains(string(cached), "stale") {
		t.Fatalf("duplicate tools response changed cache: %s", cached)
	}

	close(currentDone)
	o.drainProactiveRequests(nil)
	if got := o.PendingRequests(); got != 0 {
		t.Fatalf("pending after current-generation drain = %d, want 0", got)
	}
}

func TestOwnerInstancesUseDistinctFirstProactiveIDs(t *testing.T) {
	ids := make(chan string, 2)
	newOwner := func() *Owner {
		o, err := NewOwner(OwnerConfig{
			IPCPath: testIPCPath(t),
			Logger:  testLogger(t),
			HandlerFunc: func(_ context.Context, stdin io.Reader, _ io.Writer) error {
				var request struct {
					ID json.RawMessage `json:"id"`
				}
				if err := json.NewDecoder(stdin).Decode(&request); err != nil {
					return err
				}
				ids <- string(request.ID)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("NewOwner: %v", err)
		}
		return o
	}

	o1 := newOwner()
	defer o1.Shutdown()
	o2 := newOwner()
	defer o2.Shutdown()
	first := make([]string, 0, 2)
	for len(first) < 2 {
		select {
		case id := <-ids:
			first = append(first, id)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for proactive initialize IDs")
		}
	}
	if first[0] == first[1] {
		t.Fatalf("first proactive IDs collided: %s", first[0])
	}
}

func TestOrdinaryResponsesRequireCurrentSourceAndInflightClaim(t *testing.T) {
	o := newMinimalOwner()
	current := &upstream.Process{}
	stale := &upstream.Process{}
	o.upstream = current
	o.toolList = []byte(`{"result":{"tools":[{"name":"cached"}]}}`)

	staleID := strconv.Quote("ordinary-stale")
	o.inflightTracker.Store(staleID, &InflightRequest{Method: "tools/list"})
	o.methodTags.Store(staleID, "tools/list")
	o.pendingRequests.Add(1)
	staleResponse := parseMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"stale"}]}}`, staleID)))
	if err := o.handleUpstreamMessageFrom(stale, staleResponse); err != nil {
		t.Fatalf("handle stale-source response: %v", err)
	}
	if got := o.PendingRequests(); got != 1 {
		t.Fatalf("pending after stale-source response = %d, want 1", got)
	}
	if _, ok := o.inflightTracker.Load(staleID); !ok {
		t.Fatal("stale-source response claimed current inflight request")
	}
	if cached := o.getCachedResponse("tools/list"); !strings.Contains(string(cached), "cached") || strings.Contains(string(cached), "stale") {
		t.Fatalf("stale-source response changed cache: %s", cached)
	}

	unclaimedID := strconv.Quote("ordinary-unclaimed")
	o.methodTags.Store(unclaimedID, "tools/list")
	unclaimedResponse := parseMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"unclaimed"}]}}`, unclaimedID)))
	if err := o.handleUpstreamMessageFrom(current, unclaimedResponse); err != nil {
		t.Fatalf("handle unclaimed current-source response: %v", err)
	}
	if got := o.PendingRequests(); got != 1 {
		t.Fatalf("pending after unclaimed response = %d, want 1", got)
	}
	if _, ok := o.methodTags.Load(unclaimedID); !ok {
		t.Fatal("unclaimed response consumed method tag")
	}
	if cached := o.getCachedResponse("tools/list"); !strings.Contains(string(cached), "cached") || strings.Contains(string(cached), "unclaimed") {
		t.Fatalf("unclaimed response changed cache: %s", cached)
	}
}

func TestRegisterProactiveDrainedAfterStoreDoesNotLeakPending(t *testing.T) {
	o := newMinimalOwner()
	done := make(chan struct{})
	id := strconv.Quote("proactive-registration-race")
	o.afterProactiveStore = func() {
		close(done)
		o.drainProactiveRequests(nil)
	}

	if o.registerProactive(id, proactiveRequest{method: "tools/list", upstreamDone: done}) {
		t.Fatal("registerProactive accepted a generation drained in its published-entry window")
	}
	if got := o.PendingRequests(); got != 0 {
		t.Fatalf("pending after published-entry drain = %d, want 0", got)
	}
	if _, found := o.proactiveRequests.Load(id); found {
		t.Fatal("published-entry drain left a proactive registry record")
	}
}
