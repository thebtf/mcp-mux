package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/session"
)

type sessionNotifierAdmission struct {
	project muxcore.ProjectContext
	meta    muxcore.SessionMeta
}

type sessionNotifierHandler struct {
	mu           sync.Mutex
	notifier     muxcore.SessionNotifier
	connected    chan sessionNotifierAdmission
	disconnected chan string
	requests     chan sessionNotifierAdmission
}

var (
	_ muxcore.SessionNotifierAware            = (*sessionNotifierHandler)(nil)
	_ muxcore.SessionHandlerWithSessionMeta   = (*sessionNotifierHandler)(nil)
	_ muxcore.ProjectLifecycleWithSessionMeta = (*sessionNotifierHandler)(nil)
)

func (h *sessionNotifierHandler) SetSessionNotifier(n muxcore.SessionNotifier) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifier = n
}

func (h *sessionNotifierHandler) router(t *testing.T) muxcore.SessionNotifier {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.notifier == nil {
		t.Fatal("daemon did not inject a session notifier")
	}
	return h.notifier
}

func (h *sessionNotifierHandler) HandleRequest(context.Context, muxcore.ProjectContext, []byte) ([]byte, error) {
	return nil, fmt.Errorf("request was not dispatched with session metadata")
}

func (h *sessionNotifierHandler) HandleRequestWithSessionMeta(_ context.Context, p muxcore.ProjectContext, m muxcore.SessionMeta, raw []byte) ([]byte, error) {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	h.requests <- sessionNotifierAdmission{p, m}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  string          `json:"result"`
	}{"2.0", req.ID, "barrier"})
}

func (h *sessionNotifierHandler) OnProjectConnectWithSessionMeta(p muxcore.ProjectContext, m muxcore.SessionMeta) {
	h.connected <- sessionNotifierAdmission{p, m}
}

func (h *sessionNotifierHandler) OnProjectDisconnectWithSessionMeta(_ string, m muxcore.SessionMeta) {
	h.disconnected <- m.SessionID
}

type notifierBlockingWriter struct {
	writer  io.Writer
	started chan struct{}
	once    sync.Once
}

func (w *notifierBlockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.writer.Write(p)
}

type notifierHost struct {
	input        *io.PipeWriter
	output       *io.PipeReader
	session      *session.Session
	frames       chan string
	writeStarted <-chan struct{}
}

func notifierReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value, ok := <-ch:
		if !ok {
			t.Fatal("fixture channel closed before expected event")
		}
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fixture event")
		var zero T
		return zero
	}
}

func notifierStartReader(h *notifierHost) {
	go func() {
		defer close(h.frames)
		s := bufio.NewScanner(h.output)
		for s.Scan() {
			h.frames <- s.Text()
		}
	}()
}

func notifierAttach(t *testing.T, o *owner.Owner, cwd string, env map[string]string, read bool) *notifierHost {
	t.Helper()
	in, input := io.Pipe()
	output, out := io.Pipe()
	var writer io.Writer = out
	var started <-chan struct{}
	if !read {
		blocked := &notifierBlockingWriter{writer: out, started: make(chan struct{})}
		writer, started = blocked, blocked.started
	}
	s := session.NewSession(in, writer)
	s.Cwd, s.Env = cwd, env
	h := &notifierHost{input: input, output: output, session: s, frames: make(chan string, 512), writeStarted: started}
	if read {
		notifierStartReader(h)
	}
	t.Cleanup(func() { _ = input.Close(); _ = in.Close(); _ = output.Close(); _ = out.Close() })
	o.AddSession(s)
	return h
}

func notifierFrame(label string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/test","params":{"label":%q}}`, label)
}

func notifierNewDaemon(t *testing.T) (*Daemon, *sessionNotifierHandler) {
	t.Helper()
	h := &sessionNotifierHandler{connected: make(chan sessionNotifierAdmission, 16), disconnected: make(chan string, 16), requests: make(chan sessionNotifierAdmission, 16)}
	d, err := New(Config{Name: "sn", ControlPath: shortSocketPath(t, "session-notifier"), SessionHandler: h, SkipSnapshot: true, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Shutdown() })
	return d, h
}

