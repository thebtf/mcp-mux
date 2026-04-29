//go:build linux
// +build linux

package owner

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
)

// TestPeerCreds_LoopbackPID_Linux establishes a local Unix-domain-socket
// loopback in-process and asserts that peerCreds(serverSide) returns the
// peer (client) PID/UID matching this test process. SO_PEERCRED on Linux
// returns the credentials of the connecting peer; for an in-process client
// goroutine that peer == this test process.
func TestPeerCreds_LoopbackPID_Linux(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "peercreds.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen unix: %v", err)
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

	cli, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("Dial unix: %v", err)
	}
	defer cli.Close()

	var srv net.Conn
	select {
	case srv = <-accepted:
	case err := <-errCh:
		t.Fatalf("Accept: %v", err)
	}
	defer srv.Close()

	info := peerCreds(srv)
	if info.Platform != muxcore.PlatformLinuxUnix {
		t.Errorf("Platform: got %q, want %q", info.Platform, muxcore.PlatformLinuxUnix)
	}
	if want := os.Getpid(); info.PeerPid != want {
		t.Errorf("PeerPid: got %d, want %d", info.PeerPid, want)
	}
	// Skip UID assertion when running as root (UID 0 is indistinguishable
	// from the "0 == unavailable" sentinel; CI typically runs non-root).
	if uid := os.Getuid(); uid != 0 && info.PeerUid != uid {
		t.Errorf("PeerUid: got %d, want %d", info.PeerUid, uid)
	}
}
