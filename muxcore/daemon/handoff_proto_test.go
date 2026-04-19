package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// TestHelloMsgRoundtrip verifies that a HelloMsg marshals and unmarshals
// correctly, preserving all fields including Type and ProtocolVersion.
// If NewHelloMsg returns a zero value, Type and ProtocolVersion will be
// empty/zero and this test will fail — satisfying the anti-stub requirement.
func TestHelloMsgRoundtrip(t *testing.T) {
	orig := NewHelloMsg("deadbeef1234abcd")
	if orig.Type != MsgHello {
		t.Fatalf("NewHelloMsg: Type = %q, want %q", orig.Type, MsgHello)
	}
	if orig.ProtocolVersion != HandoffProtocolVersion {
		t.Fatalf("NewHelloMsg: ProtocolVersion = %d, want %d", orig.ProtocolVersion, HandoffProtocolVersion)
	}

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal HelloMsg: %v", err)
	}

	var got HelloMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal HelloMsg: %v", err)
	}
	if got.Type != orig.Type {
		t.Errorf("Type: got %q, want %q", got.Type, orig.Type)
	}
	if got.ProtocolVersion != orig.ProtocolVersion {
		t.Errorf("ProtocolVersion: got %d, want %d", got.ProtocolVersion, orig.ProtocolVersion)
	}
	if got.Token != orig.Token {
		t.Errorf("Token: got %q, want %q", got.Token, orig.Token)
	}
}

// TestReadyMsgRoundtrip verifies ReadyMsg round-trips including the Upstreams slice.
func TestReadyMsgRoundtrip(t *testing.T) {
	upstreams := []UpstreamRef{
		{ServerID: "ab3c", Command: "aimux", PID: 12345},
		{ServerID: "7d9e", Command: "engram", PID: 12346},
	}
	orig := NewReadyMsg(upstreams)
	if orig.Type != MsgReady {
		t.Fatalf("NewReadyMsg: Type = %q, want %q", orig.Type, MsgReady)
	}
	if orig.ProtocolVersion != HandoffProtocolVersion {
		t.Fatalf("NewReadyMsg: ProtocolVersion = %d, want %d", orig.ProtocolVersion, HandoffProtocolVersion)
	}

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal ReadyMsg: %v", err)
	}

	var got ReadyMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal ReadyMsg: %v", err)
	}
	if got.Type != orig.Type {
		t.Errorf("Type: got %q, want %q", got.Type, orig.Type)
	}
	if got.ProtocolVersion != orig.ProtocolVersion {
		t.Errorf("ProtocolVersion: got %d, want %d", got.ProtocolVersion, orig.ProtocolVersion)
	}
	if len(got.Upstreams) != len(orig.Upstreams) {
		t.Fatalf("Upstreams length: got %d, want %d", len(got.Upstreams), len(orig.Upstreams))
	}
	for i, u := range got.Upstreams {
		want := orig.Upstreams[i]
		if u.ServerID != want.ServerID {
			t.Errorf("Upstreams[%d].ServerID: got %q, want %q", i, u.ServerID, want.ServerID)
		}
		if u.Command != want.Command {
			t.Errorf("Upstreams[%d].Command: got %q, want %q", i, u.Command, want.Command)
		}
		if u.PID != want.PID {
			t.Errorf("Upstreams[%d].PID: got %d, want %d", i, u.PID, want.PID)
		}
	}
}

// TestFdTransferMsgRoundtrip verifies FdTransferMsg round-trips including
// both HandleMeta nested structs.
func TestFdTransferMsgRoundtrip(t *testing.T) {
	orig := NewFdTransferMsg("ab3c", HandleMeta{Kind: "pipe"}, HandleMeta{Kind: "pipe"})
	if orig.Type != MsgFdTransfer {
		t.Fatalf("NewFdTransferMsg: Type = %q, want %q", orig.Type, MsgFdTransfer)
	}
	if orig.ProtocolVersion != HandoffProtocolVersion {
		t.Fatalf("NewFdTransferMsg: ProtocolVersion = %d, want %d", orig.ProtocolVersion, HandoffProtocolVersion)
	}

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal FdTransferMsg: %v", err)
	}

	var got FdTransferMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal FdTransferMsg: %v", err)
	}
	if got.Type != orig.Type {
		t.Errorf("Type: got %q, want %q", got.Type, orig.Type)
	}
	if got.ProtocolVersion != orig.ProtocolVersion {
		t.Errorf("ProtocolVersion: got %d, want %d", got.ProtocolVersion, orig.ProtocolVersion)
	}
	if got.ServerID != orig.ServerID {
		t.Errorf("ServerID: got %q, want %q", got.ServerID, orig.ServerID)
	}
	if got.StdinHandleMeta.Kind != orig.StdinHandleMeta.Kind {
		t.Errorf("StdinHandleMeta.Kind: got %q, want %q", got.StdinHandleMeta.Kind, orig.StdinHandleMeta.Kind)
	}
	if got.StdoutHandleMeta.Kind != orig.StdoutHandleMeta.Kind {
		t.Errorf("StdoutHandleMeta.Kind: got %q, want %q", got.StdoutHandleMeta.Kind, orig.StdoutHandleMeta.Kind)
	}
}

