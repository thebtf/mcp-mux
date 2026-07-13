package owner

import (
	"errors"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

// TestShutdownForHandoff_HappyPath verifies that ShutdownForHandoff:
//   - returns a valid HandoffPayload with non-zero PID and FDs
//   - closes o.Done() channel on success
//   - makes a subsequent Shutdown() call a safe no-op (no panic, no block)
func TestShutdownForHandoff_HappyPath(t *testing.T) {
	ipcPath := testIPCPath(t)
	cmd, args := mockServerArgs()

	o, err := NewOwner(OwnerConfig{
		Command:  cmd,
		Args:     args,
		IPCPath:  ipcPath,
		ServerID: "test-handoff-happy",
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}

	// Give upstream a moment to start before detaching.
	time.Sleep(300 * time.Millisecond)

	payload, err := o.ShutdownForHandoff()
	if err != nil {
		t.Fatalf("ShutdownForHandoff: %v", err)
	}
	if payload.PID == 0 {
		t.Error("payload.PID must be > 0")
	}
	if payload.StdinFD == 0 {
		t.Error("payload.StdinFD must be > 0")
	}
	if payload.StdoutFD == 0 {
		t.Error("payload.StdoutFD must be > 0")
	}
	if payload.StderrFD == 0 {
		t.Error("payload.StderrFD must be > 0")
	}
	if payload.ServerID != "test-handoff-happy" {
		t.Errorf("payload.ServerID = %q, want %q", payload.ServerID, "test-handoff-happy")
	}

	// Done channel must be closed after a successful handoff.
	select {
	case <-o.Done():
		// ok
	case <-time.After(2 * time.Second):
		t.Error("o.Done() not closed after ShutdownForHandoff")
	}

	// Subsequent Shutdown() must be a safe no-op: no panic, no indefinite block.
	done := make(chan struct{})
	go func() {
		o.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		// ok — Shutdown returned immediately because shutdownOnce already fired
	case <-time.After(2 * time.Second):
		t.Error("Shutdown() after ShutdownForHandoff blocked or panicked")
	}
}

// TestShutdownForHandoff_AfterShutdown verifies that calling ShutdownForHandoff
// after Shutdown has already run returns ErrAlreadyShutDown.
func TestShutdownForHandoff_AfterShutdown(t *testing.T) {
	ipcPath := testIPCPath(t)
	cmd, args := mockServerArgs()

	o, err := NewOwner(OwnerConfig{
		Command:  cmd,
		Args:     args,
		IPCPath:  ipcPath,
		ServerID: "test-handoff-after-shutdown",
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}

	o.Shutdown()

	_, err = o.ShutdownForHandoff()
	if !errors.Is(err, ErrAlreadyShutDown) {
		t.Errorf("ShutdownForHandoff after Shutdown: got %v, want ErrAlreadyShutDown", err)
	}
}

func TestShutdownForHandoff_NoUpstreamCompletesOwner(t *testing.T) {
	o, err := NewOwner(OwnerConfig{
		IPCPath:        testIPCPath(t),
		SessionHandler: noopSessionHandler{},
		ServerID:       "test-handoff-no-upstream",
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}

	_, err = o.ShutdownForHandoff()
	if !errors.Is(err, ErrNoUpstream) {
		t.Fatalf("ShutdownForHandoff: got %v, want ErrNoUpstream", err)
	}
	select {
	case <-o.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("owner remained in limbo after no-upstream handoff failure")
	}
}

func TestShutdownForHandoff_DetachFailureAbortsTreeAndCompletesOwner(t *testing.T) {
	cmd, args := mockServerArgs()
	o, err := NewOwner(OwnerConfig{
		Command:  cmd,
		Args:     args,
		IPCPath:  testIPCPath(t),
		ServerID: "test-handoff-detach-failure",
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}

	o.mu.RLock()
	proc := o.upstream
	o.mu.RUnlock()
	if proc == nil {
		t.Fatal("expected subprocess upstream")
	}
	if _, _, _, _, _, err := proc.DetachWithAuthority(); err != nil {
		t.Fatalf("prepare conflicting detach: %v", err)
	}

	_, err = o.ShutdownForHandoff()
	if !errors.Is(err, upstream.ErrAlreadyDetached) {
		t.Fatalf("ShutdownForHandoff: got %v, want ErrAlreadyDetached", err)
	}
	select {
	case <-o.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("owner remained in limbo after detach failure")
	}
	select {
	case <-proc.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream tree survived detach failure cleanup")
	}
}
