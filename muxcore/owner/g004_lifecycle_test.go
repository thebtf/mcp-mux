package owner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
)

func g004LifecycleReceive[T any](t *testing.T, ch <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}

type g004LegacyLifecycleHandler struct {
	entered     chan struct{}
	release     chan struct{}
	returned    atomic.Bool
	requests    chan bool
	disconnects chan bool
}

func (h *g004LegacyLifecycleHandler) OnProjectConnect(muxcore.ProjectContext) {
	close(h.entered)
	<-h.release
	h.returned.Store(true)
}

func (h *g004LegacyLifecycleHandler) OnProjectDisconnect(string) {
	h.disconnects <- h.returned.Load()
}

func (h *g004LegacyLifecycleHandler) HandleRequest(_ context.Context, _ muxcore.ProjectContext, request []byte) ([]byte, error) {
	h.requests <- h.returned.Load()
	return g004LifecycleResponse(request), nil
}

func g004LifecycleResponse(request []byte) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":true}`, parseMessage(request).ID))
}

func TestG004LifecycleLegacyConnectCompletesBeforeRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &g004LegacyLifecycleHandler{
			entered: make(chan struct{}), release: make(chan struct{}),
			requests: make(chan bool, 2), disconnects: make(chan bool, 2),
		}
		o := newDispatchOwner(h)
		reader, writer := io.Pipe()
		output := &safeBuf{}
		s := NewSession(reader, output)
		s.Cwd = "/g004/legacy-connect"
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(h.release) }) }
		defer func() {
			release()
			_ = writer.Close()
			s.Close()
			synctest.Wait()
			o.Shutdown()
		}()

		added := make(chan struct{})
		go func() { o.AddSession(s); close(added) }()
		g004LifecycleReceive(t, h.entered, "legacy connect entry")
		written := make(chan error, 1)
		go func() {
			_, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":701,"method":"tools/list","params":{}}`)
			written <- err
		}()

		// Quiescence distinguishes a queued frame from dispatch without a timing window.
		synctest.Wait()
		if len(h.requests) != 0 {
			t.Error("request reached the consumer while its connect callback was blocked")
		}
		release()
		g004LifecycleReceive(t, added, "legacy admission return")
		if err := g004LifecycleReceive(t, written, "queued request write"); err != nil {
			t.Fatal(err)
		}
		if !g004LifecycleReceive(t, h.requests, "legacy request dispatch") {
			t.Error("request observed an unfinished connect callback")
		}
		synctest.Wait()
		var response struct {
			ID     int  `json:"id"`
			Result bool `json:"result"`
		}
		if err := json.Unmarshal([]byte(output.String()), &response); err != nil || response.ID != 701 || !response.Result {
			t.Fatalf("consumer response = %q, decode error = %v", output.String(), err)
		}
	})
}

func TestG004LifecycleLegacyDisconnectWaitsForConnectReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &g004LegacyLifecycleHandler{
			entered: make(chan struct{}), release: make(chan struct{}),
			requests: make(chan bool, 2), disconnects: make(chan bool, 2),
		}
		o := newDispatchOwner(h)
		reader, writer := io.Pipe()
		s := NewSession(reader, &safeBuf{})
		s.Cwd = "/g004/legacy-disconnect"
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(h.release) }) }
		defer func() {
			release()
			_ = writer.Close()
			s.Close()
			synctest.Wait()
			o.Shutdown()
		}()

		added := make(chan struct{})
		go func() { o.AddSession(s); close(added) }()
		g004LifecycleReceive(t, h.entered, "legacy connect entry")
		_ = writer.Close()
		s.Close()
		synctest.Wait()
		if len(h.disconnects) != 0 {
			t.Error("disconnect overtook the same session's blocked connect callback")
		}
		release()
		g004LifecycleReceive(t, added, "legacy admission return")
		if !g004LifecycleReceive(t, h.disconnects, "legacy disconnect") {
			t.Error("disconnect observed an unfinished connect callback")
		}
		synctest.Wait()
		if o.SessionCount() != 0 || len(h.disconnects) != 0 {
			t.Fatalf("close left a ghost or duplicate disconnect: sessions=%d extra=%d", o.SessionCount(), len(h.disconnects))
		}
	})
}

