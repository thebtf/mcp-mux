package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
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

	messages := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				buf := make([]byte, 512)
				n, _ := c.Read(buf)
				if n > 0 {
					select {
					case messages <- string(buf[:n]):
					default:
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
	})

	if !ipc.IsAvailable(dataPath) {
		t.Fatal("precondition: data socket should be available before stop")
	}

	runStop(0, true)

	if !ipc.IsAvailable(dataPath) {
		t.Fatal("live data socket was removed after stale control cleanup")
	}
	select {
	case msg := <-messages:
		if !strings.Contains(msg, `"method":"mux/shutdown"`) {
			t.Fatalf("expected legacy shutdown after stale control, got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected legacy shutdown after stale control with live data socket")
	}
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
