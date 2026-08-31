package owner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thejerf/suture/v4"
)

func TestOwnerServerID_AfterConstruction(t *testing.T) {
	ipcPath := testIPCPath(t)

	o, err := NewOwner(OwnerConfig{
		IPCPath:        ipcPath,
		SessionHandler: &mockSessionHandler{},
		ServerID:       "test-owner-server-id",
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer o.Shutdown()

	if got := o.ServerID(); got == "" {
		t.Fatal("ServerID() returned empty string")
	} else if got != "test-owner-server-id" {
		t.Fatalf("ServerID() = %q, want %q", got, "test-owner-server-id")
	}
}

// TestDecrementPending_ClampsAtZero is a regression test for the cosmetic bug
// where `pending_requests` appeared as `-1` in mux_list output. A late
// proactive-init response arriving after upstream death could fire Add(-1)
// without a matching Add(1), driving the counter negative. decrementPending
// clamps at zero while preserving correct decrement semantics otherwise.
func TestDecrementPending_ClampsAtZero(t *testing.T) {
	o := &Owner{}

	// Balanced increments / decrements behave normally.
	o.pendingRequests.Add(3)
	o.decrementPending()
	o.decrementPending()
	if got := o.pendingRequests.Load(); got != 1 {
		t.Errorf("after 3 Add(1) + 2 decrementPending, Load = %d, want 1", got)
	}

	// Extra decrements clamp at zero, never go negative.
	o.decrementPending()
	o.decrementPending()
	o.decrementPending()
	if got := o.pendingRequests.Load(); got != 0 {
		t.Errorf("after extra decrements, Load = %d, want 0 (must clamp)", got)
	}

	// Sanity: single Add(1) after the clamp recovers normal counting.
	o.pendingRequests.Add(1)
	if got := o.pendingRequests.Load(); got != 1 {
		t.Errorf("Add(1) after clamp, Load = %d, want 1", got)
	}
}

// TestDecrementPending_ConcurrentSafe stresses the CompareAndSwap loop from
// many goroutines: decrements from zero must all clamp, not race past each
// other into negatives.
func TestDecrementPending_ConcurrentSafe(t *testing.T) {
	o := &Owner{}

	const workers = 32
	const perWorker = 100

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < perWorker; j++ {
				o.decrementPending()
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := o.pendingRequests.Load(); got < 0 {
		t.Fatalf("concurrent decrements from zero produced negative counter: %d", got)
	}
}

// ---------------------------------------------------------------------------
// T008: TestNewOwner_SessionHandlerOnly_NoUpstream
// ---------------------------------------------------------------------------

// TestNewOwner_SessionHandlerOnly_NoUpstream verifies that NewOwner succeeds
// when only SessionHandler is set (no HandlerFunc, no Command), that the owner
// is created without an upstream process, that a session can dispatch a request
// to the handler, and that OnProjectConnect fires for lifecycle-aware handlers.
func TestNewOwner_SessionHandlerOnly_NoUpstream(t *testing.T) {
	ipcPath := testIPCPath(t)

	// Use the mockLifecycleHandler from dispatch_test.go — it implements both
	// SessionHandler and ProjectLifecycle (OnProjectConnect / OnProjectDisconnect).
	handler := &mockLifecycleHandler{}

	o, err := NewOwner(OwnerConfig{
		IPCPath:        ipcPath,
		SessionHandler: handler,
		ServerID:       "test-session-handler-only",
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() with SessionHandler only must not error, got: %v", err)
	}
	defer o.Shutdown()

	// Verify upstream is nil — we are in SessionHandler-only mode.
	if o.upstream != nil {
		t.Error("upstream must be nil in SessionHandler-only mode")
	}

	// Add a session and send an initialize request; the handler should receive it.
	cwd := "/test-session-handler-only-project"
	pr, pw := io.Pipe()
	buf := &safeBuf{}
	s := NewSession(pr, buf)
	s.Cwd = cwd
	o.AddSession(s)

	// Send an initialize request into the session's reader.
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}` + "\n"
	go func() {
		pw.Write([]byte(initReq))
		// Close so readSession exits after processing the single request.
		time.Sleep(200 * time.Millisecond)
		pw.Close()
	}()

	// Wait for the handler to receive the request.
	ok := waitCondition(t, 2*time.Second, func() bool {
		return len(handler.captured()) > 0
	})
	if !ok {
		t.Fatal("SessionHandler.HandleRequest was not called within timeout")
	}

	captured := handler.captured()
	if len(captured) < 1 {
		t.Fatalf("expected at least 1 captured request, got 0")
	}
	req := captured[0]
	if req.project.Cwd != cwd {
		t.Errorf("handler got Cwd=%q, want %q", req.project.Cwd, cwd)
	}
	if !strings.Contains(string(req.request), "initialize") {
		t.Errorf("handler did not receive initialize request; got: %s", req.request)
	}

	// Verify OnProjectConnect was called (handler implements ProjectLifecycle).
	ok = waitCondition(t, 2*time.Second, func() bool {
		return len(handler.capturedConnects()) > 0
	})
	if !ok {
		t.Fatal("OnProjectConnect was not called within timeout")
	}

	wantID := muxcore.ProjectContextID(cwd)
	connects := handler.capturedConnects()
	if len(connects) == 0 || connects[0] != wantID {
		t.Errorf("OnProjectConnect got IDs=%v, want first=%q", connects, wantID)
	}

	// Verify the session received a JSON-RPC response (mock handler returns result).
	ok = waitCondition(t, 2*time.Second, func() bool {
		return buf.String() != ""
	})
	if !ok {
		t.Fatal("session did not receive a response within timeout")
	}
	resp := buf.String()
	if !strings.Contains(resp, `"result"`) && !strings.Contains(resp, `"error"`) {
		t.Errorf("session response is not a JSON-RPC response: %s", resp)
	}

	// Verify owner shuts down cleanly.
	o.Shutdown()
	select {
	case <-o.Done():
		// Expected
	case <-time.After(2 * time.Second):
		t.Error("owner did not shut down within timeout after Shutdown()")
	}
}

// ---------------------------------------------------------------------------
// T009: TestServe_SessionHandlerOnly_BlocksUntilDone
// ---------------------------------------------------------------------------

// TestServe_SessionHandlerOnly_BlocksUntilDone verifies that Serve blocks
// (does not return immediately) on a SessionHandler-only owner, returns
// suture.ErrDoNotRestart on Shutdown() (not nil — a nil return would emit a
// clean-exit event that triggers cleanupDeadOwner and destroys any freshly-
// spawned replacement at the same server ID), and completes quickly.
func TestServe_SessionHandlerOnly_BlocksUntilDone(t *testing.T) {
	ipcPath := testIPCPath(t)

	handler := &mockSessionHandler{}

	o, err := NewOwner(OwnerConfig{
		IPCPath:        ipcPath,
		SessionHandler: handler,
		ServerID:       "test-serve-session-handler-only",
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	o.controlServer = nil // avoid control server in Shutdown()

	errCh := make(chan error, 1)
	go func() {
		errCh <- o.Serve(context.Background())
	}()

	// Verify Serve does NOT return immediately (it must block).
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-errCh:
		t.Fatalf("Serve returned immediately (should block): %v", err)
	default:
		// Correct: still blocking
	}

	start := time.Now()

	// Call Shutdown — Serve should return ErrDoNotRestart (not nil).
	// Returning nil would emit a clean-exit event that triggers cleanupDeadOwner,
	// which can destroy a freshly-spawned replacement at the same server ID —
	// the root cause of the supervisor restart-loop storm.
	o.Shutdown()

	select {
	case err := <-errCh:
		if !errors.Is(err, suture.ErrDoNotRestart) {
			t.Errorf("Serve returned %v after Shutdown, want suture.ErrDoNotRestart", err)
		}
		elapsed := time.Since(start)
		if elapsed > 200*time.Millisecond {
			t.Errorf("Serve took %v to return after Shutdown — possible goroutine stuck", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within 2s after Shutdown")
	}

	// Confirm done channel is closed (Shutdown was called).
	select {
	case <-o.Done():
		// Expected
	case <-time.After(500 * time.Millisecond):
		t.Error("done channel not closed after Shutdown")
	}
}

func TestSpawnUpstreamBackground_SessionHandlerOnly_NoSubprocess(t *testing.T) {
	ipcPath := testIPCPath(t)
	var logs bytes.Buffer
	snap := OwnerSnapshot{
		ServerID: "test-sessionhandler-background-spawn",
		Command:  "definitely-not-a-real-sessionhandler-upstream-command",
		Cwd:      t.TempDir(),
		Mode:     "global",
	}
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		Command:        snap.Command,
		Cwd:            snap.Cwd,
		IPCPath:        ipcPath,
		ServerID:       snap.ServerID,
		SessionHandler: &mockSessionHandler{},
		Logger:         log.New(&logs, "[test] ", 0),
	}, snap)
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot() error: %v", err)
	}
	defer o.Shutdown()

	o.SpawnUpstreamBackground()
	waitForCondition(t, time.Second, func() bool {
		return o.MaterializationState() == MaterializationReady
	}, "session-handler materialization did not reach ready state")
	o.mu.RLock()
	upstream := o.upstream
	o.mu.RUnlock()
	if upstream != nil {
		t.Fatalf("SessionHandler-only materialization created upstream: %v", upstream)
	}
	if strings.Contains(logs.String(), "upstream spawn failed") {
		t.Fatalf("SessionHandler-only path attempted subprocess:\n%s", logs.String())
	}

	errCh := make(chan error, 1)
	go func() { errCh <- o.Serve(context.Background()) }()
	select {
	case err := <-errCh:
		t.Fatalf("Serve returned immediately for SessionHandler-only owner: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	o.Shutdown()
	select {
	case err := <-errCh:
		if !errors.Is(err, suture.ErrDoNotRestart) {
			t.Fatalf("Serve returned %v after Shutdown, want suture.ErrDoNotRestart", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}

// ---------------------------------------------------------------------------
// T010: TestServe_ReturnsErrDoNotRestartAfterShutdown
// ---------------------------------------------------------------------------

// TestServe_ReturnsErrDoNotRestartAfterShutdown is the regression test for the
// supervisor restart-loop storm (see .agent/reports/2026-04-18-supervisor-restart-loop.md).
//
// Root cause: when suture retried Serve() on an already-shut-down owner, the
// early guard returned nil ("clean exit"), causing suture to fire a
// EventServiceTerminate{Err:nil} event. supervisorEventHook then called
// cleanupDeadOwner unconditionally, which destroyed any freshly-spawned
// replacement owner at the same server ID. The fix: return
// suture.ErrDoNotRestart instead of nil, so suture stops cycling the owner
// without emitting a clean-exit event.
//
// This test verifies that calling Serve() on an owner whose done channel is
// already closed returns suture.ErrDoNotRestart (not nil).
func TestServe_ReturnsErrDoNotRestartAfterShutdown(t *testing.T) {
	// Create a minimal owner with only the done channel initialized.
	// The main for-loop in Serve() hits the non-blocking done guard first,
	// so no upstream, listener, or sessions are needed.
	o := &Owner{
		done: make(chan struct{}),
	}
	// Simulate a Shutdown() call — close done without going through the full
	// Shutdown() method, which requires a properly initialized owner.
	close(o.done)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got := o.Serve(ctx)

	// Pre-fix: returned nil (clean exit) → triggered cleanupDeadOwner storm.
	// Post-fix: returns ErrDoNotRestart → suture stops cycling, no cleanup event.
	if got != suture.ErrDoNotRestart {
		t.Errorf("Serve on shut-down owner returned %v, want suture.ErrDoNotRestart", got)
	}
}

// ---------------------------------------------------------------------------
// T015: Native MCP 2026-07-28 owner behavior
// ---------------------------------------------------------------------------

func newModernWriterOwner(
	t *testing.T,
	writer io.Writer,
	onCacheReady func(*Owner, OwnerSnapshot) bool,
) *Owner {
	t.Helper()
	o, err := NewOwner(OwnerConfig{
		IPCPath:        testIPCPath(t),
		UpstreamWriter: writer,
		ProtocolEra:    era.EraModern20260728,
		OnCacheReady:   onCacheReady,
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner(modern): %v", err)
	}
	t.Cleanup(o.Shutdown)
	return o
}

func addModernOwnerSession(t *testing.T, o *Owner, cwd string) (*Session, *safeBuf) {
	t.Helper()
	session, output := newTestSession(cwd)
	o.admissionMu.Lock()
	o.addSessionLocked(session)
	o.admissionMu.Unlock()
	t.Cleanup(session.Close)
	return session, output
}

func TestModernOwner_NativeReadinessSkipsLegacyBootstrap(t *testing.T) {
	started := make(chan struct{}, 1)
	frames := make(chan []byte, 4)
	o, err := NewOwner(OwnerConfig{
		IPCPath:     testIPCPath(t),
		ProtocolEra: era.EraModern20260728,
		HandlerFunc: func(ctx context.Context, stdin io.Reader, _ io.Writer) error {
			select {
			case started <- struct{}{}:
			default:
			}
			scanner := bufio.NewScanner(stdin)
			for scanner.Scan() {
				frame := append([]byte(nil), scanner.Bytes()...)
				select {
				case frames <- frame:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return scanner.Err()
		},
		Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner(modern): %v", err)
	}
	t.Cleanup(o.Shutdown)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("modern upstream handler did not start")
	}
	select {
	case frame := <-frames:
		t.Fatalf("modern owner injected legacy bootstrap traffic before host demand: %s", frame)
	case <-time.After(150 * time.Millisecond):
	}

	if !o.IsClassifiedIsolated() {
		t.Fatal("modern owner is not forced isolated")
	}
	waitForCondition(t, time.Second, func() bool {
		return o.MaterializationState() == MaterializationReady
	}, "modern owner did not become ready without legacy discovery")
}

func TestModernOwner_PreservesNativeRequestsAndMRTRWithoutCacheOrReplay(t *testing.T) {
	var upstream safeBuf
	cachePublishes := 0
	o := newModernWriterOwner(t, &upstream, func(*Owner, OwnerSnapshot) bool {
		cachePublishes++
		return true
	})
	if !o.IsClassifiedIsolated() {
		t.Fatal("modern owner is not forced isolated")
	}
	session, downstream := addModernOwnerSession(t, o, t.TempDir())

	listRequest := []byte(`{"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}},"id":"modern-list-1","jsonrpc":"2.0"}`)
	if err := o.handleDownstreamMessage(session, parseMessage(listRequest)); err != nil {
		t.Fatalf("forward native tools/list: %v", err)
	}
	if got, want := upstream.String(), string(listRequest)+"\n"; got != want {
		t.Fatalf("first native request upstream bytes = %q, want exactly %q", got, want)
	}

	listResponse := []byte(`{"id":"modern-list-1","result":{"tools":[{"name":"modern_echo"}]},"jsonrpc":"2.0"}`)
	if err := o.handleUpstreamMessage(parseMessage(listResponse)); err != nil {
		t.Fatalf("route native tools/list response: %v", err)
	}
	if got, want := downstream.String(), string(listResponse)+"\n"; got != want {
		t.Fatalf("native tools/list response = %q, want byte-exact %q", got, want)
	}
	if cached := o.getCachedResponse("tools/list"); cached != nil {
		t.Fatalf("modern tools/list response entered legacy cache: %s", cached)
	}
	if cachePublishes != 0 {
		t.Fatalf("modern response invoked legacy cache/template publication %d time(s)", cachePublishes)
	}

	listRetry := []byte(`{"jsonrpc":"2.0","id":"modern-list-2","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if err := o.handleDownstreamMessage(session, parseMessage(listRetry)); err != nil {
		t.Fatalf("forward second native tools/list: %v", err)
	}
	wantUpstream := string(listRequest) + "\n" + string(listRetry) + "\n"
	if got := upstream.String(); got != wantUpstream {
		t.Fatalf("modern tools/list replayed cache or rewrote retry: got %q, want %q", got, wantUpstream)
	}

	callRequest := []byte(`{"params":{"name":"modern_echo","arguments":{"message":"native"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}},"jsonrpc":"2.0","id":"modern-call-1","method":"tools/call"}`)
	if err := o.handleDownstreamMessage(session, parseMessage(callRequest)); err != nil {
		t.Fatalf("forward native tools/call: %v", err)
	}
	wantUpstream += string(callRequest) + "\n"
	if got := upstream.String(); got != wantUpstream {
		t.Fatalf("native tools/call upstream bytes = %q, want exactly %q", got, wantUpstream)
	}

	inputRequired := []byte(`{"jsonrpc":"2.0","id":"modern-call-1","result":{"resultType":"input_required","inputRequests":{"fixture_confirmation":{"method":"elicitation/create","params":{"requestedSchema":{"type":"object","required":["confirmed"]}}}},"requestState":{"opaque":["preserve",{"byte":"exact"}]}}}`)
	if err := o.handleUpstreamMessage(parseMessage(inputRequired)); err != nil {
		t.Fatalf("route native input_required response: %v", err)
	}
	wantDownstream := string(listResponse) + "\n" + string(inputRequired) + "\n"
	if got := downstream.String(); got != wantDownstream {
		t.Fatalf("native input_required result was changed or misrouted: got %q, want %q", got, wantDownstream)
	}
}

func TestModernOwner_ContainsUpstreamJSONRPCRequests(t *testing.T) {
	var upstream safeBuf
	o := newModernWriterOwner(t, &upstream, nil)
	session, downstream := addModernOwnerSession(t, o, t.TempDir())

	demand := []byte(`{"jsonrpc":"2.0","id":"modern-call-active","method":"tools/call","params":{"name":"modern_echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if err := o.handleDownstreamMessage(session, parseMessage(demand)); err != nil {
		t.Fatalf("forward native request: %v", err)
	}

	serverRequest := []byte(`{"jsonrpc":"2.0","id":"upstream-server-request","method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"must stay contained"}}],"maxTokens":16}}`)
	if err := o.handleUpstreamMessage(parseMessage(serverRequest)); err != nil && !errors.Is(err, era.AdmissionContainedUpstreamRequest) {
		t.Fatalf("contain modern upstream request: %v", err)
	}
	if waitCondition(t, 200*time.Millisecond, func() bool {
		return downstream.String() != ""
	}) {
		t.Fatalf("modern upstream JSON-RPC request reached downstream: %s", downstream.String())
	}
}

func TestModernOwner_ForwardsOnlyOptedInStandardLogToSoleSession(t *testing.T) {
	var upstream safeBuf
	o := newModernWriterOwner(t, &upstream, nil)
	session, downstream := addModernOwnerSession(t, o, t.TempDir())

	standardLog := []byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","logger":"mock-modern-server","data":"request-scoped fixture log"}}`)
	if err := o.handleUpstreamMessage(parseMessage(standardLog)); err != nil {
		t.Fatalf("handle unopted standard log: %v", err)
	}
	if waitCondition(t, 150*time.Millisecond, func() bool {
		return downstream.String() != ""
	}) {
		t.Fatalf("standard log without request opt-in reached downstream: %s", downstream.String())
	}

	optedInRequest := []byte(`{"jsonrpc":"2.0","id":"modern-log-opt-in","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/logLevel":"info"}}}`)
	if err := o.handleDownstreamMessage(session, parseMessage(optedInRequest)); err != nil {
		t.Fatalf("forward log-opted native request: %v", err)
	}
	if got, want := upstream.String(), string(optedInRequest)+"\n"; got != want {
		t.Fatalf("modern logging opt-in generated or rewrote upstream traffic: got %q, want %q", got, want)
	}

	if err := o.handleUpstreamMessage(parseMessage(standardLog)); err != nil {
		t.Fatalf("handle opted-in standard log: %v", err)
	}
	if !waitCondition(t, time.Second, func() bool {
		return strings.Count(downstream.String(), string(standardLog)) == 1
	}) {
		t.Fatalf("opted-in standard log was not delivered once: %s", downstream.String())
	}
	time.Sleep(50 * time.Millisecond)
	if got, want := downstream.String(), string(standardLog)+"\n"; got != want {
		t.Fatalf("standard log was synthesized, duplicated, or transformed: got %q, want %q", got, want)
	}
}
