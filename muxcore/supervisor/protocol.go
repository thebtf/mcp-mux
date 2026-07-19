package supervisor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
)

const (
	reservedMethodPrefix = "$/mcp-mux/launcher/"

	dormantReadyMethod  = reservedMethodPrefix + "dormant-ready"
	commitDormantMethod = reservedMethodPrefix + "commit-dormant"
	dormantAckMethod    = reservedMethodPrefix + "dormant-ack"
	dormantNackMethod   = reservedMethodPrefix + "dormant-nack"

	dormantExitCode = 75
)

var ErrProtocolDisabled = errors.New("supervisor: lifecycle protocol disabled")

// Control is one private stable-launcher lifecycle message.
type Control uint8

const (
	ControlNone Control = iota
	ControlDormantReady
	ControlCommitDormant
	ControlDormantAck
	ControlDormantNack
)

// Protocol is an immutable lifecycle protocol value. Its zero value is
// deliberately disabled.
type Protocol struct {
	version uint8
}

// ProtocolV2 returns the current stable-launcher lifecycle protocol.
func ProtocolV2() Protocol { return Protocol{version: 2} }

// Enabled reports whether this value authorizes a known protocol version.
func (protocol Protocol) Enabled() bool { return protocol.version == 2 }

// Reserved reports whether method belongs to the private launcher namespace.
func (Protocol) Reserved(method string) bool {
	return strings.HasPrefix(method, reservedMethodPrefix)
}

// ControlOf classifies a private frame. Non-reserved frames return
// (ControlNone, false, nil). Unknown or malformed reserved frames return an
// error and must never become lifecycle input.
func (protocol Protocol) ControlOf(message *jsonrpc.Message) (Control, bool, error) {
	if message == nil || !protocol.Reserved(message.Method) {
		return ControlNone, false, nil
	}
	if !protocol.Enabled() {
		return ControlNone, true, ErrProtocolDisabled
	}

	frame, err := parseFrame(message.Raw, 0)
	if err != nil {
		return ControlNone, true, fmt.Errorf("supervisor: malformed private control: %w", err)
	}
	if frame.method != message.Method {
		return ControlNone, true, fmt.Errorf("supervisor: private control method mismatch")
	}
	control, err := controlOfParsedFrame(frame)
	return control, true, err
}

func controlOfParsedFrame(frame *parsedFrame) (Control, error) {
	if frame == nil || !frame.reserved || frame.kind != frameNotification || len(frame.params) != 0 {
		return ControlNone, fmt.Errorf("supervisor: private control must be a parameterless notification")
	}
	switch frame.method {
	case dormantReadyMethod:
		return ControlDormantReady, nil
	case commitDormantMethod:
		return ControlCommitDormant, nil
	case dormantAckMethod:
		return ControlDormantAck, nil
	case dormantNackMethod:
		return ControlDormantNack, nil
	default:
		return ControlNone, fmt.Errorf("supervisor: unknown private control method %q", frame.method)
	}
}

// Frame returns fresh JSON bytes for control, without the newline delimiter.
func (protocol Protocol) Frame(control Control) []byte {
	if !protocol.Enabled() {
		return nil
	}
	var method string
	switch control {
	case ControlDormantReady:
		method = dormantReadyMethod
	case ControlCommitDormant:
		method = commitDormantMethod
	case ControlDormantAck:
		method = dormantAckMethod
	case ControlDormantNack:
		method = dormantNackMethod
	default:
		return nil
	}
	return []byte(`{"jsonrpc":"2.0","method":"` + method + `"}`)
}

// DormantExitCode returns the reserved clean-dormancy exit code, or -1 for a
// disabled protocol.
func (protocol Protocol) DormantExitCode() int {
	if !protocol.Enabled() {
		return -1
	}
	return dormantExitCode
}
