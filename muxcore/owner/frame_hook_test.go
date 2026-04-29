package owner

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
)

// counterHandler increments a counter on every HandleRequest. Returns a
// fixed JSON-RPC response so the client's reader does not block.
type counterHandler struct {
	calls atomic.Int32
}

func (h *counterHandler) HandleRequest(_ context.Context, _ muxcore.ProjectContext, _ []byte) ([]byte, error) {
	h.calls.Add(1)
	return []byte(`{"jsonrpc":"2.0","id":"1","result":{}}`), nil
}

// frameHookOwner builds a TokenHandshake owner with the supplied hook + handler.
func frameHookOwner(
	t *testing.T,
	hook func(sessionID string, frameSize int, method string) muxcore.FrameAction,
	handler muxcore.SessionHandler,
) (*Owner, string, *safeBuffer) {
	t.Helper()
	if handler == nil {
		handler = &counterHandler{}
	}
	socketPath := shortSocketPath(t)
	logBuf := &safeBuffer{}
	logger := log.New(logBuf, "", 0)
	o, err := NewOwner(OwnerConfig{
		SessionHandler:  handler,
		TokenHandshake:  true,
		IPCPath:         socketPath,
		OnFrameReceived: hook,
		Logger:          logger,
		ServerID:        "frame-hook-test",
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	t.Cleanup(func() { o.Shutdown() })
	return o, socketPath, logBuf
}

// dialAndHandshake registers a hex token, opens a conn, and returns it.
// Caller must Close the conn when done.
func dialAndHandshake(t *testing.T, o *Owner, socketPath, token string) net.Conn {
	t.Helper()
	o.SessionMgr().PreRegister(token, "/proj", nil)
	conn := connectWithToken(t, socketPath, token)
	waitForCondition(t, 500*time.Millisecond, func() bool {
		return sessionCount(o) >= 1
	}, "session never added (handshake failed)")
	return conn
}

// drainResponses reads from conn until the deadline elapses, returning all
// non-empty lines observed. Used by FramePass / FrameError tests where the
// client expects responses (or an absence thereof for Drop).
func drainResponses(conn net.Conn, deadline time.Duration) []string {
	_ = conn.SetReadDeadline(time.Now().Add(deadline))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	var out []string
	buf := make([]byte, 4096)
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		n, err := conn.Read(buf)
		if n > 0 {
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if s := strings.TrimSpace(line); s != "" {
					out = append(out, s)
				}
			}
		}
		if err != nil {
			break
		}
	}
	return out
}

// sendN writes N JSON-RPC requests as fast as possible.
func sendN(t *testing.T, conn net.Conn, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":"%d","method":"tools/list"}`, i+1)
		if _, err := fmt.Fprintf(conn, "%s\n", req); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}
	}
}

// TestFrameHook_PassDispatchesNormally (T029).
func TestFrameHook_PassDispatchesNormally(t *testing.T) {
	var hookCalls atomic.Int32
	hook := func(_ string, _ int, _ string) muxcore.FrameAction {
		hookCalls.Add(1)
		return muxcore.FramePass
	}
	handler := &counterHandler{}
	o, socketPath, _ := frameHookOwner(t, hook, handler)

	conn := dialAndHandshake(t, o, socketPath, "11111111")
	defer conn.Close()

	sendN(t, conn, 5)

	waitForCondition(t, 1*time.Second, func() bool {
		return handler.calls.Load() == 5
	}, "expected 5 dispatches under FramePass")

	if hookCalls.Load() != 5 {
		t.Errorf("hook invocations: got %d, want 5", hookCalls.Load())
	}
}

// TestFrameHook_DropSilentDiscard (T030).
func TestFrameHook_DropSilentDiscard(t *testing.T) {
	var hookCalls atomic.Int32
	hook := func(_ string, _ int, _ string) muxcore.FrameAction {
		hookCalls.Add(1)
		return muxcore.FrameDrop
	}
	handler := &counterHandler{}
	o, socketPath, _ := frameHookOwner(t, hook, handler)

	conn := dialAndHandshake(t, o, socketPath, "22222222")
	defer conn.Close()

	sendN(t, conn, 10)

	// Hook must observe all 10; handler must dispatch none.
	waitForCondition(t, 1*time.Second, func() bool {
		return hookCalls.Load() == 10
	}, "hook should have observed all 10 frames")

	time.Sleep(100 * time.Millisecond) // let any spurious dispatch land
	if got := handler.calls.Load(); got != 0 {
		t.Errorf("dispatch count under FrameDrop: got %d, want 0", got)
	}

	// Client must NOT see a -32004 error frame for any of the dropped requests.
	responses := drainResponses(conn, 200*time.Millisecond)
	for _, line := range responses {
		if strings.Contains(line, "-32004") {
			t.Errorf("FrameDrop produced -32004 error to client: %q", line)
		}
	}
}

