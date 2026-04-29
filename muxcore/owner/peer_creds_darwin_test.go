//go:build darwin
// +build darwin

package owner

import (
	"net"
	"os"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
)

// TestPeerCreds_LoopbackPID_Darwin establishes a local Unix-domain-socket
// loopback in-process and asserts that peerCreds(serverSide) returns the
// peer (client) PID/UID matching this test process. macOS LOCAL_PEERPID
// returns the connecting peer's PID; LOCAL_PEERCRED returns its xucred
// (UID/GID). For an in-process client goroutine that peer == this test
// process.
func TestPeerCreds_LoopbackPID_Darwin(t *testing.T) {
	// macOS sockaddr_un.sun_path limit is 104 bytes — t.TempDir() resolves
	// to /var/folders/<deep>/<test-name>/<n>/ which can exceed the limit.
	// shortSocketPath returns a path under $TMPDIR root that fits.
	sockPath := shortSocketPath(t)
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
	case <-time.After(2 * time.Second):
		t.Fatal("Accept timeout")
	}
	defer srv.Close()

	info := peerCreds(srv)
	if info.Platform != muxcore.PlatformDarwinUnix {
		t.Errorf("Platform: got %q, want %q", info.Platform, muxcore.PlatformDarwinUnix)
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
