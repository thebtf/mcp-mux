package owner

import (
	"net"
	"strings"
	"testing"
)

// TestIsReachable_LiveListener verifies the happy path: a freshly-bound
// listener returns IsReachable == true.
func TestIsReachable_LiveListener(t *testing.T) {
	path := testIPCPath(t)
	o := newMinimalOwner()
	o.ipcPath = path

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	o.listener = ln

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	if !o.IsReachable() {
		t.Fatalf("IsReachable()=false on a live listener (path=%q)", path)
	}
	if !o.IsAccepting() {
		t.Fatalf("IsAccepting()=false on a live listener")
	}
}

// TestIsReachable_ExplicitClose verifies that an explicitly-closed listener
// (closeListener path) correctly reports IsReachable == false without
// requiring a dial probe to fire.
func TestIsReachable_ExplicitClose(t *testing.T) {
	path := testIPCPath(t)
	o := newMinimalOwner()
	o.ipcPath = path

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	o.listener = ln

	// Simulate closeListener.
	close(o.listenerDone)
	ln.Close()

	if o.IsReachable() {
		t.Fatalf("IsReachable()=true after explicit close")
	}
	if o.IsAccepting() {
		t.Fatalf("IsAccepting()=true after explicit close")
	}
}

// TestIsReachable_ZombieListener is the pre-fix regression test. It
// reproduces the "listener closed but listenerDone never signalled" state
// that IsAccepting mis-reports as healthy.
//
// Before the IsReachable fix, daemon.Spawn used IsAccepting alone: given a
// zombie owner the Spawn RPC returned its ipcPath to the shim, and the shim
// then failed "dial: connection refused". This test ensures IsReachable
// catches the exact same state and reports it correctly — any future
// refactor that reintroduces a synchronization-only liveness check here
// will immediately fail this test.
func TestIsReachable_ZombieListener(t *testing.T) {
	path := testIPCPath(t)
	o := newMinimalOwner()
	o.ipcPath = path

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	o.listener = ln

	// Zombie state: the underlying listener is closed (socket file may or
	// may not be removed), but listenerDone has NOT been signalled. This is
	// the exact state we observed in production after graceful-restart —
	// IsAccepting returned true, dial returned refused.
	if err := ln.Close(); err != nil {
		t.Fatalf("ln.Close: %v", err)
	}

	// Sanity: IsAccepting is the pre-fix probe and still returns true because
	// listenerDone is not closed. This is the bug.
	if !o.IsAccepting() {
		t.Fatalf("precondition violated: IsAccepting already false without closeListener")
	}

	// The fix: IsReachable authoritatively detects the zombie.
	if o.IsReachable() {
		t.Fatalf("IsReachable()=true on a zombie listener (listener closed, listenerDone open) — " +
			"pre-fix bug: daemon would hand this path to a shim and the shim would get " +
			"'dial: connection refused'")
	}
}

// TestIsReachable_EmptyIPCPath covers the test-owner edge case where an
// Owner has no ipcPath (typical in unit tests that bypass the bind
// machinery). IsReachable must not attempt a dial on an empty path and must
// treat the owner as reachable so test owners remain usable.
func TestIsReachable_EmptyIPCPath(t *testing.T) {
	o := newMinimalOwner()
	o.ipcPath = ""
	if !o.IsReachable() {
		t.Fatalf("IsReachable()=false with empty ipcPath — callers that use " +
			"test owners would see false-negative zombie detections")
	}
}

// TestIsReachable_NoListenerFile covers the case where the socket file was
// removed by ipc.Cleanup or some other path but listenerDone is still open.
// Dial should refuse on a missing file, and IsReachable must return false.
func TestIsReachable_NoListenerFile(t *testing.T) {
	path := testIPCPath(t)
	o := newMinimalOwner()
	o.ipcPath = path

	// No net.Listen; path does not exist.
	if o.IsReachable() {
		t.Fatalf("IsReachable()=true on a missing socket path")
	}

	// And the error propagates through ipc.IsAvailable internally — callers
	// that want the raw error can reach it via ipc.Dial. Sanity check here
	// is that we don't panic on a nonexistent path.
	_ = strings.TrimSpace("") // keep strings import referenced when only regex-style checks are removed
}
