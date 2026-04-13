package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thejerf/suture/v4"
)

// TestDaemonSupervisorInitialized verifies that the daemon creates a supervisor
// during New() and that it's reachable via the exported fields.
func TestDaemonSupervisorInitialized(t *testing.T) {
	// Use os.CreateTemp for a short path — t.TempDir() on macOS exceeds
	// the 108-byte Unix socket path limit.
	f, err := os.CreateTemp("", "daemon-ctl-*.sock")
	if err != nil {
		t.Fatalf("create temp socket path: %v", err)
	}
	ctlPath := f.Name()
	f.Close()
	os.Remove(ctlPath)
	t.Cleanup(func() { os.Remove(ctlPath) })

	d, err := New(Config{
		ControlPath:  ctlPath,
		Logger:       log.New(os.Stderr, "[test] ", 0),
		SkipSnapshot: true,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer d.Shutdown()

	if d.supervisor == nil {
		t.Error("daemon.supervisor is nil after New()")
	}
	if d.supervisorCtx == nil {
		t.Error("daemon.supervisorCtx is nil after New()")
	}
	if d.supervisorCancel == nil {
		t.Error("daemon.supervisorCancel is nil after New()")
	}
	if d.supervisorErr == nil {
		t.Error("daemon.supervisorErr is nil after New() — supervisor not started")
	}
}

// TestSupervisorEventHookFires verifies EventHook is invoked on service
// terminate events. Uses a standalone supervisor to isolate from daemon's
// lifecycle (avoid race with d.Shutdown cancelling early).
func TestSupervisorEventHookFires(t *testing.T) {
	var hookFires atomic.Int32
	done := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup := suture.New("test-hook", suture.Spec{
		EventHook: func(e suture.Event) {
			hookFires.Add(1)
			if _, ok := e.(suture.EventServiceTerminate); ok {
				select {
				case done <- struct{}{}:
				default:
				}
			}
		},
	})

	svc := &testService{
		fn: func(ctx context.Context) error {
			return errors.New("trigger terminate event")
		},
	}
	sup.Add(svc)
	errCh := sup.ServeBackground(ctx)

	// Wait for at least one EventServiceTerminate
	select {
	case <-done:
		// Event received — hook is working
	case <-time.After(3 * time.Second):
		t.Fatalf("EventHook not invoked within 3s (fires=%d)", hookFires.Load())
	}

	cancel()
	<-errCh

	if hookFires.Load() == 0 {
		t.Error("hookFires == 0 after terminate event")
	}
}

// TestSupervisorRestartsOnFailure verifies that a service returning a
// non-distinguished error is restarted by the supervisor. Demonstrates
// that suture's core restart mechanism works in our integration.
func TestSupervisorRestartsOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts atomic.Int32
	sup := suture.New("test-restart", suture.Spec{})

	svc := &testService{
		done: make(chan struct{}),
		fn: func(ctx context.Context) error {
			n := attempts.Add(1)
			if n >= 3 {
				return suture.ErrDoNotRestart // Stop after 3 attempts
			}
			return fmt.Errorf("attempt %d failed", n)
		},
	}
	sup.Add(svc)

	errCh := sup.ServeBackground(ctx)

	// Wait for 3 attempts
	deadline := time.After(5 * time.Second)
	for attempts.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d attempts after 5s, expected 3", attempts.Load())
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-errCh

	if got := attempts.Load(); got < 3 {
		t.Errorf("attempts = %d, want >= 3", got)
	}
}

// TestSupervisorExponentialBackoffOnFailureStorm verifies suture's backoff
// kicks in when a service fails repeatedly within the failure window.
// This is the safety-net feature we're integrating suture for.
func TestSupervisorExponentialBackoffOnFailureStorm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Configure aggressive thresholds for fast test execution:
	// failureDecay=1s, failureThreshold=3, failureBackoff=500ms
	sup := suture.New("test-backoff", suture.Spec{
		FailureDecay:     1.0,
		FailureThreshold: 3,
		FailureBackoff:   500 * time.Millisecond,
	})

	var attempts atomic.Int32
	var backoffFired atomic.Bool

	// Watch for backoff event via a second hook layer
	supWithHook := suture.New("test-backoff-hooked", suture.Spec{
		FailureDecay:     1.0,
		FailureThreshold: 3,
		FailureBackoff:   500 * time.Millisecond,
		EventHook: func(e suture.Event) {
			if _, ok := e.(suture.EventBackoff); ok {
				backoffFired.Store(true)
			}
		},
	})

	svc := &testService{
		done: make(chan struct{}),
		fn: func(ctx context.Context) error {
			attempts.Add(1)
			return errors.New("constant failure")
		},
	}
	supWithHook.Add(svc)

	errCh := supWithHook.ServeBackground(ctx)

	// Wait until backoff fires OR 3s timeout
	deadline := time.After(3 * time.Second)
	for !backoffFired.Load() {
		select {
		case <-deadline:
			t.Fatalf("backoff did not fire after %d attempts in 3s", attempts.Load())
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-errCh

	if !backoffFired.Load() {
		t.Error("EventBackoff not received — suture backoff mechanism not working")
	}
	_ = sup // keep reference to avoid unused warning
}

// testService is a minimal suture.Service implementation for tests.
type testService struct {
	mu   sync.Mutex
	done chan struct{}
	fn   func(context.Context) error
}

func (s *testService) Serve(ctx context.Context) error {
	return s.fn(ctx)
}
