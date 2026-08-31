package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

// mockFDConn implements fdConn using in-memory channels for unit testing.
// Two instances are wired as a connected pair via newMockFDConnPair.
type mockFDConn struct {
	writeCh  chan []byte    // JSON messages written by this conn arrive here
	readCh   chan []byte    // JSON messages read by this conn come from here
	sendFdCh chan []uintptr // FDs sent by this conn arrive here
	recvFdCh chan []uintptr // FDs received by this conn come from here
}

func (m *mockFDConn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.writeCh <- b
	return nil
}

func (m *mockFDConn) ReadJSON(v any) error {
	b := <-m.readCh
	return json.Unmarshal(b, v)
}

func (m *mockFDConn) SendFDs(fds []uintptr, header []byte) error {
	m.sendFdCh <- fds
	return nil
}

func (m *mockFDConn) RecvFDs() ([]uintptr, []byte, error) {
	fds := <-m.recvFdCh
	return fds, nil, nil
}

func (m *mockFDConn) handoffSchema() handoffHandleSchema { return handoffHandleSchema{count: 3} }
func (m *mockFDConn) closeReceivedHandles([]uintptr)     {}
func (m *mockFDConn) Close() error                       { return nil }

// newMockFDConnPair returns two connected mockFDConn instances.
// a.WriteJSON delivers to b.ReadJSON, and b.WriteJSON delivers to a.ReadJSON.
// a.SendFDs delivers to b.RecvFDs, and b.SendFDs delivers to a.RecvFDs.
func newMockFDConnPair() (*mockFDConn, *mockFDConn) {
	aTob := make(chan []byte, 16)
	bToa := make(chan []byte, 16)
	aFdTob := make(chan []uintptr, 16)
	bFdToa := make(chan []uintptr, 16)
	a := &mockFDConn{writeCh: aTob, readCh: bToa, sendFdCh: aFdTob, recvFdCh: bFdToa}
	b := &mockFDConn{writeCh: bToa, readCh: aTob, sendFdCh: bFdToa, recvFdCh: aFdTob}
	return a, b
}

// TestHandoffHappyPath verifies the full hello/ready/transfer sequence
// with two upstreams and stub FDs. Both sides must agree on the outcome.
func TestHandoffHappyPath(t *testing.T) {
	ctx := context.Background()
	upstreams := []HandoffUpstream{
		{ServerID: "s1", Command: "cmd1", PID: 100, StdinFD: 111, StdoutFD: 222, StderrFD: 223},
		{ServerID: "s2", Command: "cmd2", PID: 200, StdinFD: 333, StdoutFD: 444, StderrFD: 445},
	}
	performConn, receiveConn := newMockFDConnPair()

	type performResult struct {
		r   HandoffResult
		err error
	}
	type receiveResult struct {
		r   []HandoffUpstream
		err error
	}

	perfCh := make(chan performResult, 1)
	recvCh := make(chan receiveResult, 1)

	go func() {
		r, err := performHandoff(ctx, performConn, "secret", upstreams)
		perfCh <- performResult{r, err}
	}()
	go func() {
		r, err := receiveHandoff(ctx, receiveConn, "secret")
		recvCh <- receiveResult{r, err}
	}()

	pr := <-perfCh
	rr := <-recvCh

	if pr.err != nil {
		t.Fatalf("performHandoff error: %v", pr.err)
	}
	if rr.err != nil {
		t.Fatalf("receiveHandoff error: %v", rr.err)
	}
	if len(pr.r.Transferred) != 2 {
		t.Errorf("expected 2 transferred, got %v", pr.r.Transferred)
	}
	if len(pr.r.Aborted) != 0 {
		t.Errorf("expected 0 aborted, got %v", pr.r.Aborted)
	}
	if len(rr.r) != 2 {
		t.Fatalf("expected 2 received upstreams, got %d", len(rr.r))
	}
	// Verify FD metadata propagated through mock channel correctly.
	for _, hu := range rr.r {
		var orig HandoffUpstream
		for _, u := range upstreams {
			if u.ServerID == hu.ServerID {
				orig = u
				break
			}
		}
		if hu.StdinFD != orig.StdinFD || hu.StdoutFD != orig.StdoutFD || hu.StderrFD != orig.StderrFD || hu.AuthorityFD != orig.AuthorityFD {
			t.Errorf("server %s: FD mismatch: got stdin=%d stdout=%d stderr=%d authority=%d, want stdin=%d stdout=%d stderr=%d authority=%d",
				hu.ServerID, hu.StdinFD, hu.StdoutFD, hu.StderrFD, hu.AuthorityFD,
				orig.StdinFD, orig.StdoutFD, orig.StderrFD, orig.AuthorityFD)
		}
	}
}

