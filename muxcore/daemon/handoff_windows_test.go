//go:build windows

package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"
)

func randomPipeName(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "test-" + hex.EncodeToString(b[:])
}

// TestHandoffPipe_ListenDialRoundtrip verifies that a pipe listener and
// dialer created by listenHandoffPipe / dialHandoffPipe can exchange a
// newline-delimited JSON message in both directions.
func TestHandoffPipe_ListenDialRoundtrip(t *testing.T) {
	name := randomPipeName(t)
	ln, err := listenHandoffPipe(name)
	if err != nil {
		t.Fatalf("listenHandoffPipe: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		defer conn.Close()
		server := newWindowsFDConn(conn)
		// Read request.
		var req map[string]string
		if err := server.ReadJSON(&req); err != nil {
			t.Errorf("server ReadJSON: %v", err)
			return
		}
		if req["ping"] != "yes" {
			t.Errorf("unexpected req: %v", req)
			return
		}
		// Send response.
		if err := server.WriteJSON(map[string]string{"pong": "ok"}); err != nil {
			t.Errorf("server WriteJSON: %v", err)
		}
	}()

	conn, err := dialHandoffPipe(name, 3*time.Second)
	if err != nil {
		t.Fatalf("dialHandoffPipe: %v", err)
	}
	defer conn.Close()
	client := newWindowsFDConn(conn)
	if err := client.WriteJSON(map[string]string{"ping": "yes"}); err != nil {
		t.Fatalf("client WriteJSON: %v", err)
	}
	var resp map[string]string
	if err := client.ReadJSON(&resp); err != nil {
		t.Fatalf("client ReadJSON: %v", err)
	}
	if resp["pong"] != "ok" {
		t.Errorf("unexpected resp: %v", resp)
	}
	wg.Wait()
}

// TestHandoffPipe_DialMissingListener verifies that dial fails cleanly when
// no listener exists for the given name (not a hang).
func TestHandoffPipe_DialMissingListener(t *testing.T) {
	name := randomPipeName(t)
	_, err := dialHandoffPipe(name, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when no listener exists")
	}
}

// TestWindowsFDConn_SendRecvHandles verifies DuplicateHandle transfer within
// a single process (self-dup — targetPID = our own pid). Creates a temp file,
// sends its handle over the pipe, receiver reads the file content via the
// duplicated handle.
//
// This test is the canonical proof that DuplicateHandle "push" transfer works:
// sender calls SetTargetPID(os.Getpid()), duplicates the file handle into
// our own process, and the receiver reads it back through os.NewFile.
func TestWindowsFDConn_SendRecvHandles(t *testing.T) {
	name := randomPipeName(t)
	ln, err := listenHandoffPipe(name)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	tmp, err := os.CreateTemp("", "wfd-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(tmp.Name())
		_ = tmp.Close()
	})
	content := []byte("dup-handle-roundtrip")
	if _, err := tmp.Write(content); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		t.Fatal(err)
	}

	serverDone := make(chan error, 1)
	var recvFDs []uintptr
	var recvHeader []byte
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		srv := newWindowsFDConn(conn)
		defer srv.Close()
		fds, hdr, err := srv.RecvFDs()
		recvFDs = fds
		recvHeader = hdr
		serverDone <- err
	}()

	conn, err := dialHandoffPipe(name, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cli := newWindowsFDConn(conn)
	cli.SetTargetPID(os.Getpid()) // self-dup: target is our own process
	defer cli.Close()

	err = cli.SendFDs([]uintptr{tmp.Fd()}, []byte("header"))
	if err != nil {
		t.Fatalf("SendFDs: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
	if len(recvFDs) != 1 {
		t.Fatalf("expected 1 fd, got %d", len(recvFDs))
	}
	if string(recvHeader) != "header" {
		t.Errorf("header: %q", recvHeader)
	}

	// Verify the duplicated handle is usable: wrap it in os.File and read.
	dupFile := os.NewFile(recvFDs[0], "dup")
	defer dupFile.Close()
	if _, err := dupFile.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(content))
	if _, err := dupFile.Read(got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("dup content %q != source %q", got, content)
	}
}

// TestWindowsFDConn_SendFDs_NoTargetPID verifies that SendFDs returns an error
// when SetTargetPID has not been called. Covers the guard path that prevents
// a nil-PID DuplicateHandle call from producing an invalid handle.
func TestWindowsFDConn_SendFDs_NoTargetPID(t *testing.T) {
	name := randomPipeName(t)
	ln, err := listenHandoffPipe(name)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			time.Sleep(50 * time.Millisecond)
			_ = c.Close()
		}
	}()

	conn, err := dialHandoffPipe(name, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cli := newWindowsFDConn(conn)
	defer cli.Close()
	// targetPID is zero (SetTargetPID not called) — must return error immediately.
	err = cli.SendFDs([]uintptr{0}, []byte("h"))
	if err == nil {
		t.Fatal("expected error when targetPID not set")
	}
}