// TestFrameHook_ErrorRespondsWithJSONRPC (T031).
func TestFrameHook_ErrorRespondsWithJSONRPC(t *testing.T) {
	hook := func(_ string, _ int, _ string) muxcore.FrameAction {
		return muxcore.FrameError
	}
	handler := &counterHandler{}
	o, socketPath, _ := frameHookOwner(t, hook, handler)

	conn := dialAndHandshake(t, o, socketPath, "33333333")
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "%s\n", `{"jsonrpc":"2.0","id":42,"method":"tools/list"}`); err != nil {
		t.Fatalf("write request: %v", err)
	}

	responses := drainResponses(conn, 500*time.Millisecond)
	if len(responses) == 0 {
		t.Fatal("expected -32004 response, got nothing")
	}
	got := responses[0]
	if !strings.Contains(got, `"code":-32004`) {
		t.Errorf("missing -32004 code: %q", got)
	}
	if !strings.Contains(got, "rate limited") {
		t.Errorf("missing 'rate limited' message: %q", got)
	}
	if !strings.Contains(got, `"id":42`) {
		t.Errorf("msg.ID not preserved (want 42): %q", got)
	}
	if got := handler.calls.Load(); got != 0 {
		t.Errorf("FrameError dispatched (got %d), want 0", got)
	}
}

// TestFrameHook_TimeoutGracefulDegradation (T032). Slow callback returns
// FrameDrop after 5 ms; the 1 ms budget converts the verdict to FramePass
// and dispatch proceeds.
func TestFrameHook_TimeoutGracefulDegradation(t *testing.T) {
	hook := func(_ string, _ int, _ string) muxcore.FrameAction {
		time.Sleep(5 * time.Millisecond)
		return muxcore.FrameDrop // ignored — timer fires first
	}
	handler := &counterHandler{}
	o, socketPath, logBuf := frameHookOwner(t, hook, handler)

	conn := dialAndHandshake(t, o, socketPath, "44444444")
	defer conn.Close()

	sendN(t, conn, 1)

	waitForCondition(t, 2*time.Second, func() bool {
		return handler.calls.Load() == 1
	}, "slow callback should have been treated as FramePass + dispatched")
	waitForCondition(t, 500*time.Millisecond, func() bool {
		return strings.Contains(logBuf.String(), "frame_hook_timeout sid=")
	}, "frame_hook_timeout marker missing")
}

// TestFrameHook_PanicTreatedAsPass (T033). Reader goroutine survives.
func TestFrameHook_PanicTreatedAsPass(t *testing.T) {
	var panicMode atomic.Bool
	panicMode.Store(true)
	hook := func(_ string, _ int, _ string) muxcore.FrameAction {
		if panicMode.Load() {
			panic("test-panic")
		}
		return muxcore.FramePass
	}
	handler := &counterHandler{}
	o, socketPath, logBuf := frameHookOwner(t, hook, handler)

	conn := dialAndHandshake(t, o, socketPath, "55555555")
	defer conn.Close()

	sendN(t, conn, 1)
	waitForCondition(t, 1*time.Second, func() bool {
		return handler.calls.Load() == 1
	}, "panicking callback should still dispatch (FramePass on panic)")
	waitForCondition(t, 500*time.Millisecond, func() bool {
		return strings.Contains(logBuf.String(), "frame_hook_panic sid=")
	}, "frame_hook_panic marker missing")

	// Subsequent frames after panic mode disabled must still dispatch —
	// the reader goroutine survived.
	panicMode.Store(false)
	sendN(t, conn, 1)
	waitForCondition(t, 1*time.Second, func() bool {
		return handler.calls.Load() == 2
	}, "reader goroutine did not survive panic")
}

