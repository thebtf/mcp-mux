//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// handoffPipePrefix is the Windows named-pipe namespace root. All handoff
// pipes are anonymous within this prefix; each handoff window creates a
// unique random suffix (caller-supplied via `name`).
const handoffPipePrefix = `\\.\pipe\mcp-mux-handoff-`

// handoffPipeAcceptTimeout bounds how long the old daemon waits for the
// successor to dial before aborting the handoff. Matches FR-7 per-upstream
// atomic boundary — each transfer either completes or is aborted in bound time.
const handoffPipeAcceptTimeout = 30 * time.Second

// listenHandoffPipe creates a named-pipe listener restricted to the current
// logon session via DACL. `name` is appended to handoffPipePrefix to form
// the full pipe path (caller supplies a cryptographic random suffix).
//
// Security: the DACL is taken from winio's built-in current-user template.
// A process running as a different user cannot connect — NFR-5 security gate
// on the transport layer (complementing FR-11 token auth).
func listenHandoffPipe(name string) (net.Listener, error) {
	path := handoffPipePrefix + name
	cfg := &winio.PipeConfig{
		// SecurityDescriptor nil → winio uses a default that allows the
		// creating process and same-user to connect; denies other users.
		// InputBufferSize / OutputBufferSize: 64 KiB is generous for JSON
		// messages ~1 KiB each. FDs go out-of-band via DuplicateHandle (T017).
		InputBufferSize:  65536,
		OutputBufferSize: 65536,
		MessageMode:      false, // byte stream for newline-delimited JSON
	}
	ln, err := winio.ListenPipe(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("handoff: listen named pipe %s: %w", path, err)
	}
	return ln, nil
}

// dialHandoffPipe connects to a named pipe created by listenHandoffPipe.
// `name` must match the listener. `timeout` is applied to the dial attempt.
func dialHandoffPipe(name string, timeout time.Duration) (net.Conn, error) {
	path := handoffPipePrefix + name
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("handoff: dial named pipe %s: %w", path, err)
	}
	return conn, nil
}

// msgHandleBatch is the MsgType for windowsHandleBatch wire messages.
const msgHandleBatch MsgType = "handle_batch"

// windowsHandleBatch is the on-wire message for DuplicateHandle transfer.
// Sent by the old daemon after duplicating handles into the successor's address
// space; received by the successor. Handles contains target-space handle values
// valid in the successor's process — no further syscall needed on receive.
type windowsHandleBatch struct {
	Type            MsgType   `json:"type"`
	ProtocolVersion int       `json:"protocol_version"`
	Header          []byte    `json:"header"`
	Handles         []uintptr `json:"handles"`
}

// windowsFDConn implements fdConn on top of a named-pipe connection.
// WriteJSON / ReadJSON / Close were implemented in T016.
// SendFDs / RecvFDs use DuplicateHandle (T017): sender duplicates handles into
// the receiver's address space, receiver reads them directly — no round-trip.
type windowsFDConn struct {
	conn net.Conn
	// targetPID is set by the sender after reading the successor's Hello message.
	// Required for DuplicateHandle: the sender must open the target process.
	targetPID int
}

func newWindowsFDConn(conn net.Conn) *windowsFDConn {
	return &windowsFDConn{conn: conn}
}

// SetTargetPID records the PID of the process that will receive the handles.
// Must be called before SendFDs. On the sender side, this is the successor's
// PID taken from HelloMsg.SourcePID. On the receiver side no call is needed.
func (w *windowsFDConn) SetTargetPID(pid int) { w.targetPID = pid }

func (w *windowsFDConn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("windowsFDConn: marshal: %w", err)
	}
	b = append(b, '\n')
	for sent := 0; sent < len(b); {
		n, werr := w.conn.Write(b[sent:])
		if werr != nil {
			return fmt.Errorf("windowsFDConn: write: %w", werr)
		}
		sent += n
	}
	return nil
}

