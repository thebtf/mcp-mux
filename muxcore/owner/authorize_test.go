package owner

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
)

// authorizeOwner builds a test Owner wired with the given AuthorizeSession
// callback (or none, when nil) and an optional SessionHandler. Defaults to
// the legacy noopSessionHandler when handler == nil.
func authorizeOwner(
	t *testing.T,
	cb func(ctx context.Context, conn muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth,
	handler muxcore.SessionHandler,
) (*Owner, string, *safeBuffer) {
	t.Helper()
	if handler == nil {
		handler = noopSessionHandler{}
	}
	socketPath := shortSocketPath(t)
	logBuf := &safeBuffer{}
	logger := log.New(logBuf, "", 0)

	o, err := NewOwner(OwnerConfig{
		SessionHandler:   handler,
		TokenHandshake:   true,
		IPCPath:          socketPath,
		AuthorizeSession: cb,
		Logger:           logger,
		ServerID:         "auth-test",
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	t.Cleanup(func() { o.Shutdown() })
	return o, socketPath, logBuf
}

// readDenyResponse reads one line from conn and returns it (without the
// trailing newline). Used by tests that expect the AuthDeny JSON-RPC -32000
// frame to arrive before the connection is closed.
func readDenyResponse(t *testing.T, conn io.Reader, timeout time.Duration) string {
	t.Helper()
	type readResult struct {
		s   string
		err error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		resultCh <- readResult{s: string(buf[:n]), err: err}
	}()
	select {
	case r := <-resultCh:
		if r.s == "" && r.err != nil && r.err != io.EOF {
			t.Fatalf("read deny response: %v", r.err)
		}
		return strings.TrimSpace(r.s)
	case <-time.After(timeout):
		t.Fatalf("read deny response: timeout after %v", timeout)
		return ""
	}
}

// TestAuthorize_DenyClosesConnection (T020) — AuthDeny verdict closes
// the connection and never adds the session.
func TestAuthorize_DenyClosesConnection(t *testing.T) {
	cb := func(_ context.Context, _ muxcore.ConnInfo, _ muxcore.ProjectContext) muxcore.SessionAuth {
		return muxcore.SessionAuth{Decision: muxcore.AuthDeny, Reason: "test-deny"}
	}
	o, socketPath, logBuf := authorizeOwner(t, cb, nil)
	o.SessionMgr().PreRegister("aaaaaaaa", "/proj", nil)

	conn := connectWithToken(t, socketPath, "aaaaaaaa")
	defer conn.Close()

	resp := readDenyResponse(t, conn, 2*time.Second)
	if !strings.Contains(resp, `"code":-32000`) {
		t.Errorf("missing -32000 in response: %q", resp)
	}
	if !strings.Contains(resp, `"test-deny"`) {
		t.Errorf("missing reason test-deny in response: %q", resp)
	}

	waitForCondition(t, 500*time.Millisecond, func() bool {
		return strings.Contains(logBuf.String(), "auth_deny sid=")
	}, "auth_deny log marker missing")

	// Session must not appear in the table.
	time.Sleep(50 * time.Millisecond)
	if got := sessionCount(o); got != 0 {
		t.Errorf("sessionCount after deny = %d, want 0", got)
	}
}

// metaCapturingHandler records every WithSessionMeta dispatch so tests can
// observe the meta payload that Owner forwards to the handler.
type metaCapturingHandler struct {
	mu    sync.Mutex
	metas []muxcore.SessionMeta
	resp  []byte
}

func (h *metaCapturingHandler) HandleRequest(_ context.Context, _ muxcore.ProjectContext, _ []byte) ([]byte, error) {
	return h.resp, nil
}

func (h *metaCapturingHandler) HandleRequestWithSessionMeta(
	_ context.Context,
	_ muxcore.ProjectContext,
	meta muxcore.SessionMeta,
	_ []byte,
) ([]byte, error) {
	h.mu.Lock()
	h.metas = append(h.metas, meta)
	h.mu.Unlock()
	return h.resp, nil
}

func (h *metaCapturingHandler) captured() []muxcore.SessionMeta {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]muxcore.SessionMeta, len(h.metas))
	copy(out, h.metas)
	return out
}

// sendRequestAndWait writes one tools/list request and waits for a response
// line back. Helper for the AuthAllow-path tests.
func sendRequestAndWait(t *testing.T, conn io.ReadWriter, timeout time.Duration) string {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "%s\n", `{"jsonrpc":"2.0","id":"1","method":"tools/list"}`); err != nil {
		t.Fatalf("write request: %v", err)
	}
	type r struct {
		s   string
		err error
	}
	ch := make(chan r, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		ch <- r{s: string(buf[:n]), err: err}
	}()
	select {
	case res := <-ch:
		return strings.TrimSpace(res.s)
	case <-time.After(timeout):
		t.Fatalf("response timeout after %v", timeout)
		return ""
	}
}