// TestFrameHook_NilUnchangedDispatch (T034). nil callback path must produce
// zero frame_hook_* log markers — byte-identical to v0.23.
func TestFrameHook_NilUnchangedDispatch(t *testing.T) {
	handler := &counterHandler{}
	o, socketPath, logBuf := frameHookOwner(t, nil, handler)

	conn := dialAndHandshake(t, o, socketPath, "66666666")
	defer conn.Close()

	sendN(t, conn, 100)
	waitForCondition(t, 3*time.Second, func() bool {
		return handler.calls.Load() == 100
	}, "nil hook should dispatch all 100 frames")

	logs := logBuf.String()
	for _, marker := range []string{"frame_hook_pass", "frame_hook_drop", "frame_hook_error", "frame_hook_timeout", "frame_hook_panic"} {
		if strings.Contains(logs, marker) {
			t.Errorf("nil OnFrameReceived produced %q marker — should be byte-identical to v0.23", marker)
		}
	}
}

// TestFrameHook_OutboundFramesNotIntercepted (T036 / CHK014). Hook counter
// must equal inbound-frame count exactly — outbound responses written by
// SessionHandler MUST NOT enter the hook path.
func TestFrameHook_OutboundFramesNotIntercepted(t *testing.T) {
	var hookCalls atomic.Int32
	hook := func(_ string, _ int, _ string) muxcore.FrameAction {
		hookCalls.Add(1)
		return muxcore.FramePass
	}
	handler := &counterHandler{}
	o, socketPath, _ := frameHookOwner(t, hook, handler)

	conn := dialAndHandshake(t, o, socketPath, "77777777")
	defer conn.Close()

	const inbound = 5
	sendN(t, conn, inbound)
	waitForCondition(t, 1*time.Second, func() bool {
		return handler.calls.Load() == inbound
	}, "all inbound dispatches expected")

	// Hook count must equal inbound — handler's responses are server→client
	// frames and never re-enter handleDownstreamMessage.
	time.Sleep(100 * time.Millisecond) // settle window for any spurious increments
	if got := hookCalls.Load(); got != int32(inbound) {
		t.Errorf("hook invocations: got %d, want %d (only inbound counted)", got, inbound)
	}
}

// TestFrameHook_OverheadP99 (T035 verification path). Direct-call path on
// a synthetic session/message — measures real overhead of invokeFrameHook
// without the IPC + tokenHandshake + accept loop noise.
//
// The fail-open threshold is set to 2x frameHookBudget (2 ms) — the test
// guards against catastrophic overhead regressions, not absolute throughput.
// CI runners (especially Windows under coverage) routinely exhibit p99 in
// the 500-1500 µs range for goroutine-spawn + select-on-channel paths
// under contention; the BenchmarkFrameHook_NoopOverhead reference is the
// authoritative per-call number (≈ 800 ns on dev hardware) — this test
// is the upper-bound smoke gate.
func TestFrameHook_OverheadP99(t *testing.T) {
	if testing.Short() {
		t.Skip("overhead probe runs in non-short mode")
	}
	o := &Owner{
		onFrameReceived: func(_ string, _ int, _ string) muxcore.FrameAction {
			return muxcore.FramePass
		},
		logger: log.New(io.Discard, "", 0),
	}
	s := NewSession(strings.NewReader(""), io.Discard)
	defer s.Close()
	msg, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":"1","method":"tools/list"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	const iterations = 1000
	timings := make([]time.Duration, iterations)
	for i := range timings {
		start := time.Now()
		_ = o.invokeFrameHook(s, msg)
		timings[i] = time.Since(start)
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	p99 := timings[int(float64(iterations)*0.99)]

	const upperBound = 2 * frameHookBudget // 2 ms — guards against catastrophic regressions only
	if p99 > upperBound {
		t.Errorf("p99 invokeFrameHook overhead = %v, want < %v (catastrophic-regression guard, NOT throughput SLA — see BenchmarkFrameHook_NoopOverhead for that)",
			p99, upperBound)
	}
	t.Logf("invokeFrameHook p50=%v p99=%v p100=%v",
		timings[iterations/2], p99, timings[iterations-1])
}

// BenchmarkFrameHook_NoopOverhead (T035). Reports per-call overhead of
// invokeFrameHook with a no-op FramePass callback. Reference benchmark
// for tracking regressions on the per-frame hot path.
func BenchmarkFrameHook_NoopOverhead(b *testing.B) {
	o := &Owner{
		onFrameReceived: func(_ string, _ int, _ string) muxcore.FrameAction {
			return muxcore.FramePass
		},
		logger: log.New(io.Discard, "", 0),
	}
	s := NewSession(strings.NewReader(""), io.Discard)
	defer s.Close()
	msg, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":"1","method":"tools/list"}`))
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = o.invokeFrameHook(s, msg)
	}
}
