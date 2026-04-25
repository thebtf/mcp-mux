package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

// TestGracefulRestart_ResponseDelivery verifies that the control client
// receives the "snapshot written, shutting down" response from
// HandleGracefulRestart before the daemon exits. This is the exact
// production path: control.SendWithTimeout → handleConn → dispatch →
// HandleGracefulRestart → writeResponse → afterFn → Shutdown.
//
// Regression test for #99 (response lost on Windows AF_UNIX).
func TestGracefulRestart_ResponseDelivery(t *testing.T) {
	setupTestHandoffTimeouts(t)

	ctlPath := shortSocketPath(t, "gr-response.ctl.sock")
	d, err := New(Config{
		ControlPath:  ctlPath,
		GracePeriod:  1 * time.Second,
		IdleTimeout:  5 * time.Second,
		SkipSnapshot: true,
		Logger:       testLogger(t),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	// Send graceful-restart via the real control client — same code path
	// as ctltest / upgrade --restart.
	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:            "graceful-restart",
		DrainTimeoutMs: 5000,
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("SendWithTimeout: %v", err)
	}
	if !resp.OK {
		t.Fatalf("response not OK: %s", resp.Message)
	}
	if resp.Message != "snapshot written, shutting down" {
		t.Errorf("message = %q, want %q", resp.Message, "snapshot written, shutting down")
	}
	t.Logf("response received: ok=%v message=%q", resp.OK, resp.Message)

	// Verify daemon shuts down within a reasonable time.
	select {
	case <-d.Done():
		t.Log("daemon shut down successfully")
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down within 10s")
	}

	// Cleanup snapshot file if produced.
	if path := SnapshotPath(); path != "" {
		os.Remove(path)
	}
}

// TestGracefulRestart_ResponseDelivery_WithOwner exercises the same path
// but with a live owner — closer to the real-world scenario where
// Shutdown has actual work to do (close owners, stop supervisor).
func TestGracefulRestart_ResponseDelivery_WithOwner(t *testing.T) {
	setupTestHandoffTimeouts(t)

	ctlPath := shortSocketPath(t, "gr-owner.ctl.sock")
	d, err := New(Config{
		ControlPath:      ctlPath,
		OwnerIdleTimeout: 1 * time.Minute,
		IdleTimeout:      5 * time.Second,
		SkipSnapshot:     true,
		Logger:           testLogger(t),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	// Spawn a mock owner so Shutdown has real work.
	_, _, _, err = d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Cwd:     ".",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:            "graceful-restart",
		DrainTimeoutMs: 5000,
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("SendWithTimeout: %v", err)
	}
	if !resp.OK {
		t.Fatalf("response not OK: %s", resp.Message)
	}
	t.Logf("response received: ok=%v message=%q (with live owner)", resp.OK, resp.Message)

	select {
	case <-d.Done():
		t.Log("daemon with owner shut down successfully")
	case <-time.After(15 * time.Second):
		t.Fatal("daemon with owner did not shut down within 15s")
	}

	os.Remove(SnapshotPath())
}

// TestGracefulRestart_BackToBack sends two graceful-restart commands
// to two sequential daemons. Reproduces the Phase 1 OK / Phase 2 FAIL
// pattern observed in aimux testing.
func TestGracefulRestart_BackToBack(t *testing.T) {
	setupTestHandoffTimeouts(t)

	for i := 1; i <= 2; i++ {
		ctlPath := shortSocketPath(t, "gr-b2b.ctl.sock")
		d, err := New(Config{
			ControlPath:  ctlPath,
			GracePeriod:  1 * time.Second,
			IdleTimeout:  5 * time.Second,
			SkipSnapshot: true,
			Logger:       testLogger(t),
		})
		if err != nil {
			t.Fatalf("Phase %d: New(): %v", i, err)
		}

		resp, err := control.SendWithTimeout(ctlPath, control.Request{
			Cmd:            "graceful-restart",
			DrainTimeoutMs: 5000,
		}, 30*time.Second)
		if err != nil {
			t.Fatalf("Phase %d: SendWithTimeout: %v", i, err)
		}
		if !resp.OK {
			t.Fatalf("Phase %d: response not OK: %s", i, resp.Message)
		}
		t.Logf("Phase %d: response received: ok=%v", i, resp.OK)

		select {
		case <-d.Done():
			t.Logf("Phase %d: daemon shut down", i)
		case <-time.After(10 * time.Second):
			t.Fatalf("Phase %d: daemon did not shut down", i)
		}

		os.Remove(SnapshotPath())
	}
}
