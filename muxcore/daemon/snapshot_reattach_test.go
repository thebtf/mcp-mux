package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
)

// syncBuffer is a thread-safe bytes.Buffer — the daemon writes logs from
// multiple goroutines concurrently (supervisor, reaper, owner callbacks).
// Tests that inspect log content need serialized reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// testDaemonWithLog constructs a daemon whose logger writes into a
// syncBuffer in addition to stderr, so log-based assertions can inspect
// daemon output without racing with concurrent writers.
func testDaemonWithLog(t *testing.T) (*Daemon, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	ctlPath := shortSocketPath(t, "daemon.ctl.sock")
	logger := log.New(buf, "[daemon-test] ", log.LstdFlags)
	d, err := New(Config{
		ControlPath:  ctlPath,
		GracePeriod:  1 * time.Second,
		IdleTimeout:  5 * time.Second,
		SkipSnapshot: true,
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	return d, buf
}

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

	d, logBuf := testDaemonWithLog(t)
	restored := d.loadSnapshot()
	if restored != 2 {
		t.Fatalf("loadSnapshot() restored %d owners, want 2 (sid1 via handoff, sid2 via legacy)", restored)
	}

	// FR-7 is verified by structural log inspection rather than by probing
	// d.owners — in the partial scenario both owners get removed from the
	// map almost immediately after insertion (sid1's proactive init writes
	// to a stub pipe with no reader and errors out; sid2 spawns "echo two"
	// which exits cleanly after ~1ms). The code path that routes each
	// owner through handoff vs. legacy is already complete by the time
	// loadSnapshot returns, and daemon.Logger emits a stable marker per
	// path: "snapshot: reattached owner <sid> from handoff" for the
	// handoff path, "snapshot: restored owner <sid> for <cmd> <args>"
	// for every successful registration regardless of path.
	logs := logBuf.String()

	// Positive: sid1 took the handoff path.
	wantSid1Handoff := "snapshot: reattached owner " + sid1[:8] + " from handoff"
	if !strings.Contains(logs, wantSid1Handoff) {
		t.Errorf("expected log %q for sid1 (handoff path), not found.\nLogs:\n%s", wantSid1Handoff, logs)
	}

	// Negative: sid2 was NOT taken via handoff path.
	dontWantSid2Handoff := "snapshot: reattached owner " + sid2[:8] + " from handoff"
	if strings.Contains(logs, dontWantSid2Handoff) {
		t.Errorf("did not expect log %q for sid2 — sid2 was absent from handoff payload.\nLogs:\n%s", dontWantSid2Handoff, logs)
	}

	// Both owners must have been registered (positive check for both).
	for _, expected := range []string{
		"snapshot: restored owner " + sid1[:8] + " for echo [one]",
		"snapshot: restored owner " + sid2[:8] + " for echo [two]",
	} {
		if !strings.Contains(logs, expected) {
			t.Errorf("expected log %q, not found.\nLogs:\n%s", expected, logs)
		}
	}

	// Handoff receive must have delivered exactly one upstream.
	if !strings.Contains(logs, "handoff.receive.ok upstreams=1") {
		t.Errorf("expected %q in logs, not found.\nLogs:\n%s", "handoff.receive.ok upstreams=1", logs)
	}
}
