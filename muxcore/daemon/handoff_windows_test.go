//go:build windows

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
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

// TestHandoffPipe_DialBeforeDelayedBind proves the client can start before the
// successor has bound its pipe. The first real dial must observe FILE_NOT_FOUND;
// only then does the test bind the listener and permit the retry.
func TestHandoffPipe_DialBeforeDelayedBind(t *testing.T) {
	name := randomPipeName(t)
	firstAttempt := make(chan error, 1)
	allowRetry := make(chan struct{})
	var releaseOnce sync.Once
	releaseRetry := func() {
		releaseOnce.Do(func() { close(allowRetry) })
	}
	defer releaseRetry()

	attempts := 0
	dial := func(ctx context.Context, path string) (net.Conn, error) {
		conn, err := winio.DialPipeContext(ctx, path)
		attempts++
		if attempts == 1 {
			firstAttempt <- err
			<-allowRetry
		}
		return conn, err
	}
	type dialResult struct {
		conn net.Conn
		err  error
	}
	dialDone := make(chan dialResult, 1)
	go func() {
		conn, err := dialHandoffPipeWith(name, 2*time.Second, dial)
		dialDone <- dialResult{conn: conn, err: err}
	}()

	select {
	case err := <-firstAttempt:
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatalf("first dial error = %v, want ERROR_FILE_NOT_FOUND", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first dial attempt did not complete")
	}

	ln, err := listenHandoffPipe(name)
	if err != nil {
		t.Fatalf("listenHandoffPipe: %v", err)
	}
	defer ln.Close()
	acceptDone := make(chan dialResult, 1)
	go func() {
		conn, err := ln.Accept()
		acceptDone <- dialResult{conn: conn, err: err}
	}()
	releaseRetry()

	var client dialResult
	select {
	case client = <-dialDone:
		if client.err != nil {
			t.Fatalf("dial after delayed bind: %v", client.err)
		}
		defer client.conn.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("dial did not complete after delayed bind")
	}
	select {
	case server := <-acceptDone:
		if server.err != nil {
			t.Fatalf("accept after delayed bind: %v", server.err)
		}
		server.conn.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("accept did not complete after delayed bind")
	}
	if attempts < 2 {
		t.Fatalf("dial attempts = %d, want at least 2", attempts)
	}
}

func TestHandoffPipe_DialRetriesPipeBusy(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	attempts := 0
	conn, err := dialHandoffPipeWith(randomPipeName(t), time.Second, func(context.Context, string) (net.Conn, error) {
		attempts++
		if attempts == 1 {
			return nil, windows.ERROR_PIPE_BUSY
		}
		return client, nil
	})
	if err != nil {
		t.Fatalf("dial after ERROR_PIPE_BUSY: %v", err)
	}
	defer conn.Close()
	if attempts != 2 {
		t.Fatalf("dial attempts = %d, want 2", attempts)
	}
}

func TestHandoffPipe_DialPermanentErrorFailsFast(t *testing.T) {
	attempts := 0
	_, err := dialHandoffPipeWith(randomPipeName(t), 100*time.Millisecond, func(context.Context, string) (net.Conn, error) {
		attempts++
		return nil, windows.ERROR_ACCESS_DENIED
	})
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("dial error = %v, want ERROR_ACCESS_DENIED", err)
	}
	if attempts != 1 {
		t.Fatalf("dial attempts = %d, want 1", attempts)
	}
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
	conn, peer := net.Pipe()
	defer peer.Close()
	cli := newWindowsFDConn(conn)
	defer cli.Close()
	tmp, err := os.CreateTemp("", "handoff-no-target-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	// targetPID is zero (SetTargetPID not called) — must return error immediately.
	err = cli.SendFDs([]uintptr{tmp.Fd()}, []byte("h"))
	if err == nil {
		t.Fatal("expected error when targetPID not set")
	}
	if !strings.Contains(err.Error(), "targetPID not set") {
		t.Fatalf("error = %v, want targetPID guard", err)
	}
}

func TestListenDialHandoffWindows_Factory(t *testing.T) {
	var nameBuf [8]byte
	_, _ = rand.Read(nameBuf[:])
	pipeName := "fac-" + hex.EncodeToString(nameBuf[:])

	// Bind the pipe listener synchronously so it is ready before the client dials.
	// listenHandoffPipe returns once the named pipe is bound — the client can dial
	// immediately after without any sleep-based synchronization.
	ln, err := listenHandoffPipe(pipeName)
	if err != nil {
		t.Fatalf("listenHandoffPipe: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	var serverErr error
	go func() {
		defer wg.Done()
		rawConn, err := ln.Accept()
		if err != nil {
			serverErr = err
			return
		}
		conn := newWindowsFDConn(rawConn)
		defer conn.Close()
		var msg map[string]string
		serverErr = conn.ReadJSON(&msg)
	}()

	// Pipe is already bound — dial immediately, no sleep needed.
	client, err := dialHandoffWindows(pipeName, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if err := client.WriteJSON(map[string]string{"ping": "yes"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	wg.Wait()
	if serverErr != nil {
		t.Fatalf("server: %v", serverErr)
	}
}

func TestListenHandoffWindows_AcceptTimeout(t *testing.T) {
	var nameBuf [8]byte
	_, _ = rand.Read(nameBuf[:])
	pipeName := "timeout-" + hex.EncodeToString(nameBuf[:])

	start := time.Now()
	_, err := listenHandoffWindows(pipeName, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 1*time.Second {
		t.Errorf("timeout took too long: %s", elapsed)
	}
}
