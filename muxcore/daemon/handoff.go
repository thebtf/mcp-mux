package daemon

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fdConn abstracts over platform-specific FD transfer channels (Unix socket
// + SCM_RIGHTS, Windows named pipe + DuplicateHandle). Platform-specific
// implementations live in handoff_unix.go / handoff_windows.go (T009-T010,
// T016-T017). This file uses only the interface.
type fdConn interface {
	// WriteJSON writes one newline-delimited JSON message.
	WriteJSON(v any) error
	// ReadJSON reads one newline-delimited JSON message into v.
	ReadJSON(v any) error
	// SendFDs transfers OS file descriptors out-of-band alongside a
	// metadata header (sent via WriteJSON). On Unix this is SCM_RIGHTS;
	// on Windows it's DuplicateHandle + pipe message.
	SendFDs(fds []uintptr, header []byte) error
	// RecvFDs receives OS file descriptors alongside an out-of-band header.
	RecvFDs() (fds []uintptr, header []byte, err error)
	// Close releases the underlying transport.
	Close() error
}

// errPlatformUnsupported is returned by stubFDConn for all FD operations.
var errPlatformUnsupported = errors.New("handoff: platform not yet implemented")

// stubFDConn is a placeholder for platforms not yet implemented.
// All operations return errPlatformUnsupported except Close.
type stubFDConn struct{}

func (stubFDConn) WriteJSON(v any) error                      { return errPlatformUnsupported }
func (stubFDConn) ReadJSON(v any) error                       { return errPlatformUnsupported }
func (stubFDConn) SendFDs(fds []uintptr, header []byte) error { return errPlatformUnsupported }
func (stubFDConn) RecvFDs() ([]uintptr, []byte, error)        { return nil, nil, errPlatformUnsupported }
func (stubFDConn) Close() error                               { return nil }

// ErrTokenMismatch is returned by performHandoff when the successor daemon
// presents a token that does not match the pre-shared secret (FR-11).
var ErrTokenMismatch = errors.New("handoff: token mismatch")

// HandoffResult describes the outcome of a performHandoff call.
type HandoffResult struct {
	Transferred []string // server_ids successfully handed off
	Aborted     []string // server_ids that fell back to FR-8
	Phase       string   // last phase reached for observability
}

// HandoffUpstream is the caller-provided description of one upstream owner
// to be handed off to the successor daemon.
type HandoffUpstream struct {
	ServerID string
	Command  string
	PID      int
	StdinFD  uintptr
	StdoutFD uintptr
}

// performHandoff runs the old-daemon side of the two-daemon handoff protocol.
// conn is the fdConn dialed/accepted by the caller; token is the pre-shared
// authentication token (FR-11); upstreams is the list of owners to transfer.
//
// Protocol:
//
//	1. Wait for Hello msg from successor; verify version + token.
//	2. Send Ready listing all upstreams.
//	3. For each upstream: send FdTransfer + SendFDs; read AckTransfer.
//	   Aborted if ok:false OR SendFDs side-channel error.
//	4. Send Done with transferred/aborted split.
//	5. Read HandoffAck from successor.
//
// Returns HandoffResult with per-upstream outcome. Total error only on
// protocol-level failures (token reject, version mismatch, conn drop).
func performHandoff(ctx context.Context, conn fdConn, token string, upstreams []HandoffUpstream) (HandoffResult, error) {
	// ctx is reserved for future deadline/cancellation propagation to conn
	// reads and writes (T009). Currently fdConn does not expose a deadline API;
	// callers should set a deadline on the underlying transport before calling.
	_ = ctx

	// Step 1: Read Hello from successor.
	var hello HelloMsg
	if err := conn.ReadJSON(&hello); err != nil {
		return HandoffResult{Phase: "hello"}, fmt.Errorf("performHandoff: read hello: %w", err)
	}
	// Step 2: Validate protocol version.
	if err := validateProtocolVersion(hello.ProtocolVersion); err != nil {
		return HandoffResult{Phase: "hello"}, err
	}
	// Step 3: Validate token using constant-time comparison to prevent timing
	// side-channels. verifyHandoffToken (used on the accept path) already does
	// this; performHandoff must be consistent.
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(token)) != 1 {
		return HandoffResult{Phase: "hello"}, ErrTokenMismatch
	}
	// Platform hook: Windows uses the successor PID for DuplicateHandle.
	// Unix ignores (unixFDConn does not implement SetTargetPID).
	if setter, ok := conn.(interface{ SetTargetPID(int) }); ok {
		if hello.SourcePID <= 0 {
			return HandoffResult{Phase: "hello"},
				fmt.Errorf("performHandoff: invalid source_pid: %d", hello.SourcePID)
		}
		setter.SetTargetPID(hello.SourcePID)
	}
	// Step 4: Send Ready listing all upstreams.
	refs := make([]UpstreamRef, len(upstreams))
	for i, u := range upstreams {
		refs[i] = UpstreamRef{ServerID: u.ServerID, Command: u.Command, PID: u.PID}
	}
	if err := conn.WriteJSON(NewReadyMsg(refs)); err != nil {
		return HandoffResult{Phase: "ready"}, fmt.Errorf("performHandoff: send ready: %w", err)
	}
	// Step 5: Transfer each upstream.
	var transferred, aborted []string
	for _, u := range upstreams {
		// Send FdTransfer control message.
		if err := conn.WriteJSON(NewFdTransferMsg(u.ServerID, HandleMeta{Kind: "stdin"}, HandleMeta{Kind: "stdout"})); err != nil {
			aborted = append(aborted, u.ServerID)
			continue
		}
		// Transfer FDs out-of-band. The actual FdTransferMsg metadata was
		// sent via WriteJSON above — SCM_RIGHTS carries only the handle
		// numbers themselves, no additional header bytes. Linux accepts
		// SCM_RIGHTS with 0-byte data on SOCK_STREAM; macOS/BSD may
		// reject. The Unix impl (unixFDConn) uses a 1-byte sentinel
		// internally on platforms that require it. Call with nil header
		// from the protocol layer.
		if err := conn.SendFDs([]uintptr{u.StdinFD, u.StdoutFD}, nil); err != nil {
			// Drain the AckTransfer the successor may send (best-effort).
			var ack AckTransferMsg
			_ = conn.ReadJSON(&ack)
			aborted = append(aborted, u.ServerID)
			continue
		}
		// Read AckTransfer from successor.
		var ack AckTransferMsg
		if err := conn.ReadJSON(&ack); err != nil {
			aborted = append(aborted, u.ServerID)
			continue
		}
		if !ack.OK {
			aborted = append(aborted, u.ServerID)
		} else {
			transferred = append(transferred, u.ServerID)
		}
	}
	// Step 6: Send Done.
	if err := conn.WriteJSON(NewDoneMsg(transferred, aborted)); err != nil {
		return HandoffResult{Transferred: transferred, Aborted: aborted, Phase: "done-send"},
			fmt.Errorf("performHandoff: send done: %w", err)
	}
	// Step 7: Read HandoffAck (consume; old daemon exits regardless).
	var finalAck HandoffAckMsg
	_ = conn.ReadJSON(&finalAck)
	return HandoffResult{Transferred: transferred, Aborted: aborted, Phase: "done"}, nil
}

