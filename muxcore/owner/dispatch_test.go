package owner

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/muxcore/progress"
)

// ---------------------------------------------------------------------------
// Mock lifecycle + notifier-aware handler
// ---------------------------------------------------------------------------

// mockLifecycleHandler embeds mockSessionHandler and additionally implements
// ProjectLifecycle and NotifierAware so tests can verify hook calls and
// push notifications back through the owner.
type mockLifecycleHandler struct {
	mockSessionHandler
	mu          sync.Mutex
	connects    []string // project IDs received by OnProjectConnect
	disconnects []string // project IDs received by OnProjectDisconnect
	notifier    muxcore.Notifier
}

func (m *mockLifecycleHandler) OnProjectConnect(p muxcore.ProjectContext) {
	m.mu.Lock()
	m.connects = append(m.connects, p.ID)
	m.mu.Unlock()
}

func (m *mockLifecycleHandler) OnProjectDisconnect(projectID string) {
	m.mu.Lock()
	m.disconnects = append(m.disconnects, projectID)
	m.mu.Unlock()
}

func (m *mockLifecycleHandler) SetNotifier(n muxcore.Notifier) {
	m.mu.Lock()
	m.notifier = n
	m.mu.Unlock()
}

func (m *mockLifecycleHandler) capturedConnects() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.connects))
	copy(out, m.connects)
	return out
}

func (m *mockLifecycleHandler) capturedDisconnects() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.disconnects))
	copy(out, m.disconnects)
	return out
}

func (m *mockLifecycleHandler) getNotifier() muxcore.Notifier {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notifier
}

// ---------------------------------------------------------------------------
// Mock SessionHandler
// ---------------------------------------------------------------------------

type mockRequest struct {
	project muxcore.ProjectContext
	request []byte
}

type mockSessionHandler struct {
	mu       sync.Mutex
	requests []mockRequest
	handler  func(ctx context.Context, p muxcore.ProjectContext, req []byte) ([]byte, error)
}

