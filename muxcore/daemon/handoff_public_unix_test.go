//go:build unix

package daemon

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPerformHandoff_PublicAPI verifies that PerformHandoff + ReceiveHandoff
// (public API) produce identical behavior to the internal performHandoff /
// receiveHandoff. Setup mirrors TestHandoffIntegration_FullRoundtripUnix but
// calls the public wrappers instead of the private functions.
func TestPerformHandoff_PublicAPI(t *testing.T) {
	tmp := t.TempDir()
	// macOS has a 104-byte unix socket path limit. t.TempDir() on darwin
	// returns a ~90-byte path already. Use os.CreateTemp("", ...) so the
	// socket lives in /tmp/ (short path).
	sockFile, err := os.CreateTemp("", "handoff-pub-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	socketPath := sockFile.Name()
	_ = sockFile.Close()
	_ = os.Remove(socketPath) // listenHandoffUnix creates the socket
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	token := "public-api-test-token-128bit"

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

	// Server goroutine: listen + PerformHandoff (old-daemon side) via PUBLIC API.
	// We use listenHandoffUnix to get an fdConn, then extract the underlying
	// *net.UnixConn to pass to the public API — valid because this is a
	// same-package test.
	go func() {
		oldConn, err := listenHandoffUnix(socketPath, 5*time.Second)
		if err != nil {
			serverCh <- serverOut{err: err}
			return
		}
		defer oldConn.Close()
		// Extract underlying *net.UnixConn to call the public API.
		uc := oldConn.(*unixFDConn).conn
		result, err := PerformHandoff(context.Background(), uc, token, upstreams)
		serverCh <- serverOut{result: result, err: err}
	}()

	// Give the listener a moment to bind before dialing.
	time.Sleep(50 * time.Millisecond)

	// Client (new-daemon side): dial + ReceiveHandoff via PUBLIC API.
	newConn, err := dialHandoffUnix(socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dialHandoffUnix: %v", err)
	}
	defer newConn.Close()

	// Extract underlying *net.UnixConn to call the public API.
	newUC := newConn.(*unixFDConn).conn
	received, err := ReceiveHandoff(context.Background(), newUC, token)
	if err != nil {
		t.Fatalf("ReceiveHandoff: %v", err)
	}

	srv := <-serverCh
	if srv.err != nil {
		t.Fatalf("PerformHandoff: %v", srv.err)
	}

	if len(srv.result.Transferred) != 1 || srv.result.Transferred[0] != "s1" {
		t.Errorf("Transferred: got %v, want [s1]", srv.result.Transferred)
	}
	if len(srv.result.Aborted) != 0 {
		t.Errorf("Aborted: got %v, want []", srv.result.Aborted)
	}
	if len(received) != 1 {
		t.Errorf("ReceiveHandoff got %d upstreams, want 1", len(received))
	}
	if len(received) > 0 && received[0].ServerID != "s1" {
		t.Errorf("received[0].ServerID = %q, want s1", received[0].ServerID)
	}

	// Close received FDs to avoid leaks.
	for _, u := range received {
		if u.StdinFD != 0 {
			_ = os.NewFile(u.StdinFD, "").Close()
		}
		if u.StdoutFD != 0 && u.StdoutFD != u.StdinFD {
			_ = os.NewFile(u.StdoutFD, "").Close()
		}
	}
}
