package supervisor

import (
	"bytes"
	"errors"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
)

func TestProtocolV2ExactFrames(t *testing.T) {
	t.Parallel()

	protocol := ProtocolV2()
	if !protocol.Enabled() || protocol.DormantExitCode() != 75 {
		t.Fatalf("ProtocolV2 enabled=%v exit=%d", protocol.Enabled(), protocol.DormantExitCode())
	}

	tests := map[Control]string{
		ControlDormantReady:  `{"jsonrpc":"2.0","method":"$/mcp-mux/launcher/dormant-ready"}`,
		ControlCommitDormant: `{"jsonrpc":"2.0","method":"$/mcp-mux/launcher/commit-dormant"}`,
		ControlDormantAck:    `{"jsonrpc":"2.0","method":"$/mcp-mux/launcher/dormant-ack"}`,
		ControlDormantNack:   `{"jsonrpc":"2.0","method":"$/mcp-mux/launcher/dormant-nack"}`,
	}
	for control, want := range tests {
		got := protocol.Frame(control)
		if string(got) != want {
			t.Errorf("Frame(%v) = %s, want %s", control, got, want)
		}
		got[0] = 'x'
		if bytes.Equal(got, protocol.Frame(control)) {
			t.Errorf("Frame(%v) reused mutable bytes", control)
		}
	}
}

func TestProtocolZeroValueDisabled(t *testing.T) {
	t.Parallel()

	var protocol Protocol
	if protocol.Enabled() {
		t.Fatal("zero Protocol is enabled")
	}
	if got := protocol.Frame(ControlDormantReady); got != nil {
		t.Fatalf("zero Protocol frame = %q", got)
	}
	if got := protocol.DormantExitCode(); got != -1 {
		t.Fatalf("zero Protocol exit = %d", got)
	}

	message, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","method":"$/mcp-mux/launcher/dormant-ready"}`))
	if err != nil {
		t.Fatal(err)
	}
	control, reserved, err := protocol.ControlOf(message)
	if control != ControlNone || !reserved || !errors.Is(err, ErrProtocolDisabled) {
		t.Fatalf("ControlOf disabled = (%v, %v, %v)", control, reserved, err)
	}
}

func TestProtocolControlOf(t *testing.T) {
	t.Parallel()

	protocol := ProtocolV2()
	for control := ControlDormantReady; control <= ControlDormantNack; control++ {
		message, err := jsonrpc.Parse(protocol.Frame(control))
		if err != nil {
			t.Fatalf("Parse control %v: %v", control, err)
		}
		got, reserved, err := protocol.ControlOf(message)
		if err != nil || !reserved || got != control {
			t.Fatalf("ControlOf(%v) = (%v, %v, %v)", control, got, reserved, err)
		}
	}

	ordinary, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatal(err)
	}
	if control, reserved, err := protocol.ControlOf(ordinary); control != ControlNone || reserved || err != nil {
		t.Fatalf("ordinary ControlOf = (%v, %v, %v)", control, reserved, err)
	}
}

func TestProtocolRejectsMalformedReservedFrames(t *testing.T) {
	t.Parallel()

	protocol := ProtocolV2()
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"$/mcp-mux/launcher/dormant-ready"}`,
		`{"jsonrpc":"2.0","method":"$/mcp-mux/launcher/dormant-ready","params":{}}`,
		`{"jsonrpc":"2.0","method":"$/mcp-mux/launcher/future"}`,
	} {
		message, err := jsonrpc.Parse([]byte(raw))
		if err != nil {
			t.Fatalf("jsonrpc.Parse(%s): %v", raw, err)
		}
		if control, reserved, err := protocol.ControlOf(message); control != ControlNone || !reserved || err == nil {
			t.Errorf("ControlOf(%s) = (%v, %v, %v), want reserved error", raw, control, reserved, err)
		}
	}
}
