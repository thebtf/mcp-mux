package daemon

import (
	"errors"
	"fmt"
)

// HandoffProtocolVersion is the current version of the handoff control protocol.
// The protocol_version field is mandatory in every message; a successor daemon
// that receives an unknown version must reject the handoff and fall back to
// the legacy shutdown-and-respawn path (FR-6, FR-8).
const HandoffProtocolVersion = 1

// MsgType identifies the kind of handoff control message.
type MsgType string

const (
	// MsgHello is sent by the successor daemon as the first message after connecting.
	MsgHello MsgType = "hello"

	// MsgReady is sent by the old daemon after verifying the token; lists all upstreams.
	MsgReady MsgType = "ready"

	// MsgFdTransfer is sent by the old daemon once per upstream with handle metadata.
	// The actual file descriptors are transferred out-of-band via SCM_RIGHTS (Unix)
	// or DuplicateHandle (Windows).
	MsgFdTransfer MsgType = "fd_transfer"

	// MsgAckTransfer is sent by the successor daemon after reattaching an upstream.
	MsgAckTransfer MsgType = "ack_transfer"

	// MsgDone is the terminal message from the old daemon after all upstreams are processed.
	MsgDone MsgType = "done"

	// MsgHandoffAck is the terminal acknowledgment from the successor daemon.
	MsgHandoffAck MsgType = "handoff_ack"
)

// UpstreamRef describes a single upstream server registered with the old daemon.
type UpstreamRef struct {
	ServerID string `json:"server_id"`
	Command  string `json:"command"`
	PID      int    `json:"pid"`
}

// HandleMeta carries metadata about a process handle (stdin or stdout pipe).
// The actual handle/fd is transferred out-of-band.
type HandleMeta struct {
	Kind string `json:"kind"`
}

// HelloMsg is sent by the successor daemon to the old daemon as the first message.
// It presents the handoff token for authentication (FR-11).
type HelloMsg struct {
	Type            MsgType `json:"type"`
	ProtocolVersion int     `json:"protocol_version"`
	Token           string  `json:"token"`
}

// ReadyMsg is sent by the old daemon after token verification. It lists all
// upstreams that will be transferred.
type ReadyMsg struct {
	Type            MsgType       `json:"type"`
	ProtocolVersion int           `json:"protocol_version"`
	Upstreams       []UpstreamRef `json:"upstreams"`
}

// FdTransferMsg is sent by the old daemon once per upstream. It carries handle
// metadata; the actual file descriptors travel out-of-band via platform-specific
// mechanisms (SCM_RIGHTS on Unix, DuplicateHandle on Windows).
type FdTransferMsg struct {
	Type             MsgType    `json:"type"`
	ProtocolVersion  int        `json:"protocol_version"`
	ServerID         string     `json:"server_id"`
	StdinHandleMeta  HandleMeta `json:"stdin_handle_meta"`
	StdoutHandleMeta HandleMeta `json:"stdout_handle_meta"`
}

// AckTransferMsg is sent by the successor daemon after attempting to reattach
// an upstream. On failure, the old daemon falls back to ordinary shutdown for
// that upstream only (FR-7 per-upstream atomicity).
type AckTransferMsg struct {
	Type            MsgType `json:"type"`
	ProtocolVersion int     `json:"protocol_version"`
	ServerID        string  `json:"server_id"`
	OK              bool    `json:"ok"`
	Reason          *string `json:"reason"`
}

// DoneMsg is the terminal message sent by the old daemon after all upstreams
// have been acknowledged or aborted.
type DoneMsg struct {
	Type            MsgType  `json:"type"`
	ProtocolVersion int      `json:"protocol_version"`
	Transferred     []string `json:"transferred"`
	Aborted         []string `json:"aborted"`
}

// HandoffAckMsg is the terminal acknowledgment sent by the successor daemon.
// After receiving this message, the old daemon exits.
type HandoffAckMsg struct {
	Type            MsgType `json:"type"`
	ProtocolVersion int     `json:"protocol_version"`
	Status          string  `json:"status"`
}

// Constructor helpers. Each function sets Type and ProtocolVersion correctly.
// If a constructor body were replaced with a zero-value return, the Type and
// ProtocolVersion fields would be empty/zero, causing all roundtrip tests to fail.

// NewHelloMsg constructs a HelloMsg with the mandatory fields set.
func NewHelloMsg(token string) HelloMsg {
	return HelloMsg{
		Type:            MsgHello,
		ProtocolVersion: HandoffProtocolVersion,
		Token:           token,
	}
}

// NewReadyMsg constructs a ReadyMsg with the mandatory fields set.
func NewReadyMsg(upstreams []UpstreamRef) ReadyMsg {
	return ReadyMsg{
		Type:            MsgReady,
		ProtocolVersion: HandoffProtocolVersion,
		Upstreams:       upstreams,
	}
}

// NewFdTransferMsg constructs a FdTransferMsg with the mandatory fields set.
func NewFdTransferMsg(serverID string, stdinMeta, stdoutMeta HandleMeta) FdTransferMsg {
	return FdTransferMsg{
		Type:             MsgFdTransfer,
		ProtocolVersion:  HandoffProtocolVersion,
		ServerID:         serverID,
		StdinHandleMeta:  stdinMeta,
		StdoutHandleMeta: stdoutMeta,
	}
}

// NewAckTransferMsg constructs an AckTransferMsg with the mandatory fields set.
// reason must be nil on success (ok=true) and a non-nil pointer on failure.
func NewAckTransferMsg(serverID string, ok bool, reason *string) AckTransferMsg {
	return AckTransferMsg{
		Type:            MsgAckTransfer,
		ProtocolVersion: HandoffProtocolVersion,
		ServerID:        serverID,
		OK:              ok,
		Reason:          reason,
	}
}

// NewDoneMsg constructs a DoneMsg with the mandatory fields set.
// transferred and aborted are slices of server_ids.
func NewDoneMsg(transferred, aborted []string) DoneMsg {
	return DoneMsg{
		Type:            MsgDone,
		ProtocolVersion: HandoffProtocolVersion,
		Transferred:     transferred,
		Aborted:         aborted,
	}
}

// NewHandoffAckMsg constructs a HandoffAckMsg with the mandatory fields set.
func NewHandoffAckMsg(status string) HandoffAckMsg {
	return HandoffAckMsg{
		Type:            MsgHandoffAck,
		ProtocolVersion: HandoffProtocolVersion,
		Status:          status,
	}
}

// ErrProtocolVersionMismatch is returned by validateProtocolVersion when the
// received protocol_version does not match HandoffProtocolVersion.
// Callers should use errors.Is to detect this error class (FR-6).
var ErrProtocolVersionMismatch = errors.New("handoff: protocol version mismatch")

// validateProtocolVersion returns a wrapped ErrProtocolVersionMismatch when
// v != HandoffProtocolVersion, signaling the caller to abort the handoff and
// fall back to the legacy shutdown-and-respawn path (FR-6, FR-8).
func validateProtocolVersion(v int) error {
	if v != HandoffProtocolVersion {
		return fmt.Errorf("%w: got %d, expected %d", ErrProtocolVersionMismatch, v, HandoffProtocolVersion)
	}
	return nil
}
