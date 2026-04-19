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
		_, _ = performHandoff(ctx, oldDaemonConn, testToken, upstreams)
	}()

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
				ServerID:    sid,
				Command:     "echo",
				Args:        []string{"fallback"},
				Cwd:         t.TempDir(),
				Mode:        "global",
				CachedInit:  "e30=",
				CachedTools: "e30=",
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