// TestAckTransferMsgRoundtrip verifies both forms of AckTransferMsg:
// success (ok=true, reason=nil) and failure (ok=false, reason=ptr).
func TestAckTransferMsgRoundtrip(t *testing.T) {
	t.Run("ok_true_reason_nil", func(t *testing.T) {
		orig := NewAckTransferMsg("ab3c", true, nil)
		if orig.Type != MsgAckTransfer {
			t.Fatalf("Type = %q, want %q", orig.Type, MsgAckTransfer)
		}
		if orig.ProtocolVersion != HandoffProtocolVersion {
			t.Fatalf("ProtocolVersion = %d, want %d", orig.ProtocolVersion, HandoffProtocolVersion)
		}

		b, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got AckTransferMsg
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Type != orig.Type {
			t.Errorf("Type: got %q, want %q", got.Type, orig.Type)
		}
		if got.ProtocolVersion != orig.ProtocolVersion {
			t.Errorf("ProtocolVersion: got %d, want %d", got.ProtocolVersion, orig.ProtocolVersion)
		}
		if got.ServerID != "ab3c" {
			t.Errorf("ServerID: got %q, want %q", got.ServerID, "ab3c")
		}
		if !got.OK {
			t.Errorf("OK: got false, want true")
		}
		if got.Reason != nil {
			t.Errorf("Reason: got %v, want nil", got.Reason)
		}
	})

	t.Run("ok_false_with_reason", func(t *testing.T) {
		reason := "pid_not_owned_by_uid"
		orig := NewAckTransferMsg("ab3c", false, &reason)

		b, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got AckTransferMsg
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.OK {
			t.Errorf("OK: got true, want false")
		}
		if got.Reason == nil {
			t.Fatalf("Reason: got nil, want non-nil pointer")
		}
		if *got.Reason != reason {
			t.Errorf("Reason value: got %q, want %q", *got.Reason, reason)
		}
	})
}

// TestDoneMsgRoundtrip verifies DoneMsg round-trips including both slice fields.
func TestDoneMsgRoundtrip(t *testing.T) {
	orig := NewDoneMsg([]string{"ab3c", "7d9e"}, []string{})
	if orig.Type != MsgDone {
		t.Fatalf("NewDoneMsg: Type = %q, want %q", orig.Type, MsgDone)
	}
	if orig.ProtocolVersion != HandoffProtocolVersion {
		t.Fatalf("NewDoneMsg: ProtocolVersion = %d, want %d", orig.ProtocolVersion, HandoffProtocolVersion)
	}

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal DoneMsg: %v", err)
	}

	var got DoneMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal DoneMsg: %v", err)
	}
	if got.Type != orig.Type {
		t.Errorf("Type: got %q, want %q", got.Type, orig.Type)
	}
	if got.ProtocolVersion != orig.ProtocolVersion {
		t.Errorf("ProtocolVersion: got %d, want %d", got.ProtocolVersion, orig.ProtocolVersion)
	}
	if len(got.Transferred) != 2 {
		t.Fatalf("Transferred length: got %d, want 2", len(got.Transferred))
	}
	if got.Transferred[0] != "ab3c" || got.Transferred[1] != "7d9e" {
		t.Errorf("Transferred: got %v, want [ab3c 7d9e]", got.Transferred)
	}
	if len(got.Aborted) != 0 {
		t.Errorf("Aborted: got %v, want empty", got.Aborted)
	}
}

// TestHandoffAckMsgRoundtrip verifies HandoffAckMsg round-trips preserving Status.
func TestHandoffAckMsgRoundtrip(t *testing.T) {
	orig := NewHandoffAckMsg("accepted")
	if orig.Type != MsgHandoffAck {
		t.Fatalf("NewHandoffAckMsg: Type = %q, want %q", orig.Type, MsgHandoffAck)
	}
	if orig.ProtocolVersion != HandoffProtocolVersion {
		t.Fatalf("NewHandoffAckMsg: ProtocolVersion = %d, want %d", orig.ProtocolVersion, HandoffProtocolVersion)
	}

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal HandoffAckMsg: %v", err)
	}

	var got HandoffAckMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal HandoffAckMsg: %v", err)
	}
	if got.Type != orig.Type {
		t.Errorf("Type: got %q, want %q", got.Type, orig.Type)
	}
	if got.ProtocolVersion != orig.ProtocolVersion {
		t.Errorf("ProtocolVersion: got %d, want %d", got.ProtocolVersion, orig.ProtocolVersion)
	}
	if got.Status != orig.Status {
		t.Errorf("Status: got %q, want %q", got.Status, orig.Status)
	}
}