func notifierSpawn(t *testing.T, d *Daemon, command, cwd, protocol string, env map[string]string) (string, *owner.Owner) {
	t.Helper()
	_, id, _, err := d.Spawn(control.Request{Command: command, Cwd: cwd, Mode: "global", ProtocolEra: protocol, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	entry := d.Entry(id)
	if entry == nil || entry.Owner == nil {
		t.Fatal("spawn did not publish owner")
	}
	return id, entry.Owner
}

func notifierBarrier(t *testing.T, n muxcore.SessionNotifier, hosts []*notifierHost, expected map[string][]string) {
	t.Helper()
	for i, host := range hosts {
		id := host.session.Meta().SessionID
		marker := notifierFrame(fmt.Sprintf("barrier-%d", i))
		if err := n.NotifySession(id, []byte(marker)); err != nil {
			t.Fatalf("barrier to %s: %v", id, err)
		}
		for _, frame := range append(expected[id], marker) {
			if got := notifierReceive(t, host.frames); got != frame {
				t.Fatalf("session %s got %s, want %s", id, got, frame)
			}
		}
	}
}

func TestSessionNotifierExactAdmissionRouting(t *testing.T) {
	d, handler := notifierNewDaemon(t)
	project, otherProject := t.TempDir(), t.TempDir()
	envA, envB := map[string]string{"SESSION_TEST_TOKEN": "a"}, map[string]string{"SESSION_TEST_TOKEN": "b"}
	idA, ownerA := notifierSpawn(t, d, "session-notifier", project, "", envA)
	a := notifierAttach(t, ownerA, project, envA, true)
	admissionA := notifierReceive(t, handler.connected)
	other := notifierAttach(t, ownerA, otherProject, envA, true)
	admissionOther := notifierReceive(t, handler.connected)
	idB, ownerB := notifierSpawn(t, d, "session-notifier", project, "", envB)
	if idA == idB {
		t.Fatal("different effective credential environments shared an owner")
	}
	b := notifierAttach(t, ownerB, project, envB, true)
	admissionB := notifierReceive(t, handler.connected)
	if admissionA.meta.SessionID == "" || admissionA.meta.SessionID != a.session.Meta().SessionID || admissionB.meta.SessionID != b.session.Meta().SessionID || admissionOther.meta.SessionID != other.session.Meta().SessionID {
		t.Fatal("lifecycle did not expose exact admission identities")
	}
	if admissionA.project.ID != admissionB.project.ID || admissionA.project.ID == admissionOther.project.ID {
		t.Fatal("fixture did not produce same-project and different-project admissions")
	}
	for _, host := range []*notifierHost{a, b, other} {
		if _, err := fmt.Fprintln(host.input, `{"jsonrpc":"2.0","id":1,"method":"test/metadata"}`); err != nil {
			t.Fatal(err)
		}
		got := notifierReceive(t, handler.requests)
		if got.meta.SessionID != host.session.Meta().SessionID {
			t.Fatal("request metadata differs from admission identity")
		}
		if frame := notifierReceive(t, host.frames); frame != `{"jsonrpc":"2.0","id":1,"result":"barrier"}` {
			t.Fatalf("request response: %s", frame)
		}
	}
	n := handler.router(t)
	first, second := notifierFrame("earlier-owner"), notifierFrame("later-owner")
	if err := n.NotifySession(admissionA.meta.SessionID, []byte(first)); err != nil {
		t.Fatal(err)
	}
	if err := n.NotifySession(admissionB.meta.SessionID, []byte(second)); err != nil {
		t.Fatal(err)
	}
	notifierBarrier(t, n, []*notifierHost{a, b, other}, map[string][]string{admissionA.meta.SessionID: {first}, admissionB.meta.SessionID: {second}})
	for _, invalid := range []string{`not json`, `{"jsonrpc":"2.0","id":1,"method":"test"}`, `{"jsonrpc":"2.0","id":1,"result":{}}`, `{"jsonrpc":"1.0","method":"test"}`} {
		if err := n.NotifySession(admissionA.meta.SessionID, []byte(invalid)); err == nil {
			t.Fatalf("accepted invalid notification: %s", invalid)
		}
	}
	if err := n.NotifySession("unknown-session", []byte(first)); err == nil {
		t.Fatal("unknown identity accepted")
	}
	notifierBarrier(t, n, []*notifierHost{a, b, other}, nil)
	if err := a.input.Close(); err != nil {
		t.Fatal(err)
	}
	if got := notifierReceive(t, handler.disconnected); got != admissionA.meta.SessionID {
		t.Fatalf("disconnected %s, want %s", got, admissionA.meta.SessionID)
	}
	if err := n.NotifySession(admissionA.meta.SessionID, []byte(first)); err == nil {
		t.Fatal("disconnected identity accepted")
	}
	if err := d.Remove(idB); err != nil {
		t.Fatal(err)
	}
	if err := n.NotifySession(admissionB.meta.SessionID, []byte(second)); err == nil {
		t.Fatal("removed owner's identity accepted")
	}
	_, replacement := notifierSpawn(t, d, "session-notifier", project, "", envB)
	newHost := notifierAttach(t, replacement, project, envB, true)
	newAdmission := notifierReceive(t, handler.connected)
	if newAdmission.meta.SessionID == admissionB.meta.SessionID {
		t.Fatal("replacement reused admission identity")
	}
	if err := n.NotifySession(admissionB.meta.SessionID, []byte(second)); err == nil {
		t.Fatal("replacement revived old identity")
	}
	if err := n.NotifySession(newAdmission.meta.SessionID, []byte(second)); err != nil {
		t.Fatal(err)
	}
	notifierBarrier(t, n, []*notifierHost{newHost, other}, map[string][]string{newAdmission.meta.SessionID: {second}})
	d.Shutdown()
	if err := n.NotifySession(newAdmission.meta.SessionID, []byte(second)); err == nil {
		t.Fatal("shutdown left identity routable")
	}
}

func TestSessionNotifierSameOwnerSameProjectAdmissions(t *testing.T) {
	d, handler := notifierNewDaemon(t)
	cwd := t.TempDir()
	_, o := notifierSpawn(t, d, "session-notifier-same-project", cwd, "", nil)
	a := notifierAttach(t, o, cwd, nil, true)
	admissionA := notifierReceive(t, handler.connected)
	b := notifierAttach(t, o, cwd, nil, true)
	admissionB := notifierReceive(t, handler.connected)
	if admissionA.project.ID != admissionB.project.ID || admissionA.meta.SessionID == "" || admissionA.meta.SessionID == admissionB.meta.SessionID {
		t.Fatal("fixture did not create distinct admissions for the same project and owner")
	}
	n := handler.router(t)
	first, second := notifierFrame("same-owner-a"), notifierFrame("same-owner-b")
	if err := n.NotifySession(admissionA.meta.SessionID, []byte(first)); err != nil {
		t.Fatal(err)
	}
	notifierBarrier(t, n, []*notifierHost{a, b}, map[string][]string{admissionA.meta.SessionID: {first}})
	if err := n.NotifySession(admissionB.meta.SessionID, []byte(second)); err != nil {
		t.Fatal(err)
	}
	notifierBarrier(t, n, []*notifierHost{a, b}, map[string][]string{admissionB.meta.SessionID: {second}})
	if err := a.input.Close(); err != nil {
		t.Fatal(err)
	}
	if got := notifierReceive(t, handler.disconnected); got != admissionA.meta.SessionID {
		t.Fatalf("disconnected %q, want %q", got, admissionA.meta.SessionID)
	}
	if err := n.NotifySession(admissionA.meta.SessionID, []byte(first)); err == nil {
		t.Fatal("disconnected session remains routable")
	}
	if err := n.NotifySession(admissionB.meta.SessionID, []byte(second)); err != nil {
		t.Fatal(err)
	}
	notifierBarrier(t, n, []*notifierHost{b}, map[string][]string{admissionB.meta.SessionID: {second}})
}

func TestSessionNotifierSnapshotRestoreRevokesPriorAdmissions(t *testing.T) {
	d, handler := notifierNewDaemon(t)
	cwd := t.TempDir()
	serverID, predecessor := notifierSpawn(t, d, "session-notifier-restore", cwd, "", nil)
	a := notifierAttach(t, predecessor, cwd, nil, true)
	oldA := notifierReceive(t, handler.connected).meta.SessionID
	b := notifierAttach(t, predecessor, cwd, nil, true)
	oldB := notifierReceive(t, handler.connected).meta.SessionID
	n := handler.router(t)
	notifierBarrier(t, n, []*notifierHost{a, b}, nil)
	// This exercises the daemon's real snapshot-plan fallback for an in-process
	// SessionHandler. It does not model transfer of a live upstream process.
	if _, err := d.SerializeSnapshot(); err != nil {
		t.Fatalf("serialize daemon snapshot: %v", err)
	}
	captured, err := DeserializeSnapshot(d.logger)
	if err != nil || captured == nil {
		t.Fatalf("deserialize daemon snapshot: snapshot=%v err=%v", captured, err)
	}
	ownerIndex := -1
	for i := range captured.Owners {
		if captured.Owners[i].ServerID == serverID {
			ownerIndex = i
			break
		}
	}
	if ownerIndex < 0 {
		t.Fatalf("daemon snapshot did not contain owner %q", serverID)
	}
	snap := captured.Owners[ownerIndex]
	if err := d.Remove(serverID); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 2 {
		seen[notifierReceive(t, handler.disconnected)] = true
	}
	if !seen[oldA] || !seen[oldB] {
		t.Fatalf("disconnect callbacks did not settle for both admissions: %v", seen)
	}
	d.mu.RLock()
	_, retainedA := d.sessionRoutes[oldA]
	_, retainedB := d.sessionRoutes[oldB]
	d.mu.RUnlock()
	if retainedA || retainedB {
		t.Fatal("removed owner retained session routes after callback settlement")
	}
	frame := notifierFrame("restored")
	for _, id := range []string{oldA, oldB} {
		if err := n.NotifySession(id, []byte(frame)); err == nil {
			t.Fatalf("predecessor admission %q remains routable", id)
		}
	}
	entry, reattached, err := d.restoreSnapshotPlan(d.makeSnapshotRestorePlan(snap), nil, "snapshot_fallback", false, false)
	if err != nil || reattached || entry == nil || entry.Owner == nil {
		t.Fatalf("snapshot fallback restore: entry=%v reattached=%v err=%v", entry, reattached, err)
	}
	if entry.Owner == predecessor || entry.ServerID != serverID {
		t.Fatal("snapshot did not replace the exact predecessor owner")
	}
	restored := notifierAttach(t, entry.Owner, cwd, nil, true)
	newID := notifierReceive(t, handler.connected).meta.SessionID
	if newID == "" || newID == oldA || newID == oldB {
		t.Fatal("snapshot restored a previous admission identity")
	}
	// A late predecessor removal callback must not revoke its successor.
	d.unregisterSessionRoute(predecessor, a.session)
	d.unregisterSessionRoute(predecessor, b.session)
	for _, id := range []string{oldA, oldB} {
		if err := n.NotifySession(id, []byte(frame)); err == nil {
			t.Fatalf("restore revived predecessor admission %q", id)
		}
	}
	if err := n.NotifySession(newID, []byte(frame)); err != nil {
		t.Fatalf("stable router cannot reach restored admission: %v", err)
	}
	notifierBarrier(t, n, []*notifierHost{restored}, map[string][]string{newID: {frame}})
	if err := restored.input.Close(); err != nil {
		t.Fatal(err)
	}
	if got := notifierReceive(t, handler.disconnected); got != newID {
		t.Fatalf("restored disconnect %q, want %q", got, newID)
	}
	d.mu.RLock()
	remaining := len(d.sessionRoutes)
	d.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("%d session routes remain after all disconnect callbacks", remaining)
	}
}

func TestSessionNotifierQueueFullPreservesAcceptedNotifications(t *testing.T) {
	d, handler := notifierNewDaemon(t)
	cwd := t.TempDir()
	_, o := notifierSpawn(t, d, "session-notifier-queue", cwd, "", nil)
	host := notifierAttach(t, o, cwd, nil, false)
	admission := notifierReceive(t, handler.connected)
	n := handler.router(t)
	// The first delivery blocks on the unread pipe; accepted frames are retained
	// in order and every rejected frame must leave that sequence unchanged.
	first := notifierFrame("queue-first")
	if err := n.NotifySession(admission.meta.SessionID, []byte(first)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.writeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("notification writer did not block")
	}
	accepted := []string{first}
	for i := range 1024 {
		frame := notifierFrame(fmt.Sprintf("queue-%d", i))
		if err := n.NotifySession(admission.meta.SessionID, []byte(frame)); err != nil {
			break
		}
		accepted = append(accepted, frame)
	}
	if len(accepted) == 1025 {
		t.Fatalf("queue did not reach a bounded full state: %d accepted", len(accepted))
	}
	if err := n.NotifySession(admission.meta.SessionID, []byte(notifierFrame("must-not-evict"))); err == nil {
		t.Fatal("full queue accepted another notification")
	}
	notifierStartReader(host)
	for _, want := range accepted {
		if got := notifierReceive(t, host.frames); got != want {
			t.Fatalf("queue delivered %s, want %s", got, want)
		}
	}
	marker := notifierFrame("drained")
	if err := n.NotifySession(admission.meta.SessionID, []byte(marker)); err != nil {
		t.Fatalf("drained queue: %v", err)
	}
	if got := notifierReceive(t, host.frames); got != marker {
		t.Fatalf("unexpected extra frame after drain: %s", got)
	}
}

func TestSessionNotifierRejectsModernAdmission(t *testing.T) {
	d, handler := notifierNewDaemon(t)
	cwd := t.TempDir()
	_, o := notifierSpawn(t, d, "session-notifier-modern", cwd, "2026-07-28", nil)
	host := notifierAttach(t, o, cwd, nil, true)
	admission := notifierReceive(t, handler.connected)
	n := handler.router(t)
	if err := n.NotifySession(admission.meta.SessionID, []byte(notifierFrame("modern"))); err == nil {
		t.Fatal("generic notification accepted for modern admission")
	}
	// A real response is an event-driven boundary proving this host is live.
	if _, err := fmt.Fprintln(host.input, `{"jsonrpc":"2.0","id":1,"method":"test/metadata","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`); err != nil {
		t.Fatal(err)
	}
	if got := notifierReceive(t, handler.requests); got.meta.SessionID != admission.meta.SessionID {
		t.Fatal("modern request metadata changed")
	}
	if got := notifierReceive(t, host.frames); got != `{"jsonrpc":"2.0","id":1,"result":"barrier"}` {
		t.Fatalf("modern host received unexpected frame: %s", got)
	}
}