func (m *mockSessionHandler) HandleRequest(ctx context.Context, p muxcore.ProjectContext, req []byte) ([]byte, error) {
	m.mu.Lock()
	m.requests = append(m.requests, mockRequest{project: p, request: req})
	m.mu.Unlock()
	if m.handler != nil {
		return m.handler(ctx, p, req)
	}
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"1","result":{"projectID":"%s"}}`, p.ID)), nil
}

func (m *mockSessionHandler) captured() []mockRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newDispatchOwner builds a minimal Owner wired with the given SessionHandler.
// No upstream process, no IPC listener — just enough to call dispatchToSessionHandler.
func newDispatchOwner(h muxcore.SessionHandler) *Owner {
	return &Owner{
		sessions:               make(map[int]*Session),
		cachedInitSessions:     make(map[int]bool),
		progressOwners:         make(map[string]int),
		progressTokenRequestID: make(map[string]string),
		requestToTokens:        make(map[string][]string),
		progressTracker:        progress.NewTracker(),
		sessionMgr:             NewSessionManager(),
		sessionHandler:         h,
		logger:                 log.New(io.Discard, "", 0),
		done:                   make(chan struct{}),
		listenerDone:           make(chan struct{}),
	}
}

// newTestSession creates a Session whose responses are captured in buf.
// The session's Cwd field is set to cwd.
func newTestSession(cwd string) (*Session, *safeBuf) {
	buf := &safeBuf{}
	s := NewSession(strings.NewReader(""), buf)
	s.Cwd = cwd
	return s, buf
}

// parseMessage constructs a jsonrpc.Message from raw JSON bytes.
func parseMessage(raw []byte) *jsonrpc.Message {
	msg, err := jsonrpc.Parse(raw)
	if err != nil {
		panic(fmt.Sprintf("parseMessage: %v", err))
	}
	return msg
}

// waitForWrite blocks until buf contains at least one non-empty line or the
// deadline elapses. Returns the first line written.
func waitForWrite(t *testing.T, buf *safeBuf, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s := buf.String()
		if s != "" {
			// Return just the first line (trim trailing newline)
			return strings.TrimRight(strings.SplitN(s, "\n", 2)[0], "\r")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no response written within %v", timeout)
	return ""
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestDispatchToSessionHandler_BasicEcho verifies that dispatchToSessionHandler
// builds a ProjectContext from s.Cwd, passes it to HandleRequest, and writes
// the returned response bytes to the session.
func TestDispatchToSessionHandler_BasicEcho(t *testing.T) {
	cwd := "/project-a"
	mock := &mockSessionHandler{}
	o := newDispatchOwner(mock)

	sess, buf := newTestSession(cwd)
	defer sess.Close()

	rawReq := []byte(`{"jsonrpc":"2.0","id":"1","method":"tools/list","params":{}}`)
	msg := parseMessage(rawReq)

	if err := o.dispatchToSessionHandler(sess, msg); err != nil {
		t.Fatalf("dispatchToSessionHandler: %v", err)
	}

	line := waitForWrite(t, buf, 2*time.Second)

	// Verify handler received the right project context.
	reqs := mock.captured()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 captured request, got %d", len(reqs))
	}
	gotCwd := reqs[0].project.Cwd
	if gotCwd != cwd {
		t.Errorf("handler got Cwd=%q, want %q", gotCwd, cwd)
	}
	wantID := muxcore.ProjectContextID(cwd)
	if gotID := reqs[0].project.ID; gotID != wantID {
		t.Errorf("handler got ID=%q, want %q", gotID, wantID)
	}

	// Verify session received the response (default mock response contains projectID).
	if !strings.Contains(line, `"projectID"`) {
		t.Errorf("session response missing projectID: %s", line)
	}
}

// TestDispatchToSessionHandler_ConcurrentSessions verifies that two sessions
// with different CWDs receive responses with distinct ProjectContext.IDs.
func TestDispatchToSessionHandler_ConcurrentSessions(t *testing.T) {
	cwdA := "/project-a"
	cwdB := "/project-b"

	mock := &mockSessionHandler{}
	o := newDispatchOwner(mock)

	// Use a barrier to ensure both dispatches begin together.
	var wg sync.WaitGroup
	wg.Add(2)

	sessA, bufA := newTestSession(cwdA)
	defer sessA.Close()
	sessB, bufB := newTestSession(cwdB)
	defer sessB.Close()

	rawReqA := []byte(`{"jsonrpc":"2.0","id":"1","method":"tools/list","params":{}}`)
	rawReqB := []byte(`{"jsonrpc":"2.0","id":"2","method":"tools/list","params":{}}`)

	go func() {
		defer wg.Done()
		o.dispatchToSessionHandler(sessA, parseMessage(rawReqA)) //nolint:errcheck
	}()
	go func() {
		defer wg.Done()
		o.dispatchToSessionHandler(sessB, parseMessage(rawReqB)) //nolint:errcheck
	}()

	wg.Wait()

	// Wait for both sessions to receive their responses.
	lineA := waitForWrite(t, bufA, 2*time.Second)
	lineB := waitForWrite(t, bufB, 2*time.Second)

	idA := muxcore.ProjectContextID(cwdA)
	idB := muxcore.ProjectContextID(cwdB)

	if idA == idB {
		t.Fatal("test setup error: both CWDs produce the same project ID")
	}

	if !strings.Contains(lineA, idA) {
		t.Errorf("session A response missing ID %q: %s", idA, lineA)
	}
	if !strings.Contains(lineB, idB) {
		t.Errorf("session B response missing ID %q: %s", idB, lineB)
	}

	// Also confirm the captured requests show different IDs.
	reqs := mock.captured()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 captured requests, got %d", len(reqs))
	}
	seen := map[string]bool{}
	for _, r := range reqs {
		seen[r.project.ID] = true
	}
	if !seen[idA] || !seen[idB] {
		t.Errorf("handler did not receive both project IDs; got %v", seen)
	}
}

// TestDispatchToSessionHandler_PanicRecovery verifies that a panicking
// SessionHandler does not crash the owner and that the session receives
// a JSON-RPC error response containing "handler panic".
func TestDispatchToSessionHandler_PanicRecovery(t *testing.T) {
	mock := &mockSessionHandler{
		handler: func(_ context.Context, _ muxcore.ProjectContext, _ []byte) ([]byte, error) {
			panic("deliberate test panic")
		},
	}
	o := newDispatchOwner(mock)

	sess, buf := newTestSession("/project-panic")
	defer sess.Close()

	rawReq := []byte(`{"jsonrpc":"2.0","id":"99","method":"tools/call","params":{}}`)
	msg := parseMessage(rawReq)

	// Should not block or panic.
	if err := o.dispatchToSessionHandler(sess, msg); err != nil {
		t.Fatalf("dispatchToSessionHandler returned error: %v", err)
	}

	line := waitForWrite(t, buf, 2*time.Second)

	// Verify the session received an error response (not a result).
	if !strings.Contains(line, `"error"`) {
		t.Errorf("expected JSON-RPC error in response, got: %s", line)
	}
	if !strings.Contains(line, "handler panic") {
		t.Errorf("expected 'handler panic' in error message, got: %s", line)
	}

	// Owner must still be alive (done channel not closed).
	select {
	case <-o.Done():
		t.Error("owner done channel closed after handler panic — owner should survive")
	default:
		// correct
	}
}

// TestDispatchToSessionHandler_Timeout verifies that when toolTimeoutNs is set
// and a handler blocks longer than the timeout, the session receives a timeout
// error response well before the handler would finish.
func TestDispatchToSessionHandler_Timeout(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})

	mock := &mockSessionHandler{
		handler: func(ctx context.Context, _ muxcore.ProjectContext, _ []byte) ([]byte, error) {
			close(handlerStarted)
			defer close(handlerDone)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return []byte(`{"jsonrpc":"2.0","id":"1","result":{}}`), nil
			}
		},
	}
	o := newDispatchOwner(mock)
	// Set 100 ms timeout.
	o.toolTimeoutNs.Store(int64(100 * time.Millisecond))

	sess, buf := newTestSession("/project-timeout")
	defer sess.Close()

	rawReq := []byte(`{"jsonrpc":"2.0","id":"42","method":"tools/call","params":{}}`)
	msg := parseMessage(rawReq)

	start := time.Now()
	if err := o.dispatchToSessionHandler(sess, msg); err != nil {
		t.Fatalf("dispatchToSessionHandler returned error: %v", err)
	}

	// Wait for handler to start before checking deadline behaviour.
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	// Response must arrive well before the 5-second handler would finish.
	line := waitForWrite(t, buf, 2*time.Second)
	elapsed := time.Since(start)

	if elapsed >= 5*time.Second {
		t.Errorf("response took %v — timeout did not fire (handler ran to completion)", elapsed)
	}

	// The response must be a JSON-RPC error (timeout).
	if !strings.Contains(line, `"error"`) {
		t.Errorf("expected JSON-RPC error for timeout, got: %s", line)
	}
	if !strings.Contains(line, "request timeout") {
		t.Errorf("expected 'request timeout' in error message, got: %s", line)
	}

	// Handler goroutine should exit cleanly (context cancelled).
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Error("handler goroutine did not exit after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// Helpers for lifecycle / notifier tests
// ---------------------------------------------------------------------------

// newLifecycleOwner builds an Owner with the given lifecycle handler wired as
// its sessionHandler.  The ownerNotifier is injected via SetNotifier so tests
// can also exercise Notify/Broadcast.
func newLifecycleOwner(h *mockLifecycleHandler) *Owner {
	o := &Owner{
		sessions:               make(map[int]*Session),
		cachedInitSessions:     make(map[int]bool),
		progressOwners:         make(map[string]int),
		progressTokenRequestID: make(map[string]string),
		requestToTokens:        make(map[string][]string),
		progressTracker:        progress.NewTracker(),
		sessionMgr:             NewSessionManager(),
		sessionHandler:         h,
		logger:                 log.New(io.Discard, "", 0),
		done:                   make(chan struct{}),
		listenerDone:           make(chan struct{}),
	}
	// Wire the notifier so handlers that implement NotifierAware can call back.
	h.SetNotifier(&ownerNotifier{owner: o})
	return o
}

// addSessionDirect registers a session in o.sessions and the session manager
// without starting the readSession goroutine.  Used by notifier tests that
// don't need the full lifecycle goroutine.
func addSessionDirect(o *Owner, s *Session) {
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()
	o.sessionMgr.RegisterSession(s, s.Cwd)
}

// waitCondition polls fn until it returns true or the deadline expires.
func waitCondition(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// Lifecycle hook tests
// ---------------------------------------------------------------------------

// TestOnProjectConnect_CalledOnSessionJoin verifies that OnProjectConnect is
// called with the correct ProjectContext when AddSession is invoked.
func TestOnProjectConnect_CalledOnSessionJoin(t *testing.T) {
	cwd := "/project-lifecycle-connect"
	mock := &mockLifecycleHandler{}
	o := newLifecycleOwner(mock)

	// Build a session whose reader closes immediately so readSession exits fast.
	pr, pw := io.Pipe()
	buf := &safeBuf{}
	s := NewSession(pr, buf)
	s.Cwd = cwd

	// AddSession starts readSession in a goroutine; close the writer to make it exit.
	o.AddSession(s)
	pw.Close() // EOF → readSession returns → removeSession called

	wantID := muxcore.ProjectContextID(cwd)

	// OnProjectConnect is called in a goroutine — wait briefly.
	ok := waitCondition(t, 2*time.Second, func() bool {
		ids := mock.capturedConnects()
		return len(ids) > 0
	})
	if !ok {
		t.Fatal("OnProjectConnect was not called within timeout")
	}

	ids := mock.capturedConnects()
	if len(ids) != 1 {
		t.Fatalf("expected 1 connect call, got %d", len(ids))
	}
	if ids[0] != wantID {
		t.Errorf("OnProjectConnect got ID=%q, want %q", ids[0], wantID)
	}
}

// TestOnProjectDisconnect_CalledOnSessionLeave verifies that OnProjectDisconnect
// is called with the same project ID that OnProjectConnect received.
func TestOnProjectDisconnect_CalledOnSessionLeave(t *testing.T) {
	cwd := "/project-lifecycle-disconnect"
	mock := &mockLifecycleHandler{}
	o := newLifecycleOwner(mock)

	pr, pw := io.Pipe()
	buf := &safeBuf{}
	s := NewSession(pr, buf)
	s.Cwd = cwd

	o.AddSession(s)
	// Close the pipe to trigger readSession → removeSession → OnProjectDisconnect.
	pw.Close()

	wantID := muxcore.ProjectContextID(cwd)

	// Wait for both connect and disconnect hooks to fire.
	ok := waitCondition(t, 2*time.Second, func() bool {
		return len(mock.capturedDisconnects()) > 0
	})
	if !ok {
		t.Fatal("OnProjectDisconnect was not called within timeout")
	}

	disconnects := mock.capturedDisconnects()
	if len(disconnects) != 1 {
		t.Fatalf("expected 1 disconnect call, got %d", len(disconnects))
	}
	if disconnects[0] != wantID {
		t.Errorf("OnProjectDisconnect got ID=%q, want %q", disconnects[0], wantID)
	}

	// Connect hook must have fired with the same ID.
	connects := mock.capturedConnects()
	if len(connects) == 0 {
		t.Fatal("OnProjectConnect was never called")
	}
	if connects[0] != disconnects[0] {
		t.Errorf("connect ID %q != disconnect ID %q", connects[0], disconnects[0])
	}
}

// ---------------------------------------------------------------------------
// Notifier tests
// ---------------------------------------------------------------------------

// TestNotifier_TargetedDelivery verifies that Notify sends to exactly the
// session whose CWD maps to the given project ID, not to other sessions.
func TestNotifier_TargetedDelivery(t *testing.T) {
	cwdA := "/project-notify-a"
	cwdB := "/project-notify-b"

	mock := &mockLifecycleHandler{}
	o := newLifecycleOwner(mock)

	sessA, bufA := newTestSession(cwdA)
	defer sessA.Close()
	sessB, bufB := newTestSession(cwdB)
	defer sessB.Close()

	addSessionDirect(o, sessA)
	addSessionDirect(o, sessB)

	notification := []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	projectIDA := muxcore.ProjectContextID(cwdA)

	n := &ownerNotifier{owner: o}
	if err := n.Notify(projectIDA, notification); err != nil {
		t.Fatalf("Notify returned unexpected error: %v", err)
	}

	// sessA should have received the notification.
	lineA := waitForWrite(t, bufA, 2*time.Second)
	if !strings.Contains(lineA, "notifications/tools/list_changed") {
		t.Errorf("sessA did not receive notification; got: %s", lineA)
	}

	// sessB must NOT have received anything.
	time.Sleep(50 * time.Millisecond)
	if got := bufB.String(); got != "" {
		t.Errorf("sessB received unexpected data: %s", got)
	}
}

// TestNotifier_InvalidProjectID_ReturnsError verifies that Notify returns an
// error when no session matches the given project ID.
func TestNotifier_InvalidProjectID_ReturnsError(t *testing.T) {
	mock := &mockLifecycleHandler{}
	o := newLifecycleOwner(mock)

	// No sessions registered — any project ID should produce an error.
	n := &ownerNotifier{owner: o}
	err := n.Notify("nonexistent-project-id", []byte(`{"jsonrpc":"2.0","method":"test"}`))
	if err == nil {
		t.Fatal("expected error for unknown project ID, got nil")
	}
}

// TestNotifier_Broadcast verifies that Broadcast delivers the notification to
// all registered sessions.
func TestNotifier_Broadcast(t *testing.T) {
	cwdA := "/project-broadcast-a"
	cwdB := "/project-broadcast-b"

	mock := &mockLifecycleHandler{}
	o := newLifecycleOwner(mock)

	sessA, bufA := newTestSession(cwdA)
	defer sessA.Close()
	sessB, bufB := newTestSession(cwdB)
	defer sessB.Close()

	addSessionDirect(o, sessA)
	addSessionDirect(o, sessB)

	notification := []byte(`{"jsonrpc":"2.0","method":"notifications/resources/list_changed"}`)

	n := &ownerNotifier{owner: o}
	n.Broadcast(notification)

	// Both sessions should receive the broadcast.
	lineA := waitForWrite(t, bufA, 2*time.Second)
	if !strings.Contains(lineA, "notifications/resources/list_changed") {
		t.Errorf("sessA did not receive broadcast; got: %s", lineA)
	}

	lineB := waitForWrite(t, bufB, 2*time.Second)
	if !strings.Contains(lineB, "notifications/resources/list_changed") {
		t.Errorf("sessB did not receive broadcast; got: %s", lineB)
	}
}
