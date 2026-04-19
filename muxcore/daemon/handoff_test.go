package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// mockFDConn implements fdConn using in-memory channels for unit testing.
// Two instances are wired as a connected pair via newMockFDConnPair.
type mockFDConn struct {
	writeCh  chan []byte   // JSON messages written by this conn arrive here
	readCh   chan []byte   // JSON messages read by this conn come from here
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

func (m *mockFDConn) Close() error { return nil }

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
		{ServerID: "s1", Command: "cmd1", PID: 100, StdinFD: 111, StdoutFD: 222},
		{ServerID: "s2", Command: "cmd2", PID: 200, StdinFD: 333, StdoutFD: 444},
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
		if hu.StdinFD != orig.StdinFD || hu.StdoutFD != orig.StdoutFD {
			t.Errorf("server %s: FD mismatch: got stdin=%d stdout=%d, want stdin=%d stdout=%d",
				hu.ServerID, hu.StdinFD, hu.StdoutFD, orig.StdinFD, orig.StdoutFD)
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
		bad := HelloMsg{Type: MsgHello, ProtocolVersion: 2, Token: "secret"}
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
