package owner

import (
	"bytes"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
)

// TestHandleUpstreamMessage_UnknownProgressToken_Dropped is a regression test
// for the netcoredbg-mcp "failed + manual reconnect" symptom.
//
// Before the fix, handleUpstreamMessage for notifications/progress fell through
// to broadcast() whenever routeProgressNotification returned an error (i.e. the
// token was not in progressOwners). Broadcasting a notification with an unknown
// token causes some MCP clients (Claude Code) to log
//   "Received a progress notification for an unknown token"
// and close the stdio transport as erroneous, killing the shim and requiring
// a manual /mcp reconnect.
//
// After the fix, the notification is dropped with a log line; no session
// receives it.
func TestHandleUpstreamMessage_UnknownProgressToken_Dropped(t *testing.T) {
	var logBuf bytes.Buffer
	o := newMinimalOwner()
	o.logger = log.New(&logBuf, "", 0)

	// Create a real session with a captured-write net.Conn so we can detect
	// whether the notification ended up on the wire.
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()

	receivedOnClient := make(chan []byte, 4)
	go func() {
		buf := make([]byte, 4096)
		for {
			_ = clientSide.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, err := clientSide.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				receivedOnClient <- cp
			}
			if err != nil {
				return
			}
		}
	}()

	s := NewSession(serverSide, serverSide)
	s.ID = 1
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()
	defer func() {
		// Drain any stray goroutine by closing the session's transport.
		serverSide.Close()
	}()

	// Notification with a token the owner has never seen.
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"ghost-tok","progress":100,"total":100,"message":"late progress"}}`)
	msg, err := jsonrpc.Parse(raw)
	if err != nil {
		t.Fatalf("jsonrpc.Parse: %v", err)
	}

	if err := o.handleUpstreamMessage(msg); err != nil {
		t.Fatalf("handleUpstreamMessage returned error: %v", err)
	}

	// Give any stray goroutine a moment to attempt a write.
	// The session's drain goroutine runs async; wait briefly.
	select {
	case got := <-receivedOnClient:
		t.Fatalf("unknown-token progress notification was forwarded to the client; "+
			"this would trigger the 'unknown progress token' transport tear-down in MCP clients. got=%q",
			got)
	case <-time.After(200 * time.Millisecond):
		// No data reached the client — correct behaviour.
	}

	// Log line must record the drop with the specific guard message, so
	// operators can see "it was mcp-mux that dropped the notification, not CC
	// losing it".
	logged := logBuf.String()
	if !strings.Contains(logged, "drop notifications/progress") {
		t.Errorf("expected drop log line, got: %q", logged)
	}
	if !strings.Contains(logged, "no owner for progressToken") {
		t.Errorf("expected log to include underlying reason 'no owner for progressToken', got: %q", logged)
	}
	if !strings.Contains(logged, "preventing transport tear-down") {
		t.Errorf("expected log to explain why we drop (CC transport tear-down), got: %q", logged)
	}
}

// TestHandleUpstreamMessage_KnownProgressToken_Forwarded confirms the happy
// path still works after the fix — a notification with a tracked token is
// delivered to the owning session only, not broadcast, not dropped.
func TestHandleUpstreamMessage_KnownProgressToken_Forwarded(t *testing.T) {
	o := newMinimalOwner()

	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()

	s := NewSession(serverSide, serverSide)
	s.ID = 42
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()

	// Track the token so routeProgressNotification finds an owner.
	seedProgressToken(o, s.ID, `"req-1"`, `"known-tok"`)

	receivedOnClient := make(chan []byte, 4)
	var mu sync.Mutex
	var all []byte
	go func() {
		buf := make([]byte, 4096)
		for {
			_ = clientSide.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, err := clientSide.Read(buf)
			if n > 0 {
				mu.Lock()
				all = append(all, buf[:n]...)
				mu.Unlock()
				cp := make([]byte, n)
				copy(cp, buf[:n])
				receivedOnClient <- cp
			}
			if err != nil {
				return
			}
		}
	}()

	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"known-tok","progress":5,"message":"5s elapsed"}}`)
	msg, err := jsonrpc.Parse(raw)
	if err != nil {
		t.Fatalf("jsonrpc.Parse: %v", err)
	}

	if err := o.handleUpstreamMessage(msg); err != nil {
		t.Fatalf("handleUpstreamMessage: %v", err)
	}

	// The owning session should receive the payload.
	select {
	case <-receivedOnClient:
		mu.Lock()
		got := string(all)
		mu.Unlock()
		if !strings.Contains(got, `"progressToken":"known-tok"`) {
			t.Errorf("delivered payload missing progressToken: %q", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout: known-token progress notification was not delivered to the owning session")
	}
}