// receiveHandoff runs the new-daemon side of the two-daemon handoff protocol.
// Returns the list of upstreams successfully received; callers use this to
// bind them to re-created Owner instances.
func receiveHandoff(ctx context.Context, conn fdConn, token string) (received []HandoffUpstream, err error) {
	// ctx is reserved for future deadline/cancellation propagation (T009).
	_ = ctx

	// Step 1: Send Hello with token.
	if err := conn.WriteJSON(NewHelloMsgWithPID(token, os.Getpid())); err != nil {
		return nil, fmt.Errorf("receiveHandoff: send hello: %w", err)
	}
	// Step 2: Read Ready; record upstream list.
	var ready ReadyMsg
	if err := conn.ReadJSON(&ready); err != nil {
		return nil, fmt.Errorf("receiveHandoff: read ready: %w", err)
	}
	if err := validateProtocolVersion(ready.ProtocolVersion); err != nil {
		return nil, err
	}
	// Build a ref map for command/pid lookup during ack construction.
	refMap := make(map[string]UpstreamRef, len(ready.Upstreams))
	for _, ref := range ready.Upstreams {
		refMap[ref.ServerID] = ref
	}
	// Step 3: For each upstream reference: read FdTransfer, RecvFDs, send Ack.
	for range ready.Upstreams {
		var transfer FdTransferMsg
		if err := conn.ReadJSON(&transfer); err != nil {
			return nil, fmt.Errorf("receiveHandoff: read fd_transfer: %w", err)
		}
		if err := validateProtocolVersion(transfer.ProtocolVersion); err != nil {
			return nil, err
		}
		fds, _, recvErr := conn.RecvFDs()
		if recvErr != nil || len(fds) < 2 {
			reason := "recv fds failed"
			if recvErr != nil {
				reason = recvErr.Error()
			}
			if werr := conn.WriteJSON(NewAckTransferMsg(transfer.ServerID, false, &reason)); werr != nil {
				return nil, fmt.Errorf("receiveHandoff: send ack (failure): %w", werr)
			}
			continue
		}
		if werr := conn.WriteJSON(NewAckTransferMsg(transfer.ServerID, true, nil)); werr != nil {
			return nil, fmt.Errorf("receiveHandoff: send ack (success): %w", werr)
		}
		ref := refMap[transfer.ServerID]
		received = append(received, HandoffUpstream{
			ServerID: transfer.ServerID,
			Command:  ref.Command,
			PID:      ref.PID,
			StdinFD:  fds[0],
			StdoutFD: fds[1],
		})
	}
	// Step 4: Read Done; cross-check is left to callers if needed.
	var done DoneMsg
	if err := conn.ReadJSON(&done); err != nil {
		return nil, fmt.Errorf("receiveHandoff: read done: %w", err)
	}
	// Step 5: Send HandoffAck.
	if err := conn.WriteJSON(NewHandoffAckMsg("accepted")); err != nil {
		return nil, fmt.Errorf("receiveHandoff: send handoff_ack: %w", err)
	}
	return received, nil
}