type g004LifecycleEvent struct {
	kind    string
	project muxcore.ProjectContext
	meta    muxcore.SessionMeta
}

type g004MetadataLifecycleHandler struct {
	mu        sync.Mutex
	events    []g004LifecycleEvent
	next      chan g004LifecycleEvent
	onConnect func(muxcore.ProjectContext, muxcore.SessionMeta)
}

var _ muxcore.ProjectLifecycleWithSessionMeta = (*g004MetadataLifecycleHandler)(nil)

func (h *g004MetadataLifecycleHandler) record(kind string, project muxcore.ProjectContext, meta muxcore.SessionMeta) {
	event := g004LifecycleEvent{kind: kind, project: project, meta: meta}
	h.mu.Lock()
	h.events = append(h.events, event)
	h.mu.Unlock()
	h.next <- event
}

func (h *g004MetadataLifecycleHandler) counts() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	counts := make(map[string]int)
	for _, event := range h.events {
		counts[event.kind]++
	}
	return counts
}

func (h *g004MetadataLifecycleHandler) OnProjectConnect(project muxcore.ProjectContext) {
	h.record("legacy-connect", project, muxcore.SessionMeta{})
}

func (h *g004MetadataLifecycleHandler) OnProjectDisconnect(projectID string) {
	h.record("legacy-disconnect", muxcore.ProjectContext{ID: projectID}, muxcore.SessionMeta{})
}

func (h *g004MetadataLifecycleHandler) OnProjectConnectWithSessionMeta(project muxcore.ProjectContext, meta muxcore.SessionMeta) {
	if h.onConnect != nil {
		h.onConnect(project, meta)
	}
	h.record("connect", project, meta)
}

func (h *g004MetadataLifecycleHandler) OnProjectDisconnectWithSessionMeta(projectID string, meta muxcore.SessionMeta) {
	h.record("disconnect", muxcore.ProjectContext{ID: projectID}, meta)
}

func (h *g004MetadataLifecycleHandler) HandleRequest(_ context.Context, project muxcore.ProjectContext, request []byte) ([]byte, error) {
	h.record("legacy-request", project, muxcore.SessionMeta{})
	return g004LifecycleResponse(request), nil
}

func (h *g004MetadataLifecycleHandler) HandleRequestWithSessionMeta(_ context.Context, project muxcore.ProjectContext, meta muxcore.SessionMeta, request []byte) ([]byte, error) {
	h.record("request", project, meta)
	return g004LifecycleResponse(request), nil
}

func (h *g004MetadataLifecycleHandler) HandleNotification(_ context.Context, project muxcore.ProjectContext, _ []byte) {
	h.record("legacy-notification", project, muxcore.SessionMeta{})
}

func (h *g004MetadataLifecycleHandler) HandleNotificationWithSessionMeta(_ context.Context, project muxcore.ProjectContext, meta muxcore.SessionMeta, _ []byte) {
	h.record("notification", project, meta)
}

func g004LifecycleExpect(t *testing.T, h *g004MetadataLifecycleHandler, kind string) g004LifecycleEvent {
	t.Helper()
	event := g004LifecycleReceive(t, h.next, kind)
	if event.kind != kind {
		t.Fatalf("callback = %q, want %q (session=%q)", event.kind, kind, event.meta.SessionID)
	}
	return event
}