// TestAuthorize_AllowPopulatesSessionMeta (T021) — AuthAllow stamps
// TenantID + AuthorizedAt; the handler observes the populated meta.
func TestAuthorize_AllowPopulatesSessionMeta(t *testing.T) {
	cb := func(_ context.Context, _ muxcore.ConnInfo, _ muxcore.ProjectContext) muxcore.SessionAuth {
		return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: "frontend-team"}
	}
	handler := &metaCapturingHandler{resp: []byte(`{"jsonrpc":"2.0","id":"1","result":{}}`)}
	o, socketPath, logBuf := authorizeOwner(t, cb, handler)
	o.SessionMgr().PreRegister("bbbbbbbb", "/proj", nil)

	conn := connectWithToken(t, socketPath, "bbbbbbbb")
	defer conn.Close()

	waitForCondition(t, 500*time.Millisecond, func() bool {
		return sessionCount(o) == 1
	}, "session never added after allow")
	waitForCondition(t, 500*time.Millisecond, func() bool {
		return strings.Contains(logBuf.String(), `auth_allow sid=`)
	}, "auth_allow log marker missing")

	resp := sendRequestAndWait(t, conn, 2*time.Second)
	if !strings.Contains(resp, `"id":"1"`) {
		t.Fatalf("unexpected response: %q", resp)
	}

	metas := handler.captured()
	if len(metas) != 1 {
		t.Fatalf("expected 1 WithSessionMeta call, got %d", len(metas))
	}
	got := metas[0]
	if got.TenantID != "frontend-team" {
		t.Errorf("TenantID: got %q, want frontend-team", got.TenantID)
	}
	if got.AuthorizedAt.IsZero() {
		t.Error("AuthorizedAt: got zero, want non-zero (post-AuthAllow)")
	}
	if !got.IsAuthorized() {
		t.Error("IsAuthorized() == false after AuthAllow")
	}
}

// TestAuthorize_NilConfigUnchangedDispatch (T022) — cfg.AuthorizeSession=nil
// produces zero auth_* log markers across N sessions; v0.23 dispatch path
// is byte-identical (FR-7, NFR-3).
func TestAuthorize_NilConfigUnchangedDispatch(t *testing.T) {
	o, socketPath, logBuf := authorizeOwner(t, nil, nil)

	const sessions = 5
	conns := make([]net.Conn, 0, sessions)
	for i := 0; i < sessions; i++ {
		token := fmt.Sprintf("0000000%d", i) // hex tokens (readToken gates on isHexChar)
		o.SessionMgr().PreRegister(token, "/proj", nil)
		c := connectWithToken(t, socketPath, token)
		conns = append(conns, c)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			c.Close()
		}
	})

	waitForCondition(t, 500*time.Millisecond, func() bool {
		return sessionCount(o) == sessions
	}, "all 5 sessions should be added")

	logs := logBuf.String()
	for _, marker := range []string{"auth_invoked", "auth_allow", "auth_deny", "auth_panic", "auth_skipped_shutdown"} {
		if strings.Contains(logs, marker) {
			t.Errorf("nil AuthorizeSession path produced %q marker — should be byte-identical to v0.23", marker)
		}
	}
}

