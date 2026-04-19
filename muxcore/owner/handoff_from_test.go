package owner

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

// TestNewOwnerFromHandoff_ListenerStarts verifies that newOwnerWithProcess:
//   - succeeds without error
//   - starts the IPC listener so a dial to ipcPath succeeds
//   - does NOT immediately close o.Done() (owner remains alive)
func TestNewOwnerFromHandoff_ListenerStarts(t *testing.T) {
	ipcPath := testIPCPath(t)

	// Use a handler-based Process as a stand-in for a real FD-transferred process.
	// NewProcessFromHandler produces a *Process with PID()==0 — no subprocess spawned.
	hctx, hcancel := context.WithCancel(context.Background())
	proc := upstream.NewProcessFromHandler(hctx,
		func(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
			<-ctx.Done()
			return nil
		})

	payload := HandoffPayload{
		ServerID: "test-handoff-listener",
		Command:  "mock-handler",
	}

	o, err := newOwnerWithProcess(OwnerConfig{
		IPCPath:  ipcPath,
		ServerID: payload.ServerID,
		Logger:   testLogger(t),
	}, payload, proc)
	if err != nil {
		t.Fatalf("newOwnerWithProcess: %v", err)
	}
	// Cancel handler FIRST on teardown (lets proc.Done close), THEN shutdown
	// the owner (which needs upstream exit to complete cleanly). Defers run in
	// LIFO order — Shutdown registered last fires first, but it blocks on
	// upstream exit; hcancel registered earlier triggers that exit.
	defer o.Shutdown()
	defer hcancel()

	// Verify the IPC listener is up by dialling the socket.
	var conn net.Conn
	var dialErr error
	for i := 0; i < 20; i++ {
		conn, dialErr = net.DialTimeout("unix", ipcPath, 200*time.Millisecond)
		if dialErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("IPC socket %s not reachable after newOwnerWithProcess: %v", ipcPath, dialErr)
	}
	conn.Close()

	// o.Done() must NOT be closed — the owner should still be running.
	select {
	case <-o.Done():
		t.Error("o.Done() was closed immediately after construction; owner must remain alive")
	default:
		// expected
	}
}

// TestNewOwnerFromHandoff_NoSpawn verifies that newOwnerWithProcess does not
// spawn any subprocess. A handler-based Process has PID() == 0, which proves
// that procgroup.Spawn was never called.
func TestNewOwnerFromHandoff_NoSpawn(t *testing.T) {
	ipcPath := testIPCPath(t)

	hctx, hcancel := context.WithCancel(context.Background())
	proc := upstream.NewProcessFromHandler(hctx,
		func(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
			<-ctx.Done()
			return nil
		})

	payload := HandoffPayload{
		ServerID: "test-no-spawn",
		Command:  "mock-handler",
	}

	o, err := newOwnerWithProcess(OwnerConfig{
		IPCPath:  ipcPath,
		ServerID: "test-no-spawn",
		Logger:   testLogger(t),
	}, payload, proc)
	if err != nil {
		t.Fatalf("newOwnerWithProcess: %v", err)
	}
	defer o.Shutdown()
	defer hcancel()

	if o.upstream == nil {
		t.Fatal("o.upstream must not be nil after newOwnerWithProcess")
	}
	// Handler-based process has PID() == 0 — proves no OS subprocess was created.
	if pid := o.upstream.PID(); pid != 0 {
		t.Errorf("o.upstream.PID() = %d, want 0 (handler process = no subprocess spawned)", pid)
	}
}
