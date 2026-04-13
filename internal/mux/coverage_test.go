package mux

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/classify"
	"github.com/thebtf/mcp-mux/internal/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/internal/muxcore/progress"
	"github.com/thebtf/mcp-mux/internal/muxcore/serverid"
)

// safeBuf is a thread-safe bytes.Buffer for use in tests where concurrent
// goroutines (e.g. drainNotifications) may write while the test reads.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------------------------------------------------------------------------
// pathToFileURI
// ---------------------------------------------------------------------------

func TestPathToFileURI_Unix(t *testing.T) {
	got := pathToFileURI("/home/user/project")
	want := "file:///home/user/project"
	if got != want {
		t.Errorf("pathToFileURI unix: got %q, want %q", got, want)
	}
}

func TestPathToFileURI_Windows(t *testing.T) {
	// Windows-style path with drive letter (already slash-normalised)
	got := pathToFileURI("C:/Users/foo/bar")
	want := "file:///C:/Users/foo/bar"
	if got != want {
		t.Errorf("pathToFileURI windows: got %q, want %q", got, want)
	}
}

func TestPathToFileURI_Root(t *testing.T) {
	got := pathToFileURI("/")
	want := "file:///"
	if got != want {
		t.Errorf("pathToFileURI root: got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Session — WriteMessage, Close, Done
// ---------------------------------------------------------------------------

func TestSessionWriteMessage(t *testing.T) {
	var buf bytes.Buffer
	s := NewSession(strings.NewReader(""), &buf)

	id := json.RawMessage(`42`)
	result := map[string]string{"status": "ok"}
	if err := s.WriteMessage(id, result); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, `"id":42`) {
		t.Errorf("WriteMessage output missing id: %s", out)
	}
	if !strings.Contains(out, `"jsonrpc":"2.0"`) {
		t.Errorf("WriteMessage output missing jsonrpc: %s", out)
	}
	if !strings.Contains(out, `"status":"ok"`) {
		t.Errorf("WriteMessage output missing result: %s", out)
	}
}

func TestSessionWriteRaw_ClosedSession(t *testing.T) {
	var buf bytes.Buffer
	s := NewSession(strings.NewReader(""), &buf)
	s.Close()

	err := s.WriteRaw([]byte(`{"test":1}`))
	if err == nil {
		t.Error("expected error writing to closed session, got nil")
	}
}

func TestSessionDone(t *testing.T) {
	s := NewSession(strings.NewReader(""), io.Discard)

	select {
	case <-s.Done():
		t.Error("Done() should not be closed before Close()")
	default:
		// correct
	}

	s.Close()

	select {
	case <-s.Done():
		// correct
	case <-time.After(100 * time.Millisecond):
		t.Error("Done() should be closed after Close()")
	}
}

func TestSessionCloseIdempotent(t *testing.T) {
	s := NewSession(strings.NewReader(""), io.Discard)
	// Should not panic on double close
	s.Close()
	s.Close()

	select {
	case <-s.Done():
		// correct
	case <-time.After(100 * time.Millisecond):
		t.Error("Done() should be closed after Close()")
	}
}

// ---------------------------------------------------------------------------
// initFingerprintMatches — unit tests (no upstream process needed)
// ---------------------------------------------------------------------------

func TestInitFingerprintMatches_NoFingerprint(t *testing.T) {
	o := &Owner{
		sessions:           make(map[int]*Session),
		cachedInitSessions: make(map[int]bool),
		progressOwners:     make(map[string]int),
		logger:             log.New(io.Discard, "", 0),
		done:               make(chan struct{}),
		listenerDone:       make(chan struct{}),
	}

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	// No fingerprint stored — should match (allow replay)
	if !o.initFingerprintMatches(raw) {
		t.Error("expected true when no fingerprint captured")
	}
}

func TestInitFingerprintMatches_SameVersion(t *testing.T) {
	o := &Owner{
		sessions:            make(map[int]*Session),
		cachedInitSessions:  make(map[int]bool),
		progressOwners:      make(map[string]int),
		initProtocolVersion: "2025-03-26",
		logger:              log.New(io.Discard, "", 0),
		done:                make(chan struct{}),
		listenerDone:        make(chan struct{}),
	}

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if !o.initFingerprintMatches(raw) {
		t.Error("expected true for matching protocolVersion")
	}
}

func TestInitFingerprintMatches_DifferentVersion(t *testing.T) {
	o := &Owner{
		sessions:            make(map[int]*Session),
		cachedInitSessions:  make(map[int]bool),
		progressOwners:      make(map[string]int),
		initProtocolVersion: "2025-03-26",
		logger:              log.New(io.Discard, "", 0),
		done:                make(chan struct{}),
		listenerDone:        make(chan struct{}),
	}

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	if o.initFingerprintMatches(raw) {
		t.Error("expected false for mismatching protocolVersion")
	}
}

func TestInitFingerprintMatches_UnparsableJSON(t *testing.T) {
	o := &Owner{
		sessions:            make(map[int]*Session),
		cachedInitSessions:  make(map[int]bool),
		progressOwners:      make(map[string]int),
		initProtocolVersion: "2025-03-26",
		logger:              log.New(io.Discard, "", 0),
		done:                make(chan struct{}),
		listenerDone:        make(chan struct{}),
	}

	// Malformed JSON — should return true (allow replay on parse failure)
	if !o.initFingerprintMatches([]byte(`{bad json`)) {
		t.Error("expected true when JSON is unparsable")
	}
}

// ---------------------------------------------------------------------------
// captureInitFingerprint — unit tests
// ---------------------------------------------------------------------------

func TestCaptureInitFingerprint(t *testing.T) {
	o := &Owner{
		sessions:           make(map[int]*Session),
		cachedInitSessions: make(map[int]bool),
		progressOwners:     make(map[string]int),
		logger:             log.New(io.Discard, "", 0),
		done:               make(chan struct{}),
		listenerDone:       make(chan struct{}),
	}

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	o.captureInitFingerprint(raw)

	o.mu.RLock()
	got := o.initProtocolVersion
	o.mu.RUnlock()

	if got != "2025-03-26" {
		t.Errorf("captureInitFingerprint: got %q, want %q", got, "2025-03-26")
	}
}

func TestCaptureInitFingerprint_OnlyFirstCapture(t *testing.T) {
	o := &Owner{
		sessions:           make(map[int]*Session),
		cachedInitSessions: make(map[int]bool),
		progressOwners:     make(map[string]int),
		logger:             log.New(io.Discard, "", 0),
		done:               make(chan struct{}),
		listenerDone:       make(chan struct{}),
	}

	o.captureInitFingerprint([]byte(`{"params":{"protocolVersion":"first"}}`))
	o.captureInitFingerprint([]byte(`{"params":{"protocolVersion":"second"}}`))

	o.mu.RLock()
	got := o.initProtocolVersion
	o.mu.RUnlock()

	if got != "first" {
		t.Errorf("expected first to win, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Owner getter methods
// ---------------------------------------------------------------------------

func newMinimalOwner() *Owner {
	return &Owner{
		sessions:               make(map[int]*Session),
		cachedInitSessions:     make(map[int]bool),
		progressOwners:         make(map[string]int),
		progressTokenRequestID: make(map[string]string),
		requestToTokens:        make(map[string][]string),
		progressTracker:        progress.NewTracker(),
		sessionMgr:             NewSessionManager(),
		ipcPath:                "/tmp/test.sock",
		command:                "echo",
		args:                   []string{"hello", "world"},
		serverID:               "test-server-id",
		logger:                 log.New(io.Discard, "", 0),
		done:                   make(chan struct{}),
		listenerDone:           make(chan struct{}),
	}
}

func TestOwnerGetters(t *testing.T) {
	o := newMinimalOwner()

	if got := o.ServerID(); got != "test-server-id" {
		t.Errorf("ServerID() = %q, want %q", got, "test-server-id")
	}
	if got := o.IPCPath(); got != "/tmp/test.sock" {
		t.Errorf("IPCPath() = %q, want %q", got, "/tmp/test.sock")
	}
	if got := o.Command(); got != "echo" {
		t.Errorf("Command() = %q, want %q", got, "echo")
	}
	args := o.Args()
	if len(args) != 2 || args[0] != "hello" || args[1] != "world" {
		t.Errorf("Args() = %v, want [hello world]", args)
	}
}

func TestOwnerDone(t *testing.T) {
	o := newMinimalOwner()

	select {
	case <-o.Done():
		t.Error("Done() should not be closed on a live owner")
	default:
		// correct
	}
}

func TestOwnerPendingRequests(t *testing.T) {
	o := newMinimalOwner()
	if got := o.PendingRequests(); got != 0 {
		t.Errorf("PendingRequests() = %d, want 0", got)
	}
	o.pendingRequests.Add(3)
	if got := o.PendingRequests(); got != 3 {
		t.Errorf("PendingRequests() = %d, want 3", got)
	}
}

// ---------------------------------------------------------------------------
// Status() and HandleStatus — tested via integration (require real upstream)
// The existing TestOwnerStatusIncludesClassification covers Status() already.
// We add a classification_reason branch test here using a real owner.
// ---------------------------------------------------------------------------

func TestOwnerStatusWithClassificationReason(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
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

	// Prime initialize and tools/list to trigger classification
	sendReq(t, clientW, 1, "initialize", `{}`)
	readResp(t, clientR)
	sendReq(t, clientW, 2, "tools/list", `{}`)
	readResp(t, clientR)

	// Manually inject classification_reason to test the branch
	owner.mu.Lock()
	owner.classificationReason = []string{"write_file", "bash"}
	owner.mu.Unlock()

	status := owner.Status()

	// classification_reason branch
	reasons, ok := status["classification_reason"]
	if !ok {
		t.Fatal("status missing classification_reason after injection")
	}
	reasonSlice, ok := reasons.([]string)
	if !ok || len(reasonSlice) != 2 {
		t.Errorf("status[classification_reason] = %v, want 2 entries", reasons)
	}
}

// TestHandleStatus checks that HandleStatus delegates to Status().
func TestHandleStatus(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	result := owner.HandleStatus()
	if _, ok := result["mux_version"]; !ok {
		t.Error("HandleStatus() result missing mux_version")
	}
}

// ---------------------------------------------------------------------------
// getCachedResponse — all branches
// ---------------------------------------------------------------------------

func TestGetCachedResponse_AllMethods(t *testing.T) {
	o := newMinimalOwner()
	o.mu.Lock()
	o.initResp = []byte(`{"init":1}`)
	o.toolList = []byte(`{"tools":1}`)
	o.promptList = []byte(`{"prompts":1}`)
	o.resourceList = []byte(`{"resources":1}`)
	o.resourceTemplateList = []byte(`{"templates":1}`)
	o.mu.Unlock()

	cases := []struct {
		method string
		want   string
	}{
		{"initialize", `{"init":1}`},
		{"tools/list", `{"tools":1}`},
		{"prompts/list", `{"prompts":1}`},
		{"resources/list", `{"resources":1}`},
		{"resources/templates/list", `{"templates":1}`},
		{"unknown/method", ""},
	}
	for _, tc := range cases {
		got := o.getCachedResponse(tc.method)
		if tc.want == "" {
			if got != nil {
				t.Errorf("getCachedResponse(%q) = %s, want nil", tc.method, got)
			}
		} else {
			if string(got) != tc.want {
				t.Errorf("getCachedResponse(%q) = %s, want %s", tc.method, got, tc.want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// invalidateCacheIfNeeded — additional notification types
// ---------------------------------------------------------------------------

func TestInvalidateCachePromptListChanged(t *testing.T) {
	o := newMinimalOwner()
	o.mu.Lock()
	o.promptList = []byte(`{"prompts":1}`)
	o.mu.Unlock()

	o.invalidateCacheIfNeeded([]byte(`{"jsonrpc":"2.0","method":"notifications/prompts/list_changed"}`))

	o.mu.RLock()
	got := o.promptList
	o.mu.RUnlock()

	if got != nil {
		t.Error("expected promptList to be nil after prompts/list_changed")
	}
}

func TestInvalidateCacheResourceListChanged(t *testing.T) {
	o := newMinimalOwner()
	o.mu.Lock()
	o.resourceList = []byte(`{"r":1}`)
	o.resourceTemplateList = []byte(`{"t":1}`)
	o.mu.Unlock()

	o.invalidateCacheIfNeeded([]byte(`{"jsonrpc":"2.0","method":"notifications/resources/list_changed"}`))

	o.mu.RLock()
	rl := o.resourceList
	rt := o.resourceTemplateList
	o.mu.RUnlock()

	if rl != nil {
		t.Error("expected resourceList to be nil after resources/list_changed")
	}
	if rt != nil {
		t.Error("expected resourceTemplateList to be nil after resources/list_changed")
	}
}

// ---------------------------------------------------------------------------
// checkPersistent
// ---------------------------------------------------------------------------

func TestCheckPersistent_True(t *testing.T) {
	o := newMinimalOwner()
	o.serverID = "srv-1"

	called := false
	o.onPersistentDetected = func(id string) {
		if id != "srv-1" {
			return
		}
		called = true
	}

	initJSON := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"persistent":true}}}}`)
	o.checkPersistent(initJSON)

	if !called {
		t.Error("expected onPersistentDetected to be called")
	}
}

func TestCheckPersistent_False(t *testing.T) {
	o := newMinimalOwner()
	called := false
	o.onPersistentDetected = func(id string) { called = true }

	initJSON := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"persistent":false}}}}`)
	o.checkPersistent(initJSON)

	if called {
		t.Error("expected onPersistentDetected NOT to be called")
	}
}

func TestCheckPersistent_NoXMux(t *testing.T) {
	o := newMinimalOwner()
	called := false
	o.onPersistentDetected = func(id string) { called = true }

	initJSON := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`)
	o.checkPersistent(initJSON)

	if called {
		t.Error("expected onPersistentDetected NOT to be called when no x-mux")
	}
}

func TestCheckPersistent_NilCallback(t *testing.T) {
	o := newMinimalOwner()
	// No callback set — should not panic
	initJSON := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"persistent":true}}}}`)
	o.checkPersistent(initJSON) // must not panic
}

// ---------------------------------------------------------------------------
// classifyFromCapabilities — unit tests (no upstream)
// ---------------------------------------------------------------------------

func TestClassifyFromCapabilities_Isolated(t *testing.T) {
	o := newMinimalOwner()

	// Must supply a fake listener so closeListener doesn't panic
	// We use a no-op approach: just verify the classification fields are set
	// without triggering closeListener (by supplying a capability = shared)
	initJSON := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"sharing":"shared"}}}}`)
	o.classifyFromCapabilities(initJSON)

	o.mu.RLock()
	mode := o.autoClassification
	src := o.classificationSource
	o.mu.RUnlock()

	if mode != "shared" {
		t.Errorf("classifyFromCapabilities: mode = %q, want shared", mode)
	}
	if src != "capability" {
		t.Errorf("classifyFromCapabilities: source = %q, want capability", src)
	}
}

func TestClassifyFromCapabilities_NoXMux(t *testing.T) {
	o := newMinimalOwner()

	// No x-mux — classification should remain empty
	initJSON := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`)
	o.classifyFromCapabilities(initJSON)

	o.mu.RLock()
	mode := o.autoClassification
	o.mu.RUnlock()

	if mode != "" {
		t.Errorf("classifyFromCapabilities: expected empty mode, got %q", mode)
	}
}

