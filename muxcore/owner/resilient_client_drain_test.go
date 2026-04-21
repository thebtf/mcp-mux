package owner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// startSlowEchoIPCServer starts an IPC echo server whose response for each
// request is blocked until onRequest returns. This lets tests close stdin
// while a response is still pending, validating the drain window.
//
// The server handles "initialize" normally (no blocking). For all other
// requests with an id, it calls onRequest(conn, rawID) — which may block —
// then writes the response.
func startSlowEchoIPCServer(t *testing.T, path string, onRequest func(conn net.Conn, rawID string)) *echoServer {
	t.Helper()
	received := make(chan string, 100)

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("slow-echo listen %s: %v", path, err)
	}

	srv := &echoServer{ln: ln, received: received}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.mu.Lock()
			srv.conns = append(srv.conns, conn)
			srv.mu.Unlock()
			go handleSlowEchoConn(conn, received, onRequest)
		}
	}()

	t.Cleanup(func() {
		srv.closeAll()
		os.Remove(path)
	})

	return srv
}

// handleSlowEchoConn handles one slow-echo IPC connection.
// "initialize" is answered immediately. All other requests call onRequest
// before sending the response (allowing the test to gate delivery).
func handleSlowEchoConn(conn net.Conn, received chan string, onRequest func(conn net.Conn, rawID string)) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		select {
		case received <- line:
		default:
		}

		var msg map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		idRaw, hasID := msg["id"]
		if !hasID {
			continue // notification
		}

		methodRaw := msg["method"]
		method := strings.Trim(string(methodRaw), `"`)

		switch method {
		case "initialize":
			resp := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"slow-test","version":"1.0"},"capabilities":{}}}`,
				string(idRaw),
			)
			if _, err := fmt.Fprintf(conn, "%s\n", resp); err != nil {
				return
			}
		default:
			// Block until the test releases the response.
			if onRequest != nil {
				onRequest(conn, string(idRaw))
			}
			resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, string(idRaw))
			if _, err := fmt.Fprintf(conn, "%s\n", resp); err != nil {
				return
			}
		}
	}
}

// TestStdinEOF_DrainPending verifies that when stdin closes with in-flight
// requests, the shim waits for IPC responses before exiting (no data loss).
func TestStdinEOF_DrainPending(t *testing.T) {
	ipcPath := newTestIPCPath(t)

	// Gate channel: closed by the test to release the pending tools/call response.
	releaseCh := make(chan struct{})

	// Track whether the slow server has received and is blocking the tools/call.
	serverBlockedCh := make(chan struct{}, 1)

	startSlowEchoIPCServer(t, ipcPath, func(conn net.Conn, rawID string) {
		select {
		case serverBlockedCh <- struct{}{}:
		default:
		}
		<-releaseCh
	})

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond, // disable probe detection
		Stdin:            ccStdinR,
		Stdout:           ccStdoutW,
		InitialIPCPath:   ipcPath,
		ReconnectTimeout: 5 * time.Second,
		Logger:           resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(cfg)
	}()

	// toolsRespSeen is signaled when the tools/call response appears on CC stdout.
	// We wait for it BEFORE closing ccStdoutW to avoid the write-after-close race:
	// runStdoutWriter goroutine outlives the shim and writes the buffered response
	// after the drain loop unblocks; closing ccStdoutW too early would drop it.
	toolsRespSeen := make(chan struct{}, 1)
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(ccStdoutR)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			// Signal when we see the tools/call response (id=2, has "result").
			if strings.Contains(line, `"id":2`) && strings.Contains(line, `"result"`) {
				select {
				case toolsRespSeen <- struct{}{}:
				default:
				}
			}
		}
	}()

	// Step 1: send initialize so ipcMsgSent fires (disables probe path).
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	fmt.Fprintf(ccStdinW, "%s\n", initReq)
	time.Sleep(300 * time.Millisecond) // let initialize round-trip complete

	// Step 2: send tools/call — the slow server will block before responding.
	toolsReq := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"test"}}`
	fmt.Fprintf(ccStdinW, "%s\n", toolsReq)

	// Wait for the slow server to receive and block on tools/call.
	select {
	case <-serverBlockedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: slow server did not receive tools/call")
	}

	// Step 3: close stdin while the tools/call response is still pending.
	ccStdinW.Close()

	// Give the shim 100ms to enter drain mode.
	time.Sleep(100 * time.Millisecond)

	// Shim must NOT have exited yet (it's draining).
	select {
	case err := <-errCh:
		t.Fatalf("shim exited before drain complete: %v", err)
	default:
		// Good — still running.
	}

	// Step 4: release the pending response.
	close(releaseCh)

	// Step 5: verify ORDERING — shim must NOT exit before the response is delivered.
	// Race both channels: toolsRespSeen should fire first (or simultaneously).
	// If errCh fires before toolsRespSeen, the drain exited too early.
	shimExitedFirst := false
	responseDelivered := false
	for !responseDelivered {
		select {
		case <-toolsRespSeen:
			t.Logf("tools/call response delivered to CC stdout")
			responseDelivered = true
		case err := <-errCh:
			if !responseDelivered {
				shimExitedFirst = true
				t.Errorf("ORDERING VIOLATION: shim exited (err=%v) BEFORE tools/call response was delivered to stdout", err)
			}
			// Continue to wait for response even if shim exited (stdout writer goroutine may still deliver)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout: tools/call response not seen on CC stdout after releasing drain")
		}
	}

	// Step 6: shim exits cleanly after response delivery.
	if !shimExitedFirst {
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("shim exited with unexpected error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout: shim did not exit after drain complete")
		}
	}

	// Close the stdout pipe to unblock the scanner goroutine.
	ccStdoutW.Close()
	<-stdoutDone
}

