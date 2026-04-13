package owner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/classify"
	"github.com/thebtf/mcp-mux/internal/muxcore/upstream"
	"github.com/thejerf/suture/v4"
)

// mockLiveUpstream returns a minimal *upstream.Process with a Done channel
// that never closes — simulates a healthy, running upstream for Serve tests
// that need to block on ctx.Done or o.done instead of upstream death.
// Sets drainTimeout via SetDrainTimeout to a tiny value so that Shutdown→Close
// phase 2 (voluntary exit wait) returns quickly in tests.
func mockLiveUpstream() *upstream.Process {
	p := &upstream.Process{
		Done: make(chan struct{}),
	}
	// Make Close() phase-2 wait very short for test speed
	p.SetDrainTimeout(10 * time.Millisecond)
	return p
}

// TestOwnerServe_BlocksUntilCancel verifies that Serve blocks until
// the provided context is cancelled, then calls Shutdown and returns
// the context's error. Uses a mock live upstream so Serve blocks on
// ctx/done rather than returning immediately on upstream death.
func TestOwnerServe_BlocksUntilCancel(t *testing.T) {
	o := newMinimalOwner()
	o.controlServer = nil // avoid control server shutdown in Shutdown()
	mockUp := mockLiveUpstream()
	o.upstream = mockUp

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- o.Serve(ctx)
	}()

	// Give Serve time to enter its select
	time.Sleep(50 * time.Millisecond)

	// Verify it's blocking (no early return)
	select {
	case err := <-errCh:
		t.Fatalf("Serve returned early: %v", err)
	default:
	}

	// Cancel the context — Serve will observe ctx.Done() first (both
	// upstream Done and ctx.Done fire in select; we want ctx to win to
	// verify the cancellation path). Close mockUp.Done after a short
	// delay so Shutdown→Close can proceed quickly.
	cancel()
	go func() {
		// Give Serve's select time to pick ctx.Done() over upstream.Done.
		time.Sleep(20 * time.Millisecond)
		close(mockUp.Done)
	}()

	// Wait for Serve to return
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}

	// Verify Shutdown was called (done channel closed)
	select {
	case <-o.Done():
		// Expected: Shutdown closed the done channel
	case <-time.After(1 * time.Second):
		t.Error("Shutdown was not called (done channel not closed)")
	}
}

// TestOwnerServe_ReturnsNilOnCleanShutdown verifies that if Shutdown
// is called externally while Serve is waiting, Serve returns nil —
// telling suture not to restart.
func TestOwnerServe_ReturnsNilOnCleanShutdown(t *testing.T) {
	o := newMinimalOwner()
	o.controlServer = nil
	o.upstream = mockLiveUpstream() // prevent early return from upstream-dead path

	errCh := make(chan error, 1)
	go func() {
		errCh <- o.Serve(context.Background())
	}()

	// Give Serve time to enter blocking select
	time.Sleep(50 * time.Millisecond)

	// Call Shutdown — closes o.done, Serve returns nil
	o.Shutdown()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve after Shutdown returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}

// TestOwnerServe_IsolatedReturnsErrDoNotRestart verifies that when an
// isolated owner is served and the upstream is absent/dead, Serve returns
// suture.ErrDoNotRestart. With a nil upstream, upstreamDeadCh returns the
// package-level closedChan, so Serve observes "upstream dead" immediately.
// The classification branch should then convert this to ErrDoNotRestart.
func TestOwnerServe_IsolatedReturnsErrDoNotRestart(t *testing.T) {
	o := newMinimalOwner()
	o.controlServer = nil
	o.autoClassification = classify.ModeIsolated
	o.classificationSource = "tools"
	// Leave o.upstream = nil — upstreamDeadCh will return closedChan.

	err := o.Serve(context.Background())
	if !errors.Is(err, suture.ErrDoNotRestart) {
		t.Errorf("Serve returned %v, want suture.ErrDoNotRestart", err)
	}
}

// TestOwnerServe_NonIsolatedReturnsErrorOnUpstreamDead verifies that a
// non-isolated owner with a nil (dead) upstream returns a non-nil error
// to trigger supervisor restart with backoff.
func TestOwnerServe_NonIsolatedReturnsErrorOnUpstreamDead(t *testing.T) {
	o := newMinimalOwner()
	o.controlServer = nil
	o.autoClassification = classify.ModeShared
	o.classificationSource = "tools"

	err := o.Serve(context.Background())
	if err == nil {
		t.Error("Serve returned nil, want non-nil error for dead upstream")
	}
	if errors.Is(err, suture.ErrDoNotRestart) {
		t.Error("Serve returned ErrDoNotRestart for non-isolated owner")
	}
}

// TestOwnerUpstreamDeadCh_NilUpstreamReturnsClosedChannel verifies the
// helper returns closedChan when upstream is nil, so Serve can observe
// upstream absence as failure (not hang).
func TestOwnerUpstreamDeadCh_NilUpstreamReturnsClosedChannel(t *testing.T) {
	o := newMinimalOwner()
	ch := o.upstreamDeadCh()
	select {
	case <-ch:
		// Expected: closedChan is already closed
	case <-time.After(50 * time.Millisecond):
		t.Fatal("upstreamDeadCh with nil upstream must return a closed channel")
	}
}

// TestOwnerServe_ReturnsErrorOnUpstreamExit verifies that when a non-isolated
// upstream dies, Serve returns a non-nil error (which suture will use as
// signal to restart with backoff).
func TestOwnerServe_ReturnsErrorOnUpstreamExit(t *testing.T) {
	// This requires a real upstream.Process or a mock that can close Done.
	// Deferred to Phase 4 integration tests where we have the test infra.
	t.Skip("covered by Phase 4 TestSupervisor_ExponentialBackoff")
}

// Verify the Serve method signature matches suture.Service interface.
func TestOwnerServe_ImplementsServiceInterface(t *testing.T) {
	var _ suture.Service = (*Owner)(nil)
}