// ---------------------------------------------------------------------------
// classifyFromToolList — skipped when already classified by capability
// ---------------------------------------------------------------------------

func TestClassifyFromToolList_SkippedWhenCapabilitySet(t *testing.T) {
	o := newMinimalOwner()
	o.mu.Lock()
	o.classificationSource = "capability"
	o.autoClassification = "isolated"
	o.mu.Unlock()

	// toolsJSON with no isolation tools — if not skipped, would overwrite to "shared"
	toolsJSON := []byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo"}]}}`)
	o.classifyFromToolList(toolsJSON)

	o.mu.RLock()
	mode := o.autoClassification
	src := o.classificationSource
	o.mu.RUnlock()

	if mode != "isolated" {
		t.Errorf("classifyFromToolList should be skipped: mode = %q, want isolated", mode)
	}
	if src != "capability" {
		t.Errorf("classifyFromToolList should be skipped: source = %q, want capability", src)
	}
}

// ---------------------------------------------------------------------------
// initVersion — smoke test
// ---------------------------------------------------------------------------

func TestInitVersion_ReturnsString(t *testing.T) {
	v := initVersion()
	if v == "" {
		t.Error("initVersion() returned empty string")
	}
	// Should be either "dev" or a short commit hash
	if len(v) > 20 {
		t.Errorf("initVersion() suspiciously long: %q", v)
	}
}

