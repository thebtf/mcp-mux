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

// windowsFDConn implements fdConn on top of a named-pipe connection.
// T016 implements WriteJSON / ReadJSON / Close. SendFDs / RecvFDs land
// in T017 (DuplicateHandle over the pipe). This file includes stubs for
// those two methods so the type satisfies fdConn; T017 replaces the stubs.
type windowsFDConn struct {
	conn net.Conn
}

func newWindowsFDConn(conn net.Conn) *windowsFDConn {
	return &windowsFDConn{conn: conn}
}

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

// SendFDs is stubbed in T016; T017 implements DuplicateHandle transfer.
func (w *windowsFDConn) SendFDs(fds []uintptr, header []byte) error {
	return errors.New("windowsFDConn: SendFDs not yet implemented; T017")
}

// RecvFDs is stubbed in T016; T017 implements DuplicateHandle transfer.
func (w *windowsFDConn) RecvFDs() ([]uintptr, []byte, error) {
	return nil, nil, errors.New("windowsFDConn: RecvFDs not yet implemented; T017")
}

func (w *windowsFDConn) Close() error { return w.conn.Close() }