// TestProtocolVersionReject verifies that validateProtocolVersion returns
// ErrProtocolVersionMismatch for any version != HandoffProtocolVersion (FR-6).
// Each of the 6 message types is covered.
func TestProtocolVersionReject(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "hello",
			raw:  `{"type":"hello","protocol_version":2,"token":"abc"}`,
		},
		{
			name: "ready",
			raw:  `{"type":"ready","protocol_version":2,"upstreams":[]}`,
		},
		{
			name: "fd_transfer",
			raw:  `{"type":"fd_transfer","protocol_version":2,"server_id":"x","stdin_handle_meta":{"kind":"pipe"},"stdout_handle_meta":{"kind":"pipe"}}`,
		},
		{
			name: "ack_transfer",
			raw:  `{"type":"ack_transfer","protocol_version":2,"server_id":"x","ok":true,"reason":null}`,
		},
		{
			name: "done",
			raw:  `{"type":"done","protocol_version":2,"transferred":[],"aborted":[]}`,
		},
		{
			name: "handoff_ack",
			raw:  `{"type":"handoff_ack","protocol_version":2,"status":"accepted"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var base struct {
				ProtocolVersion int `json:"protocol_version"`
			}
			if err := json.Unmarshal([]byte(tc.raw), &base); err != nil {
				t.Fatalf("unmarshal base: %v", err)
			}
			err := validateProtocolVersion(base.ProtocolVersion)
			if err == nil {
				t.Fatal("expected error for protocol_version 2, got nil")
			}
			if !errors.Is(err, ErrProtocolVersionMismatch) {
				t.Errorf("expected errors.Is(err, ErrProtocolVersionMismatch), got: %v", err)
			}
		})
	}
}

// TestProtocolVersionAccept verifies the current protocol version is accepted.
func TestProtocolVersionAccept(t *testing.T) {
	if err := validateProtocolVersion(HandoffProtocolVersion); err != nil {
		t.Fatalf("validateProtocolVersion(%d) error = %v, want nil", HandoffProtocolVersion, err)
	}
}

// TestHelloMsg_WithPID_RoundTrip verifies that NewHelloMsgWithPID sets SourcePID
// and that it survives a JSON round-trip (required for T017 DuplicateHandle flow).
// The SourcePID field is omitempty, so a zero PID should be absent from JSON;
// a non-zero PID must be present and recovered after unmarshal.
func TestHelloMsg_WithPID_RoundTrip(t *testing.T) {
	msg := NewHelloMsgWithPID("tok", 12345)
	if msg.SourcePID != 12345 {
		t.Errorf("SourcePID before marshal: got %d, want 12345", msg.SourcePID)
	}
	if msg.Type != MsgHello {
		t.Errorf("Type: got %q, want %q", msg.Type, MsgHello)
	}
	if msg.ProtocolVersion != HandoffProtocolVersion {
		t.Errorf("ProtocolVersion: got %d, want %d", msg.ProtocolVersion, HandoffProtocolVersion)
	}

	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got HelloMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.SourcePID != 12345 {
		t.Errorf("SourcePID after round-trip: got %d, want 12345", got.SourcePID)
	}
	if got.Token != "tok" {
		t.Errorf("Token: got %q, want tok", got.Token)
	}
	if got.Type != MsgHello {
		t.Errorf("Type: got %q, want %q", got.Type, MsgHello)
	}
	if got.ProtocolVersion != HandoffProtocolVersion {
		t.Errorf("ProtocolVersion: got %d, want %d", got.ProtocolVersion, HandoffProtocolVersion)
	}

	// Verify omitempty: a zero SourcePID must not appear in JSON output.
	zeroMsg := NewHelloMsg("tok2")
	bZero, err := json.Marshal(zeroMsg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bZero, []byte("source_pid")) {
		t.Errorf("source_pid unexpectedly present in JSON for zero SourcePID: %s", bZero)
	}
}

// TestMessageTypeField verifies that each constructor sets the Type field to
// the correct MsgType constant. If a constructor returned a zero value, the
// Type field would be empty string and this test would fail.
func TestMessageTypeField(t *testing.T) {
	reason := "test-reason"
	cases := []struct {
		name     string
		wantType MsgType
		gotType  MsgType
	}{
		{
			name:     "HelloMsg",
			wantType: MsgHello,
			gotType:  NewHelloMsg("tok").Type,
		},
		{
			name:     "ReadyMsg",
			wantType: MsgReady,
			gotType:  NewReadyMsg(nil).Type,
		},
		{
			name:     "FdTransferMsg",
			wantType: MsgFdTransfer,
			gotType:  NewFdTransferMsg("id", HandleMeta{Kind: "pipe"}, HandleMeta{Kind: "pipe"}).Type,
		},
		{
			name:     "AckTransferMsg",
			wantType: MsgAckTransfer,
			gotType:  NewAckTransferMsg("id", true, &reason).Type,
		},
		{
			name:     "DoneMsg",
			wantType: MsgDone,
			gotType:  NewDoneMsg(nil, nil).Type,
		},
		{
			name:     "HandoffAckMsg",
			wantType: MsgHandoffAck,
			gotType:  NewHandoffAckMsg("accepted").Type,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.gotType != tc.wantType {
				t.Errorf("Type field: got %q, want %q", tc.gotType, tc.wantType)
			}
		})
	}
}