// TestAuthorize_PanicTreatedAsDeny (T023) — callback panic is caught,
// reported as auth_panic, treated as AuthDeny{"authorize panic"}, and
// the daemon survives (subsequent connection succeeds with a non-panicking
// callback).
func TestAuthorize_PanicTreatedAsDeny(t *testing.T) {
	var panicMode atomic.Bool
	panicMode.Store(true)
	cb := func(_ context.Context, _ muxcore.ConnInfo, _ muxcore.ProjectContext) muxcore.SessionAuth {
		if panicMode.Load() {
			panic("test-panic")
		}
		return muxcore.SessionAuth{Decision: muxcore.AuthAllow}
	}
	o, socketPath, logBuf := authorizeOwner(t, cb, nil)
	o.SessionMgr().PreRegister("ccccccc1", "/proj", nil)
	o.SessionMgr().PreRegister("ccccccc2", "/proj", nil)

	conn1 := connectWithToken(t, socketPath, "ccccccc1")
	defer conn1.Close()
	resp := readDenyResponse(t, conn1, 2*time.Second)
	if !strings.Contains(resp, `"code":-32000`) {
		t.Errorf("missing -32000 after panic: %q", resp)
	}
	if !strings.Contains(resp, "authorize panic") {
		t.Errorf("missing 'authorize panic' reason: %q", resp)
	}

	waitForCondition(t, 500*time.Millisecond, func() bool {
		return strings.Contains(logBuf.String(), "auth_panic sid=")
	}, "auth_panic log marker missing")

	// Daemon must remain accepting — switch to non-panicking allow path.
	panicMode.Store(false)
	conn2 := connectWithToken(t, socketPath, "ccccccc2")
	defer conn2.Close()
	waitForCondition(t, 500*time.Millisecond, func() bool {
		return sessionCount(o) == 1
	}, "subsequent connection rejected — daemon did not survive panic")
}

// TestAuthorize_AllowEmptyTenantIdLegitimate (T024 / CHK013 / EC-12) —
// AuthAllow with empty TenantID is dispatched normally; meta.TenantID==""
// AND meta.IsAuthorized()==true distinguishes "authorized but no tenant"
// from "callback not configured".
func TestAuthorize_AllowEmptyTenantIdLegitimate(t *testing.T) {
	cb := func(_ context.Context, _ muxcore.ConnInfo, _ muxcore.ProjectContext) muxcore.SessionAuth {
		return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: ""}
	}
	handler := &metaCapturingHandler{resp: []byte(`{"jsonrpc":"2.0","id":"1","result":{}}`)}
	o, socketPath, _ := authorizeOwner(t, cb, handler)
	o.SessionMgr().PreRegister("dddddddd", "/proj", nil)

	conn := connectWithToken(t, socketPath, "dddddddd")
	defer conn.Close()

	waitForCondition(t, 500*time.Millisecond, func() bool {
		return sessionCount(o) == 1
	}, "session not added after empty-tenant AuthAllow")

	_ = sendRequestAndWait(t, conn, 2*time.Second)

	metas := handler.captured()
	if len(metas) != 1 {
		t.Fatalf("expected 1 WithSessionMeta call, got %d", len(metas))
	}
	got := metas[0]
	if got.TenantID != "" {
		t.Errorf("TenantID: got %q, want empty", got.TenantID)
	}
	if !got.IsAuthorized() {
		t.Error("IsAuthorized() == false despite AuthAllow — discriminator broken")
	}
}

// TestAuthorize_DenyNoUpstreamSpawn (T025) — Owner running with a
// SessionHandler has no subprocess upstream by design (o.upstream == nil
// before connect). After AuthDeny, o.upstream MUST remain nil — denial
// is zero-cost on resources.
func TestAuthorize_DenyNoUpstreamSpawn(t *testing.T) {
	cb := func(_ context.Context, _ muxcore.ConnInfo, _ muxcore.ProjectContext) muxcore.SessionAuth {
		return muxcore.SessionAuth{Decision: muxcore.AuthDeny, Reason: "no-upstream-test"}
	}
	o, socketPath, _ := authorizeOwner(t, cb, nil)
	o.SessionMgr().PreRegister("eeeeeeee", "/proj", nil)

	upstreamBefore := o.upstream
	if upstreamBefore != nil {
		t.Fatalf("baseline: o.upstream should be nil for a SessionHandler-only Owner, got %v", upstreamBefore)
	}

	conn := connectWithToken(t, socketPath, "eeeeeeee")
	defer conn.Close()

	_ = readDenyResponse(t, conn, 2*time.Second)

	// 100ms is enough for any spurious spawn goroutine to land on o.upstream.
	time.Sleep(100 * time.Millisecond)
	if o.upstream != nil {
		t.Errorf("o.upstream != nil after deny — denial path spawned upstream (got %v)", o.upstream)
	}
	if got := sessionCount(o); got != 0 {
		t.Errorf("sessionCount after deny = %d, want 0", got)
	}
}