// TestHandoffVersionReject verifies that performHandoff rejects a Hello
// message carrying an unknown protocol version.
func TestHandoffVersionReject(t *testing.T) {
	ctx := context.Background()
	performConn, receiveConn := newMockFDConnPair()

	// Successor sends Hello with wrong protocol version.
	go func() {
		bad := HelloMsg{Type: MsgHello, ProtocolVersion: HandoffProtocolVersion + 1, Token: "secret"}
		_ = receiveConn.WriteJSON(bad)
	}()

	_, err := performHandoff(ctx, performConn, "secret", nil)
	if err == nil {
		t.Fatal("expected error on version mismatch, got nil")
	}
	if !errors.Is(err, ErrProtocolVersionMismatch) {
		t.Errorf("expected ErrProtocolVersionMismatch, got: %v", err)
	}
}

// TestHandoffTokenReject verifies that performHandoff returns ErrTokenMismatch
// when the successor presents a wrong authentication token.
func TestHandoffTokenReject(t *testing.T) {
	ctx := context.Background()
	performConn, receiveConn := newMockFDConnPair()

	// Successor sends Hello with correct version but wrong token.
	go func() {
		bad := HelloMsg{Type: MsgHello, ProtocolVersion: HandoffProtocolVersion, Token: "wrong-token"}
		_ = receiveConn.WriteJSON(bad)
	}()

	_, err := performHandoff(ctx, performConn, "secret", nil)
	if err == nil {
		t.Fatal("expected error on token mismatch, got nil")
	}
	if !errors.Is(err, ErrTokenMismatch) {
		t.Errorf("expected ErrTokenMismatch, got: %v", err)
	}
}

func TestRetireOldOwnerSocketsRemovesPathsAndCounts(t *testing.T) {
	dir := t.TempDir()
	ipc, err := os.CreateTemp(dir, "owner-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp ipc: %v", err)
	}
	if err := ipc.Close(); err != nil {
		t.Fatalf("Close ipc: %v", err)
	}
	control, err := os.CreateTemp(dir, "owner-*.ctl.sock")
	if err != nil {
		t.Fatalf("CreateTemp control: %v", err)
	}
	if err := control.Close(); err != nil {
		t.Fatalf("Close control: %v", err)
	}

	d := &Daemon{}
	if !d.retireOldOwnerSockets(ipc.Name(), control.Name()) {
		t.Fatal("retireOldOwnerSockets returned false, want true")
	}
	if got := d.oldOwnerSocketRetiredCount.Load(); got != 1 {
		t.Fatalf("oldOwnerSocketRetiredCount = %d, want 1", got)
	}
	for _, path := range []string{ipc.Name(), control.Name()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("old owner socket path %s still reachable: %v", path, err)
		}
	}
}

