//go:build windows
// +build windows

package owner

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	muxcore "github.com/thebtf/mcp-mux/muxcore"
)

// TestPeerCreds_LoopbackPID_Windows establishes a winio named-pipe loopback
// in-process and asserts that peerCreds(serverSide) returns the peer
// (client) PID matching this test process via
// GetNamedPipeClientProcessId(handle). PeerUid is always 0 on Windows
// (no UID concept comparable to Unix per readPeerUID stub).
func TestPeerCreds_LoopbackPID_Windows(t *testing.T) {
	pipeName := fmt.Sprintf(`\\.\pipe\mcp-mux-test-peercreds-%d-%d`, os.Getpid(), time.Now().UnixNano())

	ln, err := winio.ListenPipe(pipeName, nil)
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		accepted <- c
	}()

	cli, err := winio.DialPipe(pipeName, nil)
	if err != nil {
		t.Fatalf("DialPipe: %v", err)
	}
	defer cli.Close()

	var srv net.Conn
	select {
	case srv = <-accepted:
	case err := <-errCh:
		t.Fatalf("Accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Accept timeout")
	}
	defer srv.Close()

	info := peerCreds(srv)
	if info.Platform != muxcore.PlatformWindowsNamedPipe {
		t.Errorf("Platform: got %q, want %q", info.Platform, muxcore.PlatformWindowsNamedPipe)
	}
	if want := os.Getpid(); info.PeerPid != want {
		t.Errorf("PeerPid: got %d, want %d", info.PeerPid, want)
	}
	// Windows has no Unix-style UID — readPeerUID returns 0 by design.
	if info.PeerUid != 0 {
		t.Errorf("PeerUid on Windows: got %d, want 0", info.PeerUid)
	}
}