// writeHandoffToken generates a 128-bit handoff token and writes it to
// {dir}/mcp-mux-handoff.tok with 0600 perms. Returns (token, path, err).
// The caller MUST delete the file after the handoff window closes —
// use deleteHandoffToken for idempotent cleanup.
func writeHandoffToken(dir string) (token string, path string, err error) {
	token, err = generateToken()
	if err != nil {
		return "", "", fmt.Errorf("handoff: generate token: %w", err)
	}
	path = filepath.Join(dir, "mcp-mux-handoff.tok")
	// os.WriteFile with 0600 perm. On Windows the perm is advisory;
	// matches the existing daemon token discipline.
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		return "", "", fmt.Errorf("handoff: write token to %s: %w", path, err)
	}
	return token, path, nil
}

// readHandoffToken reads a previously written handoff token.
func readHandoffToken(path string) (token string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("handoff: read token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// deleteHandoffToken removes the token file if it exists. Idempotent —
// missing file is NOT an error. Call from defer in performHandoff.
func deleteHandoffToken(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("handoff: delete token: %w", err)
	}
	return nil
}

// verifyHandoffToken reads a HelloMsg from conn, validates the protocol
// version, then constant-time compares the token. Returns
// ErrProtocolVersionMismatch OR ErrTokenMismatch on failure.
func verifyHandoffToken(conn fdConn, expected string) error {
	var hello HelloMsg
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("handoff: read hello: %w", err)
	}
	if err := validateProtocolVersion(hello.ProtocolVersion); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(expected)) != 1 {
		return ErrTokenMismatch
	}
	return nil
}