func TestSuccessorExecutableUsesActiveEnginePointer(t *testing.T) {
	dir := t.TempDir()
	enginePath := filepath.Join(dir, "versions", "abc123", "mcp-mux-engine.exe")
	if err := os.MkdirAll(filepath.Dir(enginePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(enginePath, []byte("engine"), 0o755); err != nil {
		t.Fatalf("WriteFile engine: %v", err)
	}
	pointerPath := filepath.Join(dir, "active.txt")
	rel, err := filepath.Rel(filepath.Dir(pointerPath), enginePath)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if err := os.WriteFile(pointerPath, []byte(rel+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile pointer: %v", err)
	}

	t.Setenv("MCPMUX_SUCCESSOR_EXE", "")
	t.Setenv("MCPMUX_ACTIVE_ENGINE_FILE", pointerPath)
	got, err := successorExecutable()
	if err != nil {
		t.Fatalf("successorExecutable() error = %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(enginePath) {
		t.Fatalf("successorExecutable() = %q, want %q", got, enginePath)
	}
}

func TestSuccessorExecutableExplicitOverrideWins(t *testing.T) {
	t.Setenv("MCPMUX_ACTIVE_ENGINE_FILE", filepath.Join(t.TempDir(), "missing-active.txt"))
	want := filepath.Join(t.TempDir(), "explicit-engine.exe")
	t.Setenv("MCPMUX_SUCCESSOR_EXE", want)

	got, err := successorExecutable()
	if err != nil {
		t.Fatalf("successorExecutable() error = %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("successorExecutable() = %q, want %q", got, want)
	}
}

func TestSuccessorExecutableRequestOverrideWins(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "env-engine.exe")
	requestPath := filepath.Join(t.TempDir(), "request-engine.exe")
	t.Setenv("MCPMUX_SUCCESSOR_EXE", envPath)

	got, err := successorExecutableFor(requestPath)
	if err != nil {
		t.Fatalf("successorExecutableFor() error = %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(requestPath) {
		t.Fatalf("successorExecutableFor() = %q, want request override %q", got, requestPath)
	}
}

type scriptedHandoffRead struct {
	msg any
	err error
}

type scriptedHandoffConn struct {
	reads  []scriptedHandoffRead
	readAt int
	fds    [][]uintptr
	fdAt   int
	closed []uintptr
	writes []any
	schema handoffHandleSchema
}

func (c *scriptedHandoffConn) WriteJSON(v any) error {
	c.writes = append(c.writes, v)
	return nil
}

func (c *scriptedHandoffConn) ReadJSON(v any) error {
	if c.readAt >= len(c.reads) {
		return io.EOF
	}
	step := c.reads[c.readAt]
	c.readAt++
	if step.err != nil {
		return step.err
	}
	data, err := json.Marshal(step.msg)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (c *scriptedHandoffConn) SendFDs([]uintptr, []byte) error { return nil }

func (c *scriptedHandoffConn) RecvFDs() ([]uintptr, []byte, error) {
	if c.fdAt >= len(c.fds) {
		return nil, nil, io.EOF
	}
	fds := c.fds[c.fdAt]
	c.fdAt++
	return fds, nil, nil
}

func (c *scriptedHandoffConn) handoffSchema() handoffHandleSchema {
	return c.schema
}

func (c *scriptedHandoffConn) closeReceivedHandles(fds []uintptr) {
	c.closed = append(c.closed, fds...)
}

func (c *scriptedHandoffConn) Close() error { return nil }

type finalAckFailHandoffConn struct {
	*scriptedHandoffConn
}

func (c *finalAckFailHandoffConn) WriteJSON(v any) error {
	if msg, ok := v.(HandoffAckMsg); ok && msg.Type == MsgHandoffAck {
		return errors.New("injected final ack failure")
	}
	return nil
}

type stallingHandoffConn struct {
	*scriptedHandoffConn
	operation string
	at        int
	reads     int
	writes    int
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *stallingHandoffConn) WriteJSON(v any) error {
	at := c.writes
	c.writes++
	if c.operation == "write" && at == c.at {
		<-c.closed
		return io.ErrClosedPipe
	}
	return c.scriptedHandoffConn.WriteJSON(v)
}

func (c *stallingHandoffConn) ReadJSON(v any) error {
	at := c.reads
	c.reads++
	if c.operation == "read" && at == c.at {
		<-c.closed
		return io.ErrClosedPipe
	}
	return c.scriptedHandoffConn.ReadJSON(v)
}

func (c *stallingHandoffConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestPerformHandoffTotalDeadlineBoundsEveryPhase(t *testing.T) {
	upstream := HandoffUpstream{ServerID: "s1", PID: 1, StdinFD: 11, StdoutFD: 12, StderrFD: 13}
	tests := []struct {
		name      string
		operation string
		at        int
		reads     []scriptedHandoffRead
	}{
		{name: "hello", operation: "read", at: 0},
		{name: "ready", operation: "write", at: 0, reads: []scriptedHandoffRead{{msg: NewHelloMsg("secret")}}},
		{name: "transfer", operation: "write", at: 1, reads: []scriptedHandoffRead{{msg: NewHelloMsg("secret")}}},
		{name: "done", operation: "write", at: 2, reads: []scriptedHandoffRead{{msg: NewHelloMsg("secret")}, {msg: NewAckTransferMsg("s1", true, nil)}}},
		{name: "final-ack", operation: "read", at: 2, reads: []scriptedHandoffRead{{msg: NewHelloMsg("secret")}, {msg: NewAckTransferMsg("s1", true, nil)}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &stallingHandoffConn{
				scriptedHandoffConn: &scriptedHandoffConn{schema: handoffHandleSchema{count: 3}, reads: tt.reads},
				operation:           tt.operation,
				at:                  tt.at,
				closed:              make(chan struct{}),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()

			done := make(chan error, 1)
			started := time.Now()
			go func() {
				_, err := performHandoff(ctx, conn, "secret", []HandoffUpstream{upstream})
				done <- err
			}()

			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("performHandoff() error = %v, want context deadline exceeded", err)
				}
				if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
					t.Fatalf("performHandoff() took %s, want total deadline bound", elapsed)
				}
			case <-time.After(200 * time.Millisecond):
				_ = conn.Close()
				<-done
				t.Fatal("performHandoff() ignored total deadline")
			}
		})
	}
}

func TestPerformHandoffFinalAckControlsLeaseSettlement(t *testing.T) {
	var committed, aborted int
	upstreams := []HandoffUpstream{
		{ServerID: "s1", PID: 1, StdinFD: 11, StdoutFD: 12, StderrFD: 13, commit: func() error { committed++; return nil }, abort: func() error { aborted++; return nil }},
		{ServerID: "s2", PID: 2, StdinFD: 21, StdoutFD: 22, StderrFD: 23, commit: func() error { committed++; return nil }, abort: func() error { aborted++; return nil }},
	}
	conn := &scriptedHandoffConn{
		schema: handoffHandleSchema{count: 3},
		reads: []scriptedHandoffRead{
			{msg: NewHelloMsg("secret")},
			{msg: NewAckTransferMsg("s1", true, nil)},
			{msg: NewAckTransferMsg("s2", true, nil)},
			{msg: NewHandoffAckResult([]string{"s1"}, []string{"s2"})},
		},
	}

	result, err := performHandoff(context.Background(), conn, "secret", upstreams)
	if err != nil {
		t.Fatalf("performHandoff: %v", err)
	}
	if committed != 1 || aborted != 1 {
		t.Fatalf("lease settlement: committed=%d aborted=%d, want 1/1", committed, aborted)
	}
	if len(result.Transferred) != 1 || result.Transferred[0] != "s1" {
		t.Fatalf("Transferred=%v, want [s1]", result.Transferred)
	}
	if len(result.Aborted) != 1 || result.Aborted[0] != "s2" {
		t.Fatalf("Aborted=%v, want [s2]", result.Aborted)
	}
}

func TestPerformHandoffFinalAckFailureAbortsEveryPreparedTree(t *testing.T) {
	var committed, aborted int
	upstreams := []HandoffUpstream{{
		ServerID: "s1", PID: 1, StdinFD: 11, StdoutFD: 12, StderrFD: 13,
		commit: func() error { committed++; return nil },
		abort:  func() error { aborted++; return nil },
	}}
	conn := &scriptedHandoffConn{
		schema: handoffHandleSchema{count: 3},
		reads: []scriptedHandoffRead{
			{msg: NewHelloMsg("secret")},
			{msg: NewAckTransferMsg("s1", true, nil)},
			{err: io.ErrUnexpectedEOF},
		},
	}

	if _, err := performHandoff(context.Background(), conn, "secret", upstreams); err == nil {
		t.Fatal("performHandoff error=nil, want final ACK failure")
	}
	if committed != 0 || aborted != 1 {
		t.Fatalf("lease settlement: committed=%d aborted=%d, want 0/1", committed, aborted)
	}
}

func TestPerformHandoffHelloRejectsBeforeLeaseSettlement(t *testing.T) {
	tests := []struct {
		name  string
		hello HelloMsg
		want  error
	}{
		{name: "version skew", hello: HelloMsg{Type: MsgHello, ProtocolVersion: HandoffProtocolVersion - 1, Token: "secret"}, want: ErrProtocolVersionMismatch},
		{name: "token mismatch", hello: HelloMsg{Type: MsgHello, ProtocolVersion: HandoffProtocolVersion, Token: "wrong"}, want: ErrTokenMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var settled int
			conn := &scriptedHandoffConn{
				schema: handoffHandleSchema{count: 3},
				reads:  []scriptedHandoffRead{{msg: tt.hello}},
			}
			upstreams := []HandoffUpstream{{
				ServerID: "s1", PID: 1, StdinFD: 11, StdoutFD: 12, StderrFD: 13,
				commit: func() error { settled++; return nil },
				abort:  func() error { settled++; return nil },
			}}

			if _, err := performHandoff(context.Background(), conn, "secret", upstreams); !errors.Is(err, tt.want) {
				t.Fatalf("performHandoff error=%v, want %v", err, tt.want)
			}
			if settled != 0 {
				t.Fatalf("lease settled %d times before Hello acceptance", settled)
			}
		})
	}
}

func TestPrepareHandoffReceiveClosesMalformedHandleBatch(t *testing.T) {
	conn := &scriptedHandoffConn{
		schema: handoffHandleSchema{count: 3},
		reads: []scriptedHandoffRead{
			{msg: NewReadyMsg([]UpstreamRef{{ServerID: "s1", Command: "cmd", PID: 1}})},
			{msg: NewFdTransferMsgWithStderr("s1", HandleMeta{Kind: "stdin"}, HandleMeta{Kind: "stdout"}, HandleMeta{Kind: "stderr"})},
			{msg: NewDoneMsg(nil, []string{"s1"})},
		},
		fds: [][]uintptr{{11, 12, 13, 14}},
	}

	receipt, err := prepareHandoffReceive(context.Background(), conn, "secret")
	if err != nil {
		t.Fatalf("prepareHandoffReceive: %v", err)
	}
	if err := receipt.finalize(nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if len(conn.closed) != 4 {
		t.Fatalf("closed handles=%v, want all malformed handles", conn.closed)
	}
}

func TestHandoffReceiptFinalizeClosesUnadoptedHandles(t *testing.T) {
	conn := &scriptedHandoffConn{schema: handoffHandleSchema{count: 3}}
	receipt := &handoffReceipt{
		conn:     conn,
		order:    []string{"unmatched"},
		received: map[string]HandoffUpstream{"unmatched": {ServerID: "unmatched", StdinFD: 31, StdoutFD: 32, StderrFD: 33}},
		owned:    map[string]bool{"unmatched": true},
		taken:    make(map[string]bool),
		rejected: make(map[string]struct{}),
	}
	if err := receipt.finalize(nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if len(conn.closed) != 3 || conn.closed[0] != 31 || conn.closed[1] != 32 || conn.closed[2] != 33 {
		t.Fatalf("closed handles=%v, want [31 32 33]", conn.closed)
	}
}

func TestReceiveHandoff_FinalAckFailureClosesTakenStderrHandle(t *testing.T) {
	base := &scriptedHandoffConn{
		schema: handoffHandleSchema{count: 3},
		reads: []scriptedHandoffRead{
			{msg: NewReadyMsg([]UpstreamRef{{ServerID: "s1", Command: "cmd", PID: 101}})},
			{msg: NewFdTransferMsgWithStderr("s1", HandleMeta{Kind: "stdin"}, HandleMeta{Kind: "stdout"}, HandleMeta{Kind: "stderr"})},
			{msg: NewDoneMsg([]string{"s1"}, nil)},
		},
		fds: [][]uintptr{{31, 32, 33}},
	}
	conn := &finalAckFailHandoffConn{scriptedHandoffConn: base}

	if _, err := receiveHandoff(context.Background(), conn, "secret"); err == nil {
		t.Fatal("receiveHandoff error=nil, want final ack failure")
	}
	if len(base.closed) != 3 || base.closed[0] != 31 || base.closed[1] != 32 || base.closed[2] != 33 {
		t.Fatalf("closed handles=%v, want stdin/stdout/stderr [31 32 33]", base.closed)
	}
}

func spawnHandoffProcessOwner(t *testing.T, d *Daemon, protocolEra string) *OwnerEntry {
	t.Helper()
	_, serverID, _, err := d.Spawn(control.Request{
		Cmd:         "spawn",
		Command:     "go",
		Args:        []string{"run", "../../testdata/mock_server.go"},
		Cwd:         ".",
		Mode:        "global",
		ProtocolEra: protocolEra,
	})
	if err != nil {
		t.Fatalf("Spawn() process-backed owner: %v", err)
	}

	var entry *OwnerEntry
	waitForDaemonCondition(t, 5*time.Second, func() bool {
		entry = d.Entry(serverID)
		return entry != nil && entry.Owner != nil && entry.Owner.HasHandoffUpstream() && entry.Owner.IsAccepting() && entry.Owner.MaterializationState() == owner.MaterializationReady
	}, "process-backed owner did not become handoff-ready")
	return entry
}

func TestModernOnlyDaemonHasNoHandoffEligibleUpstream(t *testing.T) {
	d := testDaemon(t)
	entry := spawnHandoffProcessOwner(t, d, "2026-07-28")

	if got := entry.ProtocolEra; got != era.EraModern20260728 {
		t.Fatalf("ProtocolEra = %v, want modern 2026-07-28", got)
	}
	if !strings.HasPrefix(entry.ServerID, "native-") {
		t.Fatalf("modern ServerID = %q, want native-<hex>", entry.ServerID)
	}
	beforePID := entry.Owner.Status()["upstream_pid"]

	if d.hasHandoffUpstreamOwners() {
		t.Fatal("modern-only daemon reports a handoff-eligible upstream")
	}
	if d.Entry(entry.ServerID) != entry {
		t.Fatal("modern owner entry changed while checking handoff eligibility")
	}
	if !entry.Owner.IsAccepting() {
		t.Fatal("modern owner listener stopped while checking handoff eligibility")
	}
	if !entry.Owner.HasHandoffUpstream() {
		t.Fatal("modern owner process detached while checking handoff eligibility")
	}
	if got := entry.Owner.Status()["upstream_pid"]; got != beforePID {
		t.Fatalf("modern upstream PID changed from %v to %v", beforePID, got)
	}
	select {
	case <-entry.Owner.Done():
		t.Fatal("modern owner completed while checking handoff eligibility")
	default:
	}
}

func TestCollectHandoffUpstreamsLeavesModernOwnerUntouched(t *testing.T) {
	d := testDaemon(t)
	legacy := spawnHandoffProcessOwner(t, d, "")
	modern := spawnHandoffProcessOwner(t, d, "2026-07-28")

	if got := legacy.ProtocolEra; got != era.EraLegacy {
		t.Fatalf("legacy ProtocolEra = %v, want legacy", got)
	}
	if got := modern.ProtocolEra; got != era.EraModern20260728 {
		t.Fatalf("modern ProtocolEra = %v, want modern 2026-07-28", got)
	}
	beforePID := modern.Owner.Status()["upstream_pid"]
	beforeState := modern.Owner.MaterializationState()

	upstreams := d.collectHandoffUpstreams()
	t.Cleanup(func() {
		if err := settleHandoffUpstreams(upstreams, nil); err != nil {
			t.Errorf("abort prepared legacy handoff lease: %v", err)
		}
	})

	var legacyPrepared, modernPrepared bool
	for _, upstream := range upstreams {
		switch upstream.ServerID {
		case legacy.ServerID:
			legacyPrepared = true
		case modern.ServerID:
			modernPrepared = true
		default:
			t.Errorf("prepared unexpected handoff upstream %q", upstream.ServerID)
		}
	}
	if !legacyPrepared {
		t.Errorf("legacy owner %q was not prepared for handoff", legacy.ServerID)
	}
	if modernPrepared {
		t.Errorf("modern owner %q was prepared for legacy handoff", modern.ServerID)
	}
	if got, want := len(upstreams), 1; got != want {
		t.Errorf("prepared handoff upstreams = %d, want %d legacy-only", got, want)
	}
	if d.Entry(modern.ServerID) != modern {
		t.Error("modern owner entry changed during legacy handoff collection")
	}
	if !modern.Owner.IsAccepting() {
		t.Error("modern owner listener changed during legacy handoff collection")
	}
	if !modern.Owner.HasHandoffUpstream() {
		t.Error("modern owner process detached during legacy handoff collection")
	}
	if got := modern.Owner.Status()["upstream_pid"]; got != beforePID {
		t.Errorf("modern upstream PID changed from %v to %v", beforePID, got)
	}
	if got := modern.Owner.MaterializationState(); got != beforeState {
		t.Errorf("modern materialization state changed from %s to %s", beforeState, got)
	}
	select {
	case <-modern.Owner.Done():
		t.Error("modern owner completed during legacy handoff collection")
	default:
	}
}

func TestPerformHandoffQuarantinesNativeUpstreamBeforeFDTransfer(t *testing.T) {
	const nativeID = "native-a1b2c3d4e5f60708"

	var committed, aborted int
	upstream := HandoffUpstream{
		ServerID: nativeID,
		Command:  "native-command",
		PID:      101,
		StdinFD:  11,
		StdoutFD: 12,
		StderrFD: 13,
		commit:   func() error { committed++; return nil },
		abort:    func() error { aborted++; return nil },
	}
	oldConn, successorConn := newMockFDConnPair()
	if err := successorConn.WriteJSON(NewHelloMsg("secret")); err != nil {
		t.Fatalf("send successor hello: %v", err)
	}

	type peerResult struct {
		ready     ReadyMsg
		transfers int
		done      DoneMsg
		err       error
	}
	peerDone := make(chan peerResult, 1)
	go func() {
		result := peerResult{}
		defer func() { peerDone <- result }()

		if err := successorConn.ReadJSON(&result.ready); err != nil {
			result.err = err
			return
		}
		accepted := make([]string, 0, len(result.ready.Upstreams))
		for _, ref := range result.ready.Upstreams {
			var transfer FdTransferMsg
			if err := successorConn.ReadJSON(&transfer); err != nil {
				result.err = err
				return
			}
			fds, _, err := successorConn.RecvFDs()
			if err != nil {
				result.err = err
				return
			}
			if transfer.ServerID != ref.ServerID || len(fds) != 3 {
				result.err = errors.New("successor received invalid native transfer")
				return
			}
			result.transfers++
			accepted = append(accepted, ref.ServerID)
			if err := successorConn.WriteJSON(NewAckTransferMsg(ref.ServerID, true, nil)); err != nil {
				result.err = err
				return
			}
		}
		if err := successorConn.ReadJSON(&result.done); err != nil {
			result.err = err
			return
		}
		if err := successorConn.WriteJSON(NewHandoffAckResult(accepted, nil)); err != nil {
			result.err = err
		}
	}()

	result, err := performHandoff(context.Background(), oldConn, "secret", []HandoffUpstream{upstream})
	if err != nil {
		t.Fatalf("performHandoff: %v", err)
	}
	peer := <-peerDone
	if peer.err != nil {
		t.Fatalf("successor peer: %v", peer.err)
	}
	if got := len(peer.ready.Upstreams); got != 0 {
		t.Errorf("ready announced %d native upstreams, want none", got)
	}
	if peer.transfers != 0 {
		t.Errorf("native upstream transferred %d handle batches, want none", peer.transfers)
	}
	if len(peer.done.Transferred) != 0 || len(peer.done.Aborted) != 0 {
		t.Errorf("done partition = transferred %v aborted %v, want quarantined native ID absent from wire", peer.done.Transferred, peer.done.Aborted)
	}
	if len(result.Transferred) != 0 || len(result.Aborted) != 1 || result.Aborted[0] != nativeID {
		t.Errorf("handoff result = transferred %v aborted %v, want native aborted", result.Transferred, result.Aborted)
	}
	if committed != 0 || aborted != 1 {
		t.Errorf("native lease settlement = committed %d aborted %d, want 0/1", committed, aborted)
	}
}

func TestPerformHandoffNativeQuarantineRemainsReceiptCompatible(t *testing.T) {
	const nativeID = "native-a1b2c3d4e5f60708"
	var committed, aborted int
	upstream := HandoffUpstream{
		ServerID: nativeID,
		Command:  "native-command",
		PID:      101,
		StdinFD:  11,
		StdoutFD: 12,
		StderrFD: 13,
		commit:   func() error { committed++; return nil },
		abort:    func() error { aborted++; return nil },
	}
	oldConn, successorConn := newMockFDConnPair()
	type performResult struct {
		result HandoffResult
		err    error
	}
	performDone := make(chan performResult, 1)
	receiveDone := make(chan struct {
		upstreams []HandoffUpstream
		err       error
	}, 1)
	go func() {
		result, err := performHandoff(context.Background(), oldConn, "secret", []HandoffUpstream{upstream})
		performDone <- performResult{result: result, err: err}
	}()
	go func() {
		upstreams, err := receiveHandoff(context.Background(), successorConn, "secret")
		receiveDone <- struct {
			upstreams []HandoffUpstream
			err       error
		}{upstreams: upstreams, err: err}
	}()

	performed := <-performDone
	received := <-receiveDone
	if performed.err != nil {
		t.Fatalf("performHandoff: %v", performed.err)
	}
	if received.err != nil {
		t.Fatalf("receiveHandoff: %v", received.err)
	}
	if len(received.upstreams) != 0 {
		t.Fatalf("successor received quarantined native upstreams: %+v", received.upstreams)
	}
	if len(performed.result.Transferred) != 0 || len(performed.result.Aborted) != 1 || performed.result.Aborted[0] != nativeID {
		t.Fatalf("handoff result = transferred %v aborted %v, want native aborted locally", performed.result.Transferred, performed.result.Aborted)
	}
	if committed != 0 || aborted != 1 {
		t.Fatalf("native lease settlement = committed %d aborted %d, want 0/1", committed, aborted)
	}
}

func TestPrepareHandoffReceiveQuarantinesNativeReady(t *testing.T) {
	const nativeID = "native-a1b2c3d4e5f60708"

	t.Run("refuses before handle receipt", func(t *testing.T) {
		conn := &scriptedHandoffConn{
			schema: handoffHandleSchema{count: 3},
			reads: []scriptedHandoffRead{
				{msg: NewReadyMsg([]UpstreamRef{{ServerID: nativeID, Command: "native-command", PID: 101}})},
				{msg: NewFdTransferMsgWithStderr(nativeID, HandleMeta{Kind: "stdin"}, HandleMeta{Kind: "stdout"}, HandleMeta{Kind: "stderr"})},
				{msg: NewDoneMsg([]string{nativeID}, nil)},
			},
			fds: [][]uintptr{{31, 32, 33}},
		}

		receipt, err := prepareHandoffReceive(context.Background(), conn, "secret")
		if receipt != nil {
			t.Cleanup(receipt.abort)
		}
		if err == nil {
			t.Error("prepareHandoffReceive accepted native Ready, want quarantine failure")
		}
		if conn.readAt != 1 {
			t.Errorf("native Ready consumed %d protocol messages, want Ready only", conn.readAt)
		}
		if conn.fdAt != 0 {
			t.Errorf("native Ready received %d handle batches, want none", conn.fdAt)
		}
		if receipt != nil && (len(receipt.received) != 0 || len(receipt.owned) != 0 || len(receipt.taken) != 0) {
			t.Errorf("native Ready created receipt state: received=%v owned=%v taken=%v", receipt.received, receipt.owned, receipt.taken)
		}
		for _, write := range conn.writes {
			if ack, ok := write.(AckTransferMsg); ok && ack.Type == MsgAckTransfer {
				t.Errorf("native Ready emitted receipt ack for %q", ack.ServerID)
			}
		}
	})

	t.Run("legacy final partition remains valid", func(t *testing.T) {
		const legacyID = "legacy-handoff"
		conn := &scriptedHandoffConn{
			schema: handoffHandleSchema{count: 3},
			reads: []scriptedHandoffRead{
				{msg: NewReadyMsg([]UpstreamRef{{ServerID: legacyID, Command: "legacy-command", PID: 102}})},
				{msg: NewFdTransferMsgWithStderr(legacyID, HandleMeta{Kind: "stdin"}, HandleMeta{Kind: "stdout"}, HandleMeta{Kind: "stderr"})},
				{msg: NewDoneMsg([]string{legacyID}, nil)},
			},
			fds: [][]uintptr{{41, 42, 43}},
		}

		receipt, err := prepareHandoffReceive(context.Background(), conn, "secret")
		if err != nil {
			t.Fatalf("prepareHandoffReceive legacy: %v", err)
		}
		t.Cleanup(receipt.abort)
		upstream, ok := receipt.take(legacyID)
		if !ok || upstream.ServerID != legacyID {
			t.Fatalf("take legacy upstream = %+v, %t", upstream, ok)
		}
		if err := receipt.finalize([]string{legacyID}); err != nil {
			t.Fatalf("finalize legacy receipt: %v", err)
		}
		if conn.fdAt != 1 {
			t.Errorf("legacy Ready received %d handle batches, want 1", conn.fdAt)
		}
		if len(conn.closed) != 0 {
			t.Errorf("legacy adopted handles were closed: %v", conn.closed)
		}
		if len(conn.writes) == 0 {
			t.Fatal("legacy receipt did not write a final handoff acknowledgment")
		}
		ack, ok := conn.writes[len(conn.writes)-1].(HandoffAckMsg)
		if !ok {
			t.Fatalf("legacy final message = %T, want HandoffAckMsg", conn.writes[len(conn.writes)-1])
		}
		if len(ack.Accepted) != 1 || ack.Accepted[0] != legacyID || len(ack.Aborted) != 0 {
			t.Errorf("legacy final partition = accepted %v aborted %v, want legacy accepted", ack.Accepted, ack.Aborted)
		}
	})
}
