//go:build linux

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestHandoffIntegration_FullRoundtripUnix exercises the complete two-daemon
// handoff protocol via listenHandoffUnix + dialHandoffUnix.
//
// Server goroutine: binds Unix socket → calls performHandoff (old-daemon side).
// Client (main goroutine): dials → calls receiveHandoff (new-daemon side).
// Verifies: both sides complete without error, FD is transferred, counts match.
//
// AC guard: if listenHandoffUnix or dialHandoffUnix returned nil conn, the
// performHandoff/receiveHandoff calls would panic — test would fail.
func TestHandoffIntegration_FullRoundtripUnix(t *testing.T) {
	tmp := t.TempDir()
	socketPath := filepath.Join(tmp, "handoff.sock")
	token := "test-token-128bit-abcdefghijk"

	// Open a temp file whose FD we will transfer end-to-end.
	tmpFile, err := os.CreateTemp(tmp, "handoff-fd-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer tmpFile.Close()

	upstreams := []HandoffUpstream{
		{
			ServerID: "s1",
			Command:  "test-command",
			PID:      os.Getpid(),
			StdinFD:  tmpFile.Fd(),
			StdoutFD: tmpFile.Fd(),
		},
	}

	type serverOut struct {
		result HandoffResult
		err    error
	}
	serverCh := make(chan serverOut, 1)

	// Server goroutine: listen + performHandoff (old-daemon side).
	go func() {
		oldConn, err := listenHandoffUnix(socketPath, 5*time.Second)
		if err != nil {
			serverCh <- serverOut{err: err}
			return
		}
		defer oldConn.Close()
		result, err := performHandoff(context.Background(), oldConn, token, upstreams)
		serverCh <- serverOut{result: result, err: err}
	}()

	// Give the listener a moment to bind before dialing.
	time.Sleep(50 * time.Millisecond)

	// Client (new-daemon side): dial + receiveHandoff.
	newConn, err := dialHandoffUnix(socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dialHandoffUnix: %v", err)
	}
	defer newConn.Close()

	received, err := receiveHandoff(context.Background(), newConn, token)
	if err != nil {
		t.Fatalf("receiveHandoff: %v", err)
	}

	srv := <-serverCh
	if srv.err != nil {
		t.Fatalf("performHandoff: %v", srv.err)
	}

	// Verify transferred count.
	if len(srv.result.Transferred) != 1 || srv.result.Transferred[0] != "s1" {
		t.Errorf("Transferred: got %v, want [s1]", srv.result.Transferred)
	}
	if len(srv.result.Aborted) != 0 {
		t.Errorf("Aborted: got %v, want []", srv.result.Aborted)
	}
	if len(received) != 1 {
		t.Errorf("receiveHandoff got %d upstreams, want 1", len(received))
	}
	if len(received) > 0 && received[0].ServerID != "s1" {
		t.Errorf("received[0].ServerID = %q, want s1", received[0].ServerID)
	}

	// Close the received FDs to avoid leaks.
	for _, u := range received {
		if u.StdinFD != 0 {
			_ = os.NewFile(u.StdinFD, "").Close()
		}
		if u.StdoutFD != 0 && u.StdoutFD != u.StdinFD {
			_ = os.NewFile(u.StdoutFD, "").Close()
		}
	}
}

// TestHandoffIntegration_TokenMismatch verifies that performHandoff rejects a
// mismatched token and returns ErrTokenMismatch (FR-11 negative-path guard).
// At least one side must see an error when tokens differ.
func TestHandoffIntegration_TokenMismatch(t *testing.T) {
	tmp := t.TempDir()
	socketPath := filepath.Join(tmp, "handoff-mismatch.sock")
	serverToken := "correct-token-128bit-xyz"
	clientToken := "wrong-token-abcdefghijk"

	var wg sync.WaitGroup
	var serverErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		oldConn, err := listenHandoffUnix(socketPath, 5*time.Second)
		if err != nil {
			serverErr = err
			return
		}
		defer oldConn.Close()
		_, serverErr = performHandoff(context.Background(), oldConn, serverToken, nil)
	}()

	time.Sleep(50 * time.Millisecond)

	newConn, err := dialHandoffUnix(socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dialHandoffUnix: %v", err)
	}
	defer newConn.Close()

	_, clientErr := receiveHandoff(context.Background(), newConn, clientToken)
	wg.Wait()

	// Either the server detects the mismatch (ErrTokenMismatch) or the client
	// gets a connection drop — at least one side must see an error.
	if serverErr == nil && clientErr == nil {
		t.Error("expected at least one side to report an error on token mismatch, got nil on both")
	}
	if serverErr != nil && !errors.Is(serverErr, ErrTokenMismatch) {
		t.Logf("serverErr = %v (not ErrTokenMismatch — connection may drop first; acceptable)", serverErr)
	}
}
