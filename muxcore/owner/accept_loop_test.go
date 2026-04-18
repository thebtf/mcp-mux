package owner

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore"
)

type noopSessionHandler struct{}

func (noopSessionHandler) HandleRequest(context.Context, muxcore.ProjectContext, []byte) ([]byte, error) {
	return nil, nil
}

func newTokenHandshakeOwner(t *testing.T, logger *log.Logger) (*Owner, string) {
	t.Helper()

	socketPath := shortSocketPath(t)
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	o, err := NewOwner(OwnerConfig{
		SessionHandler: noopSessionHandler{},
		TokenHandshake: true,
		IPCPath:        socketPath,
		Logger:         logger,
		ServerID:       "accept-loop-test",
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}

	t.Cleanup(func() {
		o.Shutdown()
	})

	return o, socketPath
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func connectWithToken(t *testing.T, socketPath, token string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	_, err = fmt.Fprintf(conn, "%s\n", token)
	if err != nil {
		t.Fatalf("write token: %v", err)
	}
	return conn
}

func sessionCount(o *Owner) int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.sessions)
}

func TestAcceptLoop_RejectEmptyToken(t *testing.T) {
	var logBuffer safeBuffer
	logger := log.New(&logBuffer, "", 0)
	o, socketPath := newTokenHandshakeOwner(t, logger)

	conn := connectWithToken(t, socketPath, "")
	conn.Close()

	// Positive evidence of rejection: the rejection log entry appears. Plain
	// `sessionCount == 0` is true from t=0 and cannot distinguish "rejected"
	// from "acceptLoop hasn't run yet" (CodeRabbit concern).
	waitForCondition(t, 200*time.Millisecond, func() bool {
		return strings.Contains(logBuffer.String(), "accept: rejected connection")
	}, "empty-token connection should produce a rejection log entry")
	if got := sessionCount(o); got != 0 {
		t.Fatalf("sessionCount after reject = %d, want 0", got)
	}
}

func TestAcceptLoop_RejectUnknownToken(t *testing.T) {
	var logBuffer safeBuffer
	logger := log.New(&logBuffer, "", 0)
	o, socketPath := newTokenHandshakeOwner(t, logger)

	conn := connectWithToken(t, socketPath, "cafebabe")
	conn.Close()

	waitForCondition(t, 200*time.Millisecond, func() bool {
		return strings.Contains(logBuffer.String(), "accept: rejected connection")
	}, "unknown-token connection should produce a rejection log entry")
	if got := sessionCount(o); got != 0 {
		t.Fatalf("sessionCount after reject = %d, want 0", got)
	}
}

func TestAcceptLoop_AcceptPreRegisteredToken(t *testing.T) {
	var logBuffer safeBuffer
	logger := log.New(&logBuffer, "", 0)
	o, socketPath := newTokenHandshakeOwner(t, logger)
	o.SessionMgr().PreRegister("feedface", "/workspace/project", nil)

	conn := connectWithToken(t, socketPath, "feedface")
	defer conn.Close()

	waitForCondition(t, 200*time.Millisecond, func() bool {
		return sessionCount(o) == 1
	}, "pre-registered token should be accepted")

	if strings.Contains(logBuffer.String(), "accept: rejected connection") {
		t.Fatalf("unexpected rejection for pre-registered token: %q", logBuffer.String())
	}
}

func TestAcceptLoop_ConcurrentTokenMix(t *testing.T) {
	var logBuffer safeBuffer
	logger := log.New(&logBuffer, "", 0)
	o, socketPath := newTokenHandshakeOwner(t, logger)

	const n = 10
	validTokens := make([]string, n)
	for i := 0; i < n; i++ {
		token := fmt.Sprintf("%08x", i+1)
		validTokens[i] = token
		o.SessionMgr().PreRegister(token, "/workspace", nil)
	}

	conns := make([]net.Conn, n*2)
	var connMu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(token string, idx int) {
			defer wg.Done()
			conn := connectWithToken(t, socketPath, token)
			connMu.Lock()
			conns[idx] = conn
			connMu.Unlock()
		}(validTokens[i], i)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn := connectWithToken(t, socketPath, fmt.Sprintf("bad%04x", i))
			connMu.Lock()
			conns[n+i] = conn
			connMu.Unlock()
		}(i)
	}
	wg.Wait()
	for _, conn := range conns[n:] {
		conn.Close()
	}
	t.Cleanup(func() {
		for _, conn := range conns[:n] {
			if conn != nil {
				conn.Close()
			}
		}
	})

	waitForCondition(t, 200*time.Millisecond, func() bool {
		return sessionCount(o) == n
	}, "10 valid connections should be accepted")

	rejections := strings.Count(logBuffer.String(), "accept: rejected connection")
	if rejections != n {
		t.Fatalf("reject log entries: got %d, want %d; logs: %q", rejections, n, logBuffer.String())
	}
}