func g004LifecycleRequest(t *testing.T, conn net.Conn, id int) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer conn.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`+"\n", id); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("request %d response: %v", id, err)
	}
	var response struct {
		ID     int  `json:"id"`
		Result bool `json:"result"`
	}
	if err := json.Unmarshal(line, &response); err != nil || response.ID != id || !response.Result {
		t.Fatalf("request %d response = %s, decode error = %v", id, line, err)
	}
}

func g004LifecycleAssertMeta(t *testing.T, event g004LifecycleEvent, projectID string, want muxcore.SessionMeta) {
	t.Helper()
	if event.project.ID != projectID || event.meta != want {
		t.Fatalf("%s changed session identity: project=%q meta=%+v, want project=%q meta=%+v", event.kind, event.project.ID, event.meta, projectID, want)
	}
}

func TestG004LifecycleMetadataKeepsSameProjectSessionsDistinct(t *testing.T) {
	h := &g004MetadataLifecycleHandler{next: make(chan g004LifecycleEvent, 64)}
	path := testIPCPath(t)
	o, err := NewOwner(OwnerConfig{
		IPCPath: path, ServerID: "g004-lifecycle-identity", TokenHandshake: true,
		SessionHandler: h, Logger: testLogger(t),
		AuthorizeSession: func(_ context.Context, _ muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth {
			h.record("legacy-authorize", project, muxcore.SessionMeta{})
			return muxcore.SessionAuth{Decision: muxcore.AuthDeny}
		},
		AuthorizeSessionWithSessionMeta: func(_ context.Context, project muxcore.ProjectContext, meta muxcore.SessionMeta) muxcore.SessionAuth {
			h.record("authorize", project, meta)
			return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: "same-project-tenant"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Shutdown)

	const cwd = "/g004/same-project"
	projectID := muxcore.ProjectContextID(cwd)
	var connections []net.Conn
	var metas []muxcore.SessionMeta
	for index, token := range []string{"aaaaaaaa", "bbbbbbbb"} {
		if !o.PreRegister(token, cwd, map[string]string{"connection": fmt.Sprint(index)}) {
			t.Fatal("could not reserve same-project connection")
		}
		conn := connectWithToken(t, path, token)
		t.Cleanup(func() { _ = conn.Close() })
		connections = append(connections, conn)
		auth := g004LifecycleExpect(t, h, "authorize")
		if auth.meta.SessionID == "" || auth.meta.TenantID != "" || auth.meta.IsAuthorized() {
			t.Fatalf("authorization did not receive a fresh preauthorization identity: %+v", auth.meta)
		}
		if auth.project.ID != projectID || auth.project.Cwd != cwd || auth.project.Env["connection"] != fmt.Sprint(index) {
			t.Fatalf("authorization project = %+v, want connection %d in %q", auth.project, index, cwd)
		}
		connected := g004LifecycleExpect(t, h, "connect")
		if connected.meta.SessionID != auth.meta.SessionID || connected.meta.Conn != auth.meta.Conn || connected.meta.TenantID != "same-project-tenant" || !connected.meta.IsAuthorized() {
			t.Fatalf("connect lost authorization identity or verdict: before=%+v after=%+v", auth.meta, connected.meta)
		}
		g004LifecycleAssertMeta(t, connected, projectID, connected.meta)
		metas = append(metas, connected.meta)
	}
	if metas[0].SessionID == metas[1].SessionID {
		t.Fatalf("same-project connections shared session ID %q", metas[0].SessionID)
	}

	for index, conn := range connections {
		g004LifecycleRequest(t, conn, 710+index)
		g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "request"), projectID, metas[index])
		if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","method":"notifications/g004","params":{}}`); err != nil {
			t.Fatal(err)
		}
		g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "notification"), projectID, metas[index])
	}

	_ = connections[0].Close()
	g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "disconnect"), projectID, metas[0])
	if o.SessionCount() != 1 {
		t.Fatalf("closing one same-project session removed its peer: sessions=%d", o.SessionCount())
	}
	g004LifecycleRequest(t, connections[1], 712)
	g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "request"), projectID, metas[1])
	_ = connections[1].Close()
	g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "disconnect"), projectID, metas[1])

	// A native reconnect mints a fresh token and must also mint a fresh session identity.
	reconnectToken, err := o.SessionMgr().RegisterReconnect("aaaaaaaa", func(ownerKey string) bool {
		return ownerKey == o.ServerID() && o.IsAccepting()
	})
	if err != nil {
		t.Fatal(err)
	}
	reconnected := connectWithToken(t, path, reconnectToken)
	t.Cleanup(func() { _ = reconnected.Close() })
	reauth := g004LifecycleExpect(t, h, "authorize")
	if reauth.meta.SessionID == "" || reauth.meta.SessionID == metas[0].SessionID || reauth.meta.SessionID == metas[1].SessionID {
		t.Fatalf("reconnect reused a previous admission identity: %+v", reauth.meta)
	}
	reconnect := g004LifecycleExpect(t, h, "connect")
	if reconnect.meta.SessionID != reauth.meta.SessionID || !reconnect.meta.IsAuthorized() {
		t.Fatalf("reconnect authorization identity was not preserved: auth=%+v connect=%+v", reauth.meta, reconnect.meta)
	}
	_ = reconnected.Close()
	g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "disconnect"), projectID, reconnect.meta)
	o.closeListener()
	g004LifecycleReceive(t, o.acceptDone, "accept producer return")
	o.sessionReaders.Wait()
	waitForCondition(t, 3*time.Second, func() bool {
		return o.nativeWork.Load() == 0 && o.PendingRequests() == 0
	}, "lifecycle callbacks did not actually return")
	o.Shutdown()
	g004LifecycleReceive(t, o.Done(), "owner shutdown after lifecycle callbacks")
	counts := h.counts()
	for _, kind := range []string{"legacy-authorize", "legacy-connect", "legacy-disconnect", "legacy-request", "legacy-notification"} {
		if counts[kind] != 0 {
			t.Errorf("metadata upgrade also invoked %s %d times", kind, counts[kind])
		}
	}
	for kind, want := range map[string]int{"authorize": 3, "connect": 3, "disconnect": 3, "request": 3, "notification": 2} {
		if counts[kind] != want {
			t.Errorf("%s callbacks = %d, want %d", kind, counts[kind], want)
		}
	}
}

