// Package mux implements the core multiplexer logic for mcp-mux.
package mux

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/jsonrpc"
)

// writeTimeout is the maximum time a write to a session can block before
// the session is considered dead. Prevents one slow/stuck session from
// blocking the upstream reader goroutine and stalling all other sessions.
const writeTimeout = 30 * time.Second

var sessionCounter atomic.Int32

// notificationBufferSize is the capacity of the per-session async notification
// send channel. Notifications are queued here by broadcast() and drained by a
// dedicated goroutine, preventing the upstream reader from blocking on slow sessions.
const notificationBufferSize = 256

// Session represents one downstream client connection.
// It reads JSON-RPC requests and writes responses.
type Session struct {
	ID           int
	MuxSessionID string            // unique session identifier for _meta injection (e.g., "sess_a1b2c3d4")
	Cwd          string            // working directory bound via token handshake
	Env          map[string]string // per-session env diff bound via token handshake
	reader       *jsonrpc.Scanner
	writer       io.Writer
	conn         net.Conn  // underlying connection for write deadlines (nil for stdio)
	closer       io.Closer // underlying connection (net.Conn for IPC sessions, nil for stdio)
	mu           sync.Mutex // protects writer
	notifCh      chan []byte // async notification send buffer (drained by drainNotifications goroutine)
	done         chan struct{}
	closed       atomic.Bool
}

// NewSession creates a session from a reader/writer pair.
// For IPC sessions, pass the net.Conn as closer to enable forceful disconnect on shutdown.
func NewSession(r io.Reader, w io.Writer) *Session {
	var closer io.Closer
	if c, ok := r.(io.Closer); ok {
		closer = c
	}
	// Detect net.Conn for write deadline support (IPC sessions)
	var conn net.Conn
	if nc, ok := w.(net.Conn); ok {
		conn = nc
	}
	s := &Session{
		ID:           int(sessionCounter.Add(1)),
		MuxSessionID: generateMuxSessionID(),
		reader:       jsonrpc.NewScanner(r),
		writer:       bufio.NewWriter(w),
		conn:         conn,
		closer:       closer,
		notifCh:      make(chan []byte, notificationBufferSize),
		done:         make(chan struct{}),
	}
	go s.drainNotifications()
	return s
}

// generateMuxSessionID creates a unique session identifier: "sess_" + 8 hex chars.
func generateMuxSessionID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback to counter-based ID if crypto/rand fails
		return fmt.Sprintf("sess_%08x", sessionCounter.Load())
	}
	return "sess_" + hex.EncodeToString(b)
}

// ReadMessage reads the next JSON-RPC message from the downstream client.
// Returns io.EOF when the client disconnects.
func (s *Session) ReadMessage() (*jsonrpc.Message, error) {
	return s.reader.Scan()
}

// WriteRaw writes a raw JSON line to the downstream client, followed by newline.
func (s *Session) WriteRaw(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return fmt.Errorf("session %d: closed", s.ID)
	}

	// Set write deadline for IPC sessions to prevent one slow/stuck session
	// from blocking the upstream reader goroutine (which stalls ALL sessions).
	if s.conn != nil {
		_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		defer func() { _ = s.conn.SetWriteDeadline(time.Time{}) }()
	}

	bw, ok := s.writer.(*bufio.Writer)
	if ok {
		_, err := bw.Write(data)
		if err != nil {
			return fmt.Errorf("session %d: write: %w", s.ID, err)
		}
		_, err = bw.Write([]byte("\n"))
		if err != nil {
			return fmt.Errorf("session %d: write newline: %w", s.ID, err)
		}
		return bw.Flush()
	}

	_, err := s.writer.Write(data)
	if err != nil {
		return fmt.Errorf("session %d: write: %w", s.ID, err)
	}
	_, err = s.writer.Write([]byte("\n"))
	return err
}

// WriteMessage marshals a response with the given id and sends it.
func (s *Session) WriteMessage(id json.RawMessage, result interface{}) error {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  interface{}     `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("session %d: marshal: %w", s.ID, err)
	}

	return s.WriteRaw(data)
}

// Close marks the session as closed and disconnects the underlying connection.
// For IPC sessions, this closes the net.Conn, causing shim's io.Copy to return.
func (s *Session) Close() {
	if s.closed.CompareAndSwap(false, true) {
		close(s.done)
		if s.closer != nil {
			s.closer.Close()
		}
	}
}

// SendNotification queues a notification for async delivery. Never blocks the
// caller — if the buffer is full, the oldest notification is dropped to make room.
// This prevents the upstream reader goroutine from deadlocking when a session's
// IPC write is slow (e.g., CC not reading fast enough).
func (s *Session) SendNotification(data []byte) {
	if s.closed.Load() {
		return
	}
	// Make a copy — data may be reused by caller for other sessions
	cp := make([]byte, len(data))
	copy(cp, data)

	select {
	case s.notifCh <- cp:
	default:
		// Buffer full — drop oldest to make room (backpressure)
		select {
		case <-s.notifCh:
		default:
		}
		s.notifCh <- cp
	}
}

// drainNotifications reads from notifCh and writes to the session's IPC connection.
// Runs as a goroutine per session. Exits when the session closes.
func (s *Session) drainNotifications() {
	for {
		select {
		case data := <-s.notifCh:
			if err := s.WriteRaw(data); err != nil {
				// Write failed — session likely disconnected. Stop draining.
				return
			}
		case <-s.done:
			return
		}
	}
}

// Done returns a channel that is closed when the session ends.
func (s *Session) Done() <-chan struct{} {
	return s.done
}
