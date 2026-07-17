package owner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

// TestShutdownForHandoff_HappyPath verifies that ShutdownForHandoff:
//   - returns a valid HandoffPayload with non-zero PID and FDs
//   - keeps o.Done open while the transfer is only prepared
//   - closes o.Done only after Commit settles transferred authority
//   - makes a subsequent Shutdown call a safe no-op
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

	// Preparation alone is not terminal: the predecessor retains the lease
	// until the final adoption decision settles Commit or Abort.
	select {
	case <-o.Done():
		t.Fatal("o.Done() closed before handoff settlement")
	default:
	}
	if err := payload.Commit(); err != nil {
		t.Fatalf("payload.Commit: %v", err)
	}
	select {
	case <-o.Done():
	case <-time.After(2 * time.Second):
		t.Error("o.Done() not closed after handoff commit")
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
	waitForCondition(t, 5*time.Second, func() bool { return o.MaterializationState() == MaterializationReady }, "owner did not finish initial materialization")

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

func TestShutdownForHandoffWaitsForMaterializationQuiescence(t *testing.T) {
	toolsSeen := make(chan struct{})
	releaseTools := make(chan struct{})
	var toolsOnce sync.Once
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"handoff","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				toolsOnce.Do(func() { close(toolsSeen) })
				<-releaseTools
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, OwnerSnapshot{})
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	o.SpawnUpstreamBackground()
	select {
	case <-toolsSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach discovery")
	}

	errCh := make(chan error, 1)
	go func() {
		_, handoffErr := o.ShutdownForHandoff()
		errCh <- handoffErr
	}()
	select {
	case err := <-errCh:
		t.Fatalf("handoff returned before materialization quiesced: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseTools)
	select {
	case err := <-errCh:
		if !errors.Is(err, upstream.ErrDetachUnsupported) {
			t.Fatalf("ShutdownForHandoff error=%v, want ErrDetachUnsupported", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handoff did not continue after materialization quiesced")
	}
	select {
	case <-o.Done():
	case <-time.After(time.Second):
		t.Fatal("handoff failure left owner incomplete")
	}
}

func TestHandoffAdoptsCacheWithoutProactiveInitialize(t *testing.T) {
	received := make(chan string, 1)
	proc := upstream.NewProcessFromHandler(context.Background(), func(_ context.Context, stdin io.Reader, _ io.Writer) error {
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			received <- scanner.Text()
		}
		return scanner.Err()
	})
	snap := controllerSnapshot()
	snap.Classification = "session-aware"
	o, err := newOwnerWithProcess(OwnerConfig{
		IPCPath:              testIPCPath(t),
		ServerID:             "handoff-cache-adoption",
		CachedClassification: snap.Classification,
		AdoptedSnapshot:      &snap,
		Logger:               testLogger(t),
	}, HandoffPayload{ServerID: "handoff-cache-adoption", Command: "adopted"}, proc)
	if err != nil {
		t.Fatalf("newOwnerWithProcess: %v", err)
	}
	defer o.Shutdown()
	if !o.CacheReady() || !strings.Contains(string(o.getCachedResponse("tools/list")), "old-tool") {
		t.Fatalf("handoff did not adopt coherent cache: %#v", o.Status())
	}
	select {
	case line := <-received:
		t.Fatalf("handoff sent duplicate proactive request: %s", line)
	case <-time.After(100 * time.Millisecond):
	}
}