// TestStdinEOF_WaitForDisconnect verifies that with StdinEOFWaitForDisconnect
// policy, stdin EOF does not cause the shim to exit — it stays alive until
// the IPC connection closes.
func TestStdinEOF_WaitForDisconnect(t *testing.T) {
	ipcPath := newTestIPCPath(t)
	srv, _ := startEchoIPCServer(t, ipcPath)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond, // disable probe detection
		StdinEOFPolicy:   StdinEOFWaitForDisconnect,
		Stdin:            ccStdinR,
		Stdout:           ccStdoutW,
		InitialIPCPath:   ipcPath,
		ReconnectTimeout: 2 * time.Second,
		Logger:           resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(cfg)
	}()

	// Drain CC stdout.
	go func() {
		io.Copy(io.Discard, ccStdoutR)
	}()

	// Step 1: send initialize and close stdin.
	fmt.Fprintf(ccStdinW, "%s\n", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	time.Sleep(100 * time.Millisecond)
	ccStdinW.Close()

	// Step 2: shim must stay alive for at least 500ms after stdin close.
	select {
	case err := <-errCh:
		t.Fatalf("shim exited too early after stdin EOF with WaitForDisconnect policy: %v", err)
	case <-time.After(500 * time.Millisecond):
		// Good — still running.
	}

	// Step 3: close IPC server → shim loses IPC connection and cannot reconnect
	// (no Reconnect func configured), so it exits with a reconnect error or via
	// awaitReconnectAttempt returning "reconnect function not configured".
	srv.closeAll()

	select {
	case err := <-errCh:
		// Any exit is acceptable — IPC disconnect triggered the exit.
		t.Logf("shim exited after IPC close: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: shim did not exit after IPC disconnect")
	}
}

// TestStdinEOF_NoPending_ExitsImmediately verifies that with no in-flight
// requests, stdin EOF causes the shim to exit within 200ms (no drain delay).
func TestStdinEOF_NoPending_ExitsImmediately(t *testing.T) {
	ipcPath := newTestIPCPath(t)
	_, _ = startEchoIPCServer(t, ipcPath)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()

	// Channel to detect when the initialize response is forwarded to stdout.
	initRespSeen := make(chan struct{}, 1)

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond, // disable probe detection
		Stdin:            ccStdinR,
		Stdout:           ccStdoutW,
		InitialIPCPath:   ipcPath,
		ReconnectTimeout: 5 * time.Second,
		Logger:           resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(cfg)
	}()

	// Monitor stdout for the initialize response so we know the round-trip is done.
	go func() {
		scanner := bufio.NewScanner(ccStdoutR)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, `"id":1`) && strings.Contains(line, `"result"`) {
				select {
				case initRespSeen <- struct{}{}:
				default:
				}
			}
		}
	}()

	// Step 1: send initialize.
	fmt.Fprintf(ccStdinW, "%s\n", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	// Wait for initialize response to confirm inflight map is empty.
	select {
	case <-initRespSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: initialize response not seen on stdout")
	}

	// Small extra wait to ensure the inflight Delete races resolve.
	time.Sleep(50 * time.Millisecond)

	// Step 2: close stdin — no in-flight requests at this point.
	start := time.Now()
	ccStdinW.Close()

	// Step 3: shim must exit within 200ms (no drain delay).
	select {
	case err := <-errCh:
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("expected nil on clean stdin close, got: %v", err)
		}
		if elapsed > 200*time.Millisecond {
			t.Errorf("shim took %v to exit with no pending requests (want < 200ms)", elapsed)
		}
		t.Logf("shim exited in %v (no pending requests)", elapsed)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: shim did not exit within 2s after stdin close with no pending requests")
	}

	ccStdoutW.Close()
}