func (w *windowsFDConn) ReadJSON(v any) error {
	// Read newline-delimited JSON. bufio.Scanner would be cleaner, but we
	// need to own the underlying reader for the SCM_RIGHTS-equivalent
	// DuplicateHandle path in T017 — a raw ReadMsg-style approach. For T016
	// a simple byte-at-a-time read up to '\n' is sufficient.
	var buf [1]byte
	var msg []byte
	for {
		n, err := w.conn.Read(buf[:])
		if err != nil {
			if errors.Is(err, winio.ErrPipeListenerClosed) ||
				errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("windowsFDConn: pipe closed: %w", err)
			}
			return fmt.Errorf("windowsFDConn: read: %w", err)
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			break
		}
		msg = append(msg, buf[0])
	}
	if err := json.Unmarshal(msg, v); err != nil {
		return fmt.Errorf("windowsFDConn: unmarshal %q: %w", msg, err)
	}
	return nil
}

// SendFDs duplicates each handle in fds into the target process (identified by
// targetPID) and sends the resulting target-space handle values over the pipe
// as a windowsHandleBatch JSON message.
//
// Unlike Unix SCM_RIGHTS (kernel delivers FDs atomically), Windows DuplicateHandle
// is "push": the sender opens the target process, duplicates each source handle
// into its address space, then sends the resulting handle values via the pipe.
// The receiver reads the handle values and uses them directly — no further syscall.
//
// SetTargetPID must be called before SendFDs.
func (w *windowsFDConn) SendFDs(fds []uintptr, header []byte) error {
	if w.targetPID == 0 {
		return errors.New("windowsFDConn: targetPID not set; call SetTargetPID after Hello")
	}
	targetProc, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE, false, uint32(w.targetPID))
	if err != nil {
		return fmt.Errorf("windowsFDConn: OpenProcess(pid=%d): %w", w.targetPID, err)
	}
	defer windows.CloseHandle(targetProc)

	ourProc := windows.CurrentProcess()
	duplicated := make([]uintptr, len(fds))
	for i, src := range fds {
		var dup windows.Handle
		if err := windows.DuplicateHandle(
			ourProc,
			windows.Handle(src),
			targetProc,
			&dup,
			0, // dwDesiredAccess ignored when DUPLICATE_SAME_ACCESS is set
			false,
			windows.DUPLICATE_SAME_ACCESS,
		); err != nil {
			return fmt.Errorf("windowsFDConn: DuplicateHandle[%d]: %w", i, err)
		}
		duplicated[i] = uintptr(dup)
	}

	batch := windowsHandleBatch{
		Type:            msgHandleBatch,
		ProtocolVersion: HandoffProtocolVersion,
		Header:          header,
		Handles:         duplicated,
	}
	return w.WriteJSON(&batch)
}

// RecvFDs reads a windowsHandleBatch message from the pipe and returns the
// target-space handle values. On Windows, these handle values are already valid
// in the caller's address space — DuplicateHandle was done by the sender.
func (w *windowsFDConn) RecvFDs() ([]uintptr, []byte, error) {
	var batch windowsHandleBatch
	if err := w.ReadJSON(&batch); err != nil {
		return nil, nil, fmt.Errorf("windowsFDConn: read handle batch: %w", err)
	}
	if err := validateProtocolVersion(batch.ProtocolVersion); err != nil {
		return nil, nil, err
	}
	if batch.Type != msgHandleBatch {
		return nil, nil, fmt.Errorf("windowsFDConn: expected handle_batch, got %q", batch.Type)
	}
	return batch.Handles, batch.Header, nil
}

func (w *windowsFDConn) Close() error { return w.conn.Close() }

// listenHandoffWindows binds a named pipe and accepts one connection within
// the handoff window.
func listenHandoffWindows(pipeName string, acceptTimeout time.Duration) (fdConn, error) {
	ln, err := listenHandoffPipe(pipeName)
	if err != nil {
		return nil, fmt.Errorf("handoff listen win: %w", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	var conn net.Conn
	var acceptErr error
	go func() {
		defer close(done)
		conn, acceptErr = ln.Accept()
	}()

	select {
	case <-done:
		if acceptErr != nil {
			return nil, fmt.Errorf("handoff accept win: %w", acceptErr)
		}
		return newWindowsFDConn(conn), nil
	case <-time.After(acceptTimeout):
		_ = ln.Close()
		<-done
		return nil, fmt.Errorf("handoff accept win: timeout after %s", acceptTimeout)
	}
}

// dialHandoffWindows connects to a named pipe created by listenHandoffWindows.
func dialHandoffWindows(pipeName string, dialTimeout time.Duration) (fdConn, error) {
	conn, err := dialHandoffPipe(pipeName, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("handoff dial win: %w", err)
	}
	return newWindowsFDConn(conn), nil
}
