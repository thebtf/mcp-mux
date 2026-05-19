package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func TestRunStopDoesNotRemoveLiveDataSocketWhenControlIsStale(t *testing.T) {
	tmp, err := os.MkdirTemp("", "ms-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMP", tmp)

	id := "stop-race-owner"
	ctlPath := filepath.Join(tmp, ownSocketPrefix+id+".ctl.sock")
	dataPath := filepath.Join(tmp, ownSocketPrefix+id+".sock")

	if err := os.WriteFile(ctlPath, []byte("stale control socket placeholder"), 0o600); err != nil {
		t.Fatalf("write stale control placeholder: %v", err)
	}

	ln, err := ipc.Listen(dataPath)
	if err != nil {
		t.Fatalf("listen data socket: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		for {
			select {
			case conn := <-accepted:
				conn.Close()
			default:
				return
			}
		}
	})

	if !ipc.IsAvailable(dataPath) {
		t.Fatal("precondition: data socket should be available before stop")
	}
	closeAccepted(t, accepted)

	runStop(0, true)

	if !ipc.IsAvailable(dataPath) {
		t.Fatal("live data socket was removed after stale control cleanup")
	}
	closeAccepted(t, accepted)
}

func TestRunStopDoesNotTreatDaemonControlSocketAsOwner(t *testing.T) {
	tmp, err := os.MkdirTemp("", "ms-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMP", tmp)

	daemonCtlPath := filepath.Join(tmp, ownSocketPrefix+"muxd.ctl.sock")
	if err := os.WriteFile(daemonCtlPath, []byte("new daemon control placeholder"), 0o600); err != nil {
		t.Fatalf("write daemon control placeholder: %v", err)
	}

	runStop(0, true)

	if _, err := os.Stat(daemonCtlPath); err != nil {
		t.Fatalf("daemon control socket should be ignored by owner cleanup: %v", err)
	}
}

func closeAccepted(t *testing.T, accepted <-chan net.Conn) {
	t.Helper()
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case conn := <-accepted:
			conn.Close()
		case <-deadline:
			return
		default:
			return
		}
	}
}
