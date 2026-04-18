package owner

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thejerf/suture/v4"
)

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