func TestG004LifecycleBlockedConnectIsSessionLocal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		blocked := make(chan struct{})
		h := &g004MetadataLifecycleHandler{
			next: make(chan g004LifecycleEvent, 64),
			onConnect: func(project muxcore.ProjectContext, _ muxcore.SessionMeta) {
				if project.Env["connection"] == "first" {
					close(entered)
					<-blocked
				}
			},
		}
		o := newDispatchOwner(h)
		firstReader, firstWriter := io.Pipe()
		first := NewSession(firstReader, &safeBuf{})
		first.Cwd = "/g004/blocked-same-project"
		first.Env = map[string]string{"connection": "first"}
		secondReader, secondWriter := io.Pipe()
		second := NewSession(secondReader, &safeBuf{})
		second.Cwd = first.Cwd
		second.Env = map[string]string{"connection": "second"}
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(blocked) }) }
		defer func() {
			release()
			_ = firstWriter.Close()
			_ = secondWriter.Close()
			first.Close()
			second.Close()
			synctest.Wait()
			o.Shutdown()
		}()

		go o.AddSession(first)
		g004LifecycleReceive(t, entered, "first metadata connect entry")
		go o.AddSession(second)
		connected := g004LifecycleExpect(t, h, "connect")
		if connected.project.Env["connection"] != "second" || connected.meta.SessionID == "" {
			t.Fatalf("blocked connect prevented independent same-project admission: %+v", connected)
		}
		firstMeta := first.Meta()
		if firstMeta.SessionID == "" || firstMeta.SessionID == connected.meta.SessionID {
			t.Fatalf("native admissions did not receive distinct session identities: first=%+v second=%+v", firstMeta, connected.meta)
		}
		written := make(chan error, 1)
		go func() {
			_, err := fmt.Fprintln(secondWriter, `{"jsonrpc":"2.0","id":720,"method":"tools/list","params":{}}`)
			written <- err
		}()
		if err := g004LifecycleReceive(t, written, "independent session request write"); err != nil {
			t.Fatal(err)
		}
		g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "request"), connected.project.ID, connected.meta)
		firstWritten := make(chan error, 1)
		go func() {
			_, err := fmt.Fprintln(firstWriter, `{"jsonrpc":"2.0","id":721,"method":"tools/list","params":{}}`)
			firstWritten <- err
		}()
		synctest.Wait()
		if len(h.next) != 0 {
			t.Error("request overtook its blocked metadata connect callback")
		}

		_ = firstWriter.Close()
		first.Close()
		g004LifecycleReceive(t, firstWritten, "closed session's queued write return")
		synctest.Wait()
		if len(h.next) != 0 {
			t.Fatal("disconnect overtook its blocked metadata connect callback")
		}
		release()
		g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "connect"), connected.project.ID, firstMeta)
		g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "disconnect"), connected.project.ID, firstMeta)
		synctest.Wait()
		if o.SessionCount() != 1 || second.IsClosed() {
			t.Fatal("first-session cleanup affected the live same-project session")
		}
	})
}

