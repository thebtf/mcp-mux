package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
	if err := os.MkdirAll(filepath.Dir(enginePath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(enginePath, []byte("engine"), 0755); err != nil {
		t.Fatalf("WriteFile engine: %v", err)
	}
	pointerPath := filepath.Join(dir, "active.txt")
	rel, err := filepath.Rel(filepath.Dir(pointerPath), enginePath)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if err := os.WriteFile(pointerPath, []byte(rel+"\n"), 0644); err != nil {
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
	schema handoffHandleSchema
}

func (c *scriptedHandoffConn) WriteJSON(any) error { return nil }

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

func TestPerformHandoffVersionSkewRejectsBeforeLeaseSettlement(t *testing.T) {
	var settled int
	conn := &scriptedHandoffConn{
		schema: handoffHandleSchema{count: 3},
		reads: []scriptedHandoffRead{{
			msg: HelloMsg{Type: MsgHello, ProtocolVersion: HandoffProtocolVersion - 1, Token: "secret"},
		}},
	}
	upstreams := []HandoffUpstream{{
		ServerID: "s1", PID: 1, StdinFD: 11, StdoutFD: 12, StderrFD: 13,
		commit: func() error { settled++; return nil },
		abort:  func() error { settled++; return nil },
	}}

	if _, err := performHandoff(context.Background(), conn, "secret", upstreams); !errors.Is(err, ErrProtocolVersionMismatch) {
		t.Fatalf("performHandoff error=%v, want ErrProtocolVersionMismatch", err)
	}
	if settled != 0 {
		t.Fatalf("lease settled %d times before Hello version acceptance", settled)
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
