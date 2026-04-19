package daemon

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
)

func TestLoadSnapshot_Reattach_HappyPath(t *testing.T) {
	os.Remove(SnapshotPath())

	sid := "aabbccdd-happy-reattach-test"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid,
				Command:        "echo",
				Args:           []string{"hello"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	defer os.Remove(SnapshotPath())

	// Write a handoff token file.
	tokenFile, err := os.CreateTemp("", "mcp-mux-handoff-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenFile.Close()
	const testToken = "happy-path-test-token"
	if err := os.WriteFile(tokenFile.Name(), []byte(testToken), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	defer os.Remove(tokenFile.Name())

	// Create real pipes — AttachFromFDs requires non-zero FDs.
	// stdinR/stdinW: upstream reads from stdinR; we write to stdinW (not used in test).
	// stdoutR/stdoutW: upstream writes to stdoutW; we read from stdoutR (not used in test).
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdin: %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	defer stdoutR.Close()
	defer stdoutW.Close()

	// Prepare mock conn pair: oldDaemonConn (sender) <--> successorConn (receiver).
	oldDaemonConn, successorConn := newMockFDConnPair()

	// Run the old-daemon (sender) side in a background goroutine.
	// Transfer stdinR.Fd() as StdinFD and stdoutW.Fd() as StdoutFD.
	// These are the pipe ends the upstream process would read/write.
	// Capture the performHandoff error on a buffered channel so we can
	// propagate sender-side failures to the test instead of relying on
	// receiveHandoff to time out on a half-done protocol.
	senderErrCh := make(chan error, 1)
	go func() {
		upstreams := []HandoffUpstream{
			{
				ServerID: sid,
				Command:  "echo",
				PID:      os.Getpid(), // valid PID > 0; not verified by AttachFromFDs
				StdinFD:  stdinR.Fd(),
				StdoutFD: stdoutW.Fd(),
			},
		}
		ctx := context.Background()
		_, err := performHandoff(ctx, oldDaemonConn, testToken, upstreams)
		senderErrCh <- err
	}()
	t.Cleanup(func() {
		select {
		case err := <-senderErrCh:
			if err != nil {
				t.Errorf("performHandoff (sender): %v", err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("performHandoff (sender) did not return within 500ms")
		}
	})

	// Install dialHandoffHook to return the successor side of the mock conn pair.
	origHook := dialHandoffHook
	dialHandoffHook = func(_ string, _ time.Duration) (fdConn, error) {
		return successorConn, nil
	}
	defer func() { dialHandoffHook = origHook }()

	// Set handoff env vars.
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenFile.Name())
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "/mock/socket") // intercepted by hook

	d := testDaemon(t)
	restored := d.loadSnapshot()
	if restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
	}

	var entry *OwnerEntry
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.RLock()
		entry = d.owners[sid]
		d.mu.RUnlock()
		if entry != nil && entry.Owner != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner not found in d.owners after handoff reattach (polled 2s)")
	}

	// Verify owner was constructed via handoff path: classification_source == "handoff".
	status := entry.Owner.Status()
	classSource, _ := status["classification_source"].(string)
	if classSource != "handoff" {
		t.Errorf("classification_source = %q, want %q (handoff path not used)", classSource, "handoff")
	}
}

func TestLoadSnapshot_Reattach_SocketUnreachable(t *testing.T) {
	os.Remove(SnapshotPath())

	sid := "aabbccdd-fallback-reattach-test"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid,
				Command:        "echo",
				Args:           []string{"fallback"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	defer os.Remove(SnapshotPath())

	// Write handoff token pointing at a valid file.
	tokenFile, err := os.CreateTemp("", "mcp-mux-handoff-fallback-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenFile.Close()
	if err := os.WriteFile(tokenFile.Name(), []byte("some-token"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	defer os.Remove(tokenFile.Name())

	// Set env vars with an invalid socket path (guaranteed to fail dial).
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenFile.Name())
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "/nonexistent/path/to/socket.sock")

	d := testDaemon(t)
	restored := d.loadSnapshot()

	// FR-8 fallback: must restore owner via legacy path (SpawnUpstreamBackground).
	if restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners (want 1); FR-8 fallback failed", restored)
	}

	var entry *OwnerEntry
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.RLock()
		entry = d.owners[sid]
		d.mu.RUnlock()
		if entry != nil && entry.Owner != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if entry == nil || entry.Owner == nil {
		t.Fatal("owner not found in d.owners after FR-8 fallback (polled 2s)")
	}

	// Owner must NOT have been reattached via handoff (socket was unreachable).
	status := entry.Owner.Status()
	classSource, _ := status["classification_source"].(string)
	if classSource == "handoff" {
		t.Error("classification_source = 'handoff' but socket was unreachable; expected legacy path")
	}
}

// TestLoadSnapshot_Reattach_PartialHandoff exercises FR-7: when the handoff
// delivers FDs for a subset of the snapshot's owners, the remainder MUST
// fall through to the legacy SpawnUpstreamBackground path. The snapshot
// lists two owners (sid1, sid2) but performHandoff only transfers sid1 —
// so sid1 reattaches via handoff (classification_source == "handoff") and
// sid2 comes back through the legacy path (classification_source !=
// "handoff"). Both must be present in d.owners after loadSnapshot returns.
func TestLoadSnapshot_Reattach_PartialHandoff(t *testing.T) {
	os.Remove(SnapshotPath())

	sid1 := "aabbccdd-partial-handoff-sid1"
	sid2 := "eeff0011-partial-handoff-sid2"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid1,
				Command:        "echo",
				Args:           []string{"one"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
			{
				ServerID:       sid2,
				Command:        "echo",
				Args:           []string{"two"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	defer os.Remove(SnapshotPath())

	tokenFile, err := os.CreateTemp("", "mcp-mux-handoff-partial-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenFile.Close()
	const testToken = "partial-handoff-test-token"
	if err := os.WriteFile(tokenFile.Name(), []byte(testToken), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	defer os.Remove(tokenFile.Name())

	// Real pipes for sid1 only — sid2 is intentionally omitted from the
	// handoff payload so its reattach fails and the legacy path kicks in.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdin: %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	defer stdoutR.Close()
	defer stdoutW.Close()

	oldDaemonConn, successorConn := newMockFDConnPair()

	senderErrCh := make(chan error, 1)
	go func() {
		upstreams := []HandoffUpstream{
			{
				ServerID: sid1, // Only sid1 transferred — sid2 falls through.
				Command:  "echo",
				PID:      os.Getpid(),
				StdinFD:  stdinR.Fd(),
				StdoutFD: stdoutW.Fd(),
			},
		}
		_, err := performHandoff(context.Background(), oldDaemonConn, testToken, upstreams)
		senderErrCh <- err
	}()
	t.Cleanup(func() {
		select {
		case err := <-senderErrCh:
			if err != nil {
				t.Errorf("performHandoff (sender): %v", err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("performHandoff (sender) did not return within 500ms")
		}
	})

	origHook := dialHandoffHook
	dialHandoffHook = func(_ string, _ time.Duration) (fdConn, error) {
		return successorConn, nil
	}
	defer func() { dialHandoffHook = origHook }()

	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenFile.Name())
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "/mock/socket")

	d := testDaemon(t)
	restored := d.loadSnapshot()
	if restored != 2 {
		t.Fatalf("loadSnapshot() restored %d owners, want 2 (sid1 via handoff, sid2 via legacy)", restored)
	}

	// Capture each entry independently. sid2 goes through the legacy spawn
	// path with "echo two" which exits after ~1ms — onUpstreamExit fires
	// and removes d.owners[sid2] almost immediately. Requiring both owners
	// to be visible in the SAME poll iteration would race on fast macOS
	// scheduling (sid2 is already gone by the time sid1 is observed).
	// Capture each pointer once when it first appears; the Owner struct
	// survives subsequent removal from the map so Status() is still safe.
	var entry1, entry2 *OwnerEntry
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.RLock()
		if entry1 == nil {
			if e := d.owners[sid1]; e != nil && e.Owner != nil {
				entry1 = e
			}
		}
		if entry2 == nil {
			if e := d.owners[sid2]; e != nil && e.Owner != nil {
				entry2 = e
			}
		}
		d.mu.RUnlock()
		if entry1 != nil && entry2 != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if entry1 == nil {
		t.Fatal("sid1 never appeared in d.owners after partial handoff (polled 2s)")
	}
	if entry2 == nil {
		t.Fatal("sid2 never appeared in d.owners after partial handoff (polled 2s)")
	}

	// sid1: transferred via handoff → classification_source == "handoff"
	status1 := entry1.Owner.Status()
	if class1, _ := status1["classification_source"].(string); class1 != "handoff" {
		t.Errorf("sid1 classification_source = %q, want %q (handoff path not used)", class1, "handoff")
	}

	// sid2: not in handoff payload → legacy spawn path, NOT "handoff"
	status2 := entry2.Owner.Status()
	if class2, _ := status2["classification_source"].(string); class2 == "handoff" {
		t.Errorf("sid2 classification_source = %q but sid2 was not in handoff; expected legacy path", class2)
	}
}