// ---------------------------------------------------------------------------
// HandleShutdown — force and drain paths
// ---------------------------------------------------------------------------

func TestHandleShutdown_Force(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}

	msg := owner.HandleShutdown(0)
	if !strings.Contains(msg, "force") {
		t.Errorf("HandleShutdown(0) = %q, want 'force'", msg)
	}

	// Wait for shutdown to complete
	select {
	case <-owner.Done():
		// ok
	case <-time.After(5 * time.Second):
		t.Error("owner did not shut down after HandleShutdown(0)")
	}
}

func TestHandleShutdown_Drain(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}

	msg := owner.HandleShutdown(100)
	if !strings.Contains(msg, "draining") {
		t.Errorf("HandleShutdown(100) = %q, want 'draining'", msg)
	}

	// Wait for drain+shutdown
	select {
	case <-owner.Done():
		// ok
	case <-time.After(5 * time.Second):
		t.Error("owner did not shut down after drain")
	}
}

// ---------------------------------------------------------------------------
// RunClient — dial failure path (no server listening)
// ---------------------------------------------------------------------------

func TestRunClient_DialFailure(t *testing.T) {
	// Point at a socket that doesn't exist — RunClient should return an error
	err := RunClient("/nonexistent/mux.sock", strings.NewReader(""), io.Discard)
	if err == nil {
		t.Error("expected RunClient to return error when socket doesn't exist")
	}
	if !strings.Contains(err.Error(), "client: connect") {
		t.Errorf("RunClient error = %q, expected 'client: connect'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// respondToRootsList — triggered via roots/list request from upstream
// (need a mock upstream that sends roots/list)
// We call the method directly as it's on an owner with a fake upstream pipe.
// ---------------------------------------------------------------------------

func TestRespondToRootsList_WithCwd(t *testing.T) {
	ipcPath := testIPCPath(t)
	cwd := t.TempDir()

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Cwd:     cwd,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Call respondToRootsList directly
	id := json.RawMessage(`1`)
	if err := owner.respondToRootsList(id); err != nil {
		t.Errorf("respondToRootsList: %v", err)
	}
}

func TestRespondToRootsList_EmptyCwd(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Cwd:     "", // empty — should use os.Getwd()
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	id := json.RawMessage(`2`)
	if err := owner.respondToRootsList(id); err != nil {
		t.Errorf("respondToRootsList (empty cwd): %v", err)
	}
}

// ---------------------------------------------------------------------------
// respondWithError and respondToElicitationCancel — call directly
// ---------------------------------------------------------------------------

func TestRespondWithError(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	id := json.RawMessage(`5`)
	if err := owner.respondWithError(id, -32603, "test error"); err != nil {
		t.Errorf("respondWithError: %v", err)
	}
}

func TestRespondToElicitationCancel(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	id := json.RawMessage(`6`)
	if err := owner.respondToElicitationCancel(id); err != nil {
		t.Errorf("respondToElicitationCancel: %v", err)
	}
}

// ---------------------------------------------------------------------------
// routeProgressNotification — direct call with seeded progressOwners
// ---------------------------------------------------------------------------

func TestRouteProgressNotification_Routed(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Create a session and register it
	var buf safeBuf
	s := NewSession(strings.NewReader(""), &buf)

	owner.mu.Lock()
	owner.sessions[s.ID] = s
	owner.progressOwners[`"tok-1"`] = s.ID
	owner.mu.Unlock()

	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok-1","progress":50}}`)
	if err := owner.routeProgressNotification(raw); err != nil {
		t.Errorf("routeProgressNotification: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "tok-1") {
		t.Errorf("expected progress notification routed to session, got: %s", got)
	}
}

func TestRouteProgressNotification_NoOwner(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"unknown-tok","progress":10}}`)
	err = owner.routeProgressNotification(raw)
	if err == nil {
		t.Error("expected error when no owner for progressToken")
	}
}

// ---------------------------------------------------------------------------
// WriteRaw — non-bufio path (plain io.Writer)
// ---------------------------------------------------------------------------

func TestSessionWriteRaw_NonBufio(t *testing.T) {
	var buf bytes.Buffer
	// Create session with a plain writer (not bufio.Writer) by bypassing NewSession
	s := NewSessionWithRawWriter(999, &buf)

	data := []byte(`{"test":"nonbufio"}`)
	if err := s.WriteRaw(data); err != nil {
		t.Fatalf("WriteRaw (non-bufio): %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, `"test":"nonbufio"`) {
		t.Errorf("WriteRaw non-bufio: unexpected output: %s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, ""), "\n") {
		t.Errorf("WriteRaw non-bufio: expected trailing newline")
	}
}

// ---------------------------------------------------------------------------
// isHexChar
// ---------------------------------------------------------------------------

func TestIsHexChar(t *testing.T) {
	valid := "0123456789abcdef"
	for i := 0; i < len(valid); i++ {
		if !isHexChar(valid[i]) {
			t.Errorf("isHexChar(%q) = false, want true", valid[i])
		}
	}
	// uppercase and non-hex must return false
	invalid := []byte{'A', 'F', 'g', 'z', '{', ' ', '\n', 0x00}
	for _, b := range invalid {
		if isHexChar(b) {
			t.Errorf("isHexChar(%q) = true, want false", b)
		}
	}
}

// ---------------------------------------------------------------------------
// readToken — uses net.Pipe() which supports SetReadDeadline on Go 1.12+
// ---------------------------------------------------------------------------

// pipeWithData creates a net.Pipe pair, writes data to the client end, and
// returns the server-side conn for readToken to consume.
// The caller must close both conns when done.
func pipeWithData(t *testing.T, data []byte) (serverConn net.Conn, clientConn net.Conn) {
	t.Helper()
	c, s := net.Pipe()
	go func() {
		c.Write(data)
	}()
	return s, c
}

func TestReadToken_ValidHex(t *testing.T) {
	token := "abcdef1234567890"
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		client.Write([]byte(token + "\n"))
	}()

	got, _ := readToken(server)
	if got != token {
		t.Errorf("readToken valid hex: got %q, want %q", got, token)
	}
}

func TestReadToken_EmptyNewline(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		client.Write([]byte("\n"))
	}()

	got, _ := readToken(server)
	if got != "" {
		t.Errorf("readToken empty newline: got %q, want empty string", got)
	}
}

func TestReadToken_NonHexFirstByte(t *testing.T) {
	// '{' is not a hex char — readToken should return "" and prepend it via MultiReader
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	payload := `{"jsonrpc":"2.0"}` + "\n"
	go func() {
		client.Write([]byte(payload))
	}()

	got, reader := readToken(server)
	if got != "" {
		t.Errorf("readToken non-hex first byte: expected empty token, got %q", got)
	}
	// The reader should include the prepended '{' byte
	buf := make([]byte, 1)
	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("readToken non-hex: read prepended byte: %v", err)
	}
	if buf[0] != '{' {
		t.Errorf("readToken non-hex: prepended byte = %q, want '{'", buf[0])
	}
}

func TestReadToken_TooLong(t *testing.T) {
	// >64 hex chars without newline should trigger the length guard
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Write 65 'a' bytes then newline — triggers len(buf) > 64 guard before newline is read
	go func() {
		data := bytes.Repeat([]byte("a"), 65)
		data = append(data, '\n')
		client.Write(data)
	}()

	got, _ := readToken(server)
	if got != "" {
		t.Errorf("readToken too long: expected empty token, got %q", got)
	}
}

func TestReadToken_ErrorOnFirstRead(t *testing.T) {
	// Close the write end before readToken reads anything → EOF on first Read.
	// This simulates the timeout/error path: returns ("", conn).
	server, client := net.Pipe()
	defer server.Close()

	// Close client immediately — server's Read will return io.EOF
	client.Close()

	got, reader := readToken(server)
	if got != "" {
		t.Errorf("readToken error path: expected empty token, got %q", got)
	}
	if reader == nil {
		t.Error("readToken error path: reader should not be nil")
	}
}

func TestReadToken_ErrorAfterSomeBytes(t *testing.T) {
	// Write some valid hex bytes then close before newline.
	// Should return ("", MultiReader(buf, conn)).
	server, client := net.Pipe()
	defer server.Close()

	go func() {
		client.Write([]byte("abc")) // valid hex, no newline
		client.Close()
	}()

	got, reader := readToken(server)
	if got != "" {
		t.Errorf("readToken partial hex then close: expected empty token, got %q", got)
	}
	// reader should have the partial bytes prepended
	var buf bytes.Buffer
	io.Copy(&buf, reader)
	if !strings.HasPrefix(buf.String(), "abc") {
		t.Errorf("readToken partial bytes not prepended: got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// IsAccepting
// ---------------------------------------------------------------------------

func TestIsAccepting_NewOwner(t *testing.T) {
	o := newMinimalOwner()
	if !o.IsAccepting() {
		t.Error("IsAccepting() = false on a fresh owner, want true")
	}
}

func TestIsAccepting_AfterCloseListener(t *testing.T) {
	o := newMinimalOwner()
	// closeListener requires a real listener; signal the channel directly instead.
	close(o.listenerDone)
	if o.IsAccepting() {
		t.Error("IsAccepting() = true after listenerDone closed, want false")
	}
}

// ---------------------------------------------------------------------------
// AddCwd
// ---------------------------------------------------------------------------

func TestAddCwd_AddsNew(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	newCwd := t.TempDir()
	owner.AddCwd(newCwd)
	canonicalCwd := serverid.CanonicalizePath(newCwd)

	status := owner.Status()
	cwdSet, ok := status["cwd_set"].([]string)
	if !ok {
		t.Fatalf("cwd_set not []string in status: %T", status["cwd_set"])
	}

	found := false
	for _, c := range cwdSet {
		if c == canonicalCwd {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AddCwd: canonical cwd %q not in cwd_set %v", canonicalCwd, cwdSet)
	}
}

func TestAddCwd_NoDuplicate(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	newCwd := t.TempDir()
	owner.AddCwd(newCwd)
	owner.AddCwd(newCwd) // second add — should not duplicate
	canonicalCwd := serverid.CanonicalizePath(newCwd)

	owner.mu.RLock()
	count := 0
	for c := range owner.cwdSet {
		if c == canonicalCwd {
			count++
		}
	}
	owner.mu.RUnlock()

	if count != 1 {
		t.Errorf("AddCwd duplicate: canonical cwd appears %d times in cwdSet, want 1", count)
	}
}

func TestClassifyFromToolList_IsolatedResetsCwdSetToPrimary(t *testing.T) {
	o := newMinimalOwner()
	o.cwd = t.TempDir()
	o.cwdSet = map[string]bool{
		serverid.CanonicalizePath(o.cwd):       true,
		serverid.CanonicalizePath(t.TempDir()): true,
	}

	toolsJSON := []byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"activate_project"}]}}`)
	o.classifyFromToolList(toolsJSON)

	status := o.Status()
	if status["auto_classification"] != string(classify.ModeIsolated) {
		t.Fatalf("classification = %v, want %q", status["auto_classification"], classify.ModeIsolated)
	}

	cwdSet, ok := status["cwd_set"].([]string)
	if !ok {
		t.Fatalf("cwd_set not []string in status: %T", status["cwd_set"])
	}

	primaryCwd := serverid.CanonicalizePath(o.cwd)
	if len(cwdSet) != 1 {
		t.Fatalf("cwd_set size = %d, want 1 (%v)", len(cwdSet), cwdSet)
	}
	if cwdSet[0] != primaryCwd {
		t.Fatalf("cwd_set[0] = %q, want %q", cwdSet[0], primaryCwd)
	}
}

func TestClassifyFromCapabilities_IsolatedResetsCwdSetToPrimary(t *testing.T) {
	o := newMinimalOwner()
	o.cwd = t.TempDir()
	o.cwdSet = map[string]bool{
		serverid.CanonicalizePath(o.cwd):       true,
		serverid.CanonicalizePath(t.TempDir()): true,
	}

	initJSON := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{},"x-mux":{"sharing":"isolated"}}}}`)
	o.classifyFromCapabilities(initJSON)

	status := o.Status()
	if status["auto_classification"] != string(classify.ModeIsolated) {
		t.Fatalf("classification = %v, want %q", status["auto_classification"], classify.ModeIsolated)
	}

	cwdSet, ok := status["cwd_set"].([]string)
	if !ok {
		t.Fatalf("cwd_set not []string in status: %T", status["cwd_set"])
	}

	primaryCwd := serverid.CanonicalizePath(o.cwd)
	if len(cwdSet) != 1 {
		t.Fatalf("cwd_set size = %d, want 1 (%v)", len(cwdSet), cwdSet)
	}
	if cwdSet[0] != primaryCwd {
		t.Fatalf("cwd_set[0] = %q, want %q", cwdSet[0], primaryCwd)
	}
}

// ---------------------------------------------------------------------------
// drainInflightRequests
// ---------------------------------------------------------------------------

func TestDrainInflightRequests_SendsError(t *testing.T) {
	o := newMinimalOwner()
	o.sessionMgr = NewSessionManager()

	// Create a session backed by a buffer so we can inspect what's written to it
	var buf safeBuf
	s := NewSession(strings.NewReader(""), &buf)

	// Register the session
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()
	o.sessionMgr.RegisterSession(s, "")

	// Track a request: remapped ID for session s.ID, original numeric id 42
	// Remap format for numeric id: "s{N}:n:42"
	remappedStr := fmt.Sprintf("s%d:n:42", s.ID)
	o.sessionMgr.TrackRequest(remappedStr, s.ID)
	o.pendingRequests.Add(1)

	// Call drain
	o.drainInflightRequests()

	// Session should have received a JSON-RPC error response for id 42
	out := buf.String()
	if !strings.Contains(out, `"error"`) {
		t.Errorf("drainInflightRequests: session did not receive error response; got: %s", out)
	}
	if !strings.Contains(out, `"id":42`) {
		t.Errorf("drainInflightRequests: error response missing original id 42; got: %s", out)
	}
	if !strings.Contains(out, "upstream process exited") {
		t.Errorf("drainInflightRequests: error message missing; got: %s", out)
	}
	if o.pendingRequests.Load() != 0 {
		t.Errorf("drainInflightRequests: pendingRequests = %d, want 0", o.pendingRequests.Load())
	}
}

// ---------------------------------------------------------------------------
// routeToLastActiveSession
// ---------------------------------------------------------------------------

func TestRouteToLastActiveSession_RoutesToActiveSession(t *testing.T) {
	o := newMinimalOwner()
	o.sessionMgr = NewSessionManager()

	// Create a session backed by a buffer
	var buf safeBuf
	s := NewSession(strings.NewReader(""), &buf)

	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()
	o.sessionMgr.RegisterSession(s, "")

	// Track a request so ResolveCallback returns this session
	remappedID := fmt.Sprintf("s%d:n:7", s.ID)
	o.sessionMgr.TrackRequest(remappedID, s.ID)

	// Build a server→client request message using the internal jsonrpc.Parse
	raw := []byte(`{"jsonrpc":"2.0","id":99,"method":"roots/list","params":{}}`)
	msg, err := parseMsg(raw)
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}

	if err := o.routeToLastActiveSession(msg); err != nil {
		t.Errorf("routeToLastActiveSession: %v", err)
	}

	// Delivery is async via notification channel — wait for drain goroutine
	time.Sleep(50 * time.Millisecond)

	// Session should have received the message
	got := buf.String()
	if !strings.Contains(got, "roots/list") {
		t.Errorf("routeToLastActiveSession: session did not receive message; got: %s", got)
	}
}

func TestRouteToLastActiveSession_NoSession_ElicitationCreate(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// No sessions registered — ResolveCallback returns nil
	raw := []byte(`{"jsonrpc":"2.0","id":5,"method":"elicitation/create","params":{}}`)
	msg, err := parseMsg(raw)
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}

	// Should call respondToElicitationCancel which writes to upstream — no error
	if err := owner.routeToLastActiveSession(msg); err != nil {
		t.Errorf("routeToLastActiveSession elicitation/create fallback: %v", err)
	}
}

func TestRouteToLastActiveSession_NoSession_RootsList(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	raw := []byte(`{"jsonrpc":"2.0","id":6,"method":"roots/list","params":{}}`)
	msg, err := parseMsg(raw)
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}

	// Should call respondToRootsList which writes to upstream — no error
	if err := owner.routeToLastActiveSession(msg); err != nil {
		t.Errorf("routeToLastActiveSession roots/list fallback: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SessionMgr accessor
// ---------------------------------------------------------------------------

func TestSessionMgr_NonNil(t *testing.T) {
	o := newMinimalOwner()
	o.sessionMgr = NewSessionManager()

	if o.SessionMgr() == nil {
		t.Error("SessionMgr() returned nil, want non-nil *SessionManager")
	}
	if o.SessionMgr() != o.sessionMgr {
		t.Error("SessionMgr() returned wrong value")
	}
}

// ---------------------------------------------------------------------------
// parseMsg — test helper: wraps jsonrpc.Parse for concise test code
// ---------------------------------------------------------------------------

func parseMsg(raw []byte) (*jsonrpc.Message, error) {
	return jsonrpc.Parse(raw)
}