func TestG004LifecycleAuthorizedAdmissionAbortCleansExactlyOnce(t *testing.T) {
	entered := make(chan g004LifecycleEvent, 1)
	blocked := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked) }) }
	h := &g004MetadataLifecycleHandler{next: make(chan g004LifecycleEvent, 64)}
	path := testIPCPath(t)
	o, err := NewOwner(OwnerConfig{
		IPCPath: path, ServerID: "g004-lifecycle-abort", TokenHandshake: true,
		SessionHandler: h, Logger: testLogger(t),
		AuthorizeSessionWithSessionMeta: func(_ context.Context, project muxcore.ProjectContext, meta muxcore.SessionMeta) muxcore.SessionAuth {
			entered <- g004LifecycleEvent{kind: "authorize", project: project, meta: meta}
			<-blocked
			return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: "reserved-tenant"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(); o.Shutdown() })
	const token = "cccccccc"
	const cwd = "/g004/aborted-admission"
	if !o.PreRegister(token, cwd, nil) {
		t.Fatal("could not reserve aborted admission")
	}
	conn := connectWithToken(t, path, token)
	t.Cleanup(func() { _ = conn.Close() })
	auth := g004LifecycleReceive(t, entered, "blocked metadata authorization")
	if auth.meta.SessionID == "" || auth.meta.IsAuthorized() || auth.meta.TenantID != "" {
		t.Fatalf("reservation did not receive a preauthorization session identity: %+v", auth.meta)
	}

	// Closing admission must not wait for consumer code or its authorization result.
	closed := make(chan struct{})
	go func() { o.closeListener(); close(closed) }()
	g004LifecycleReceive(t, closed, "listener closure during blocked authorization")
	_ = conn.Close()
	if o.SessionCount() != 0 {
		t.Fatal("blocked authorization published an uncommitted session")
	}
	select {
	case <-o.acceptDone:
		t.Fatal("accept producer completed before the authorization callback returned")
	default:
	}
	release()
	aborted := g004LifecycleExpect(t, h, "disconnect")
	if aborted.project.ID != muxcore.ProjectContextID(cwd) || aborted.meta.SessionID != auth.meta.SessionID {
		t.Fatalf("abort cleaned another reservation: authorization=%+v disconnect=%+v", auth, aborted)
	}
	g004LifecycleReceive(t, o.acceptDone, "aborted admission producer return")
	waitForCondition(t, 3*time.Second, func() bool { return o.nativeWork.Load() == 0 }, "abort callback did not actually return")
	o.Shutdown()
	g004LifecycleReceive(t, o.Done(), "owner shutdown after admission abort")
	counts := h.counts()
	if o.SessionCount() != 0 || counts["disconnect"] != 1 || len(h.next) != 0 {
		t.Fatalf("abort left a ghost or duplicate lifecycle event: sessions=%d callbacks=%v queued=%d", o.SessionCount(), counts, len(h.next))
	}
	for _, kind := range []string{"connect", "legacy-connect", "legacy-disconnect", "request", "notification", "legacy-request", "legacy-notification"} {
		if counts[kind] != 0 {
			t.Errorf("aborted admission later invoked %s %d times", kind, counts[kind])
		}
	}
}
