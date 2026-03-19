// Package mux implements the core multiplexer logic for mcp-mux.
package mux

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/thebtf/mcp-mux/internal/jsonrpc"
)

var sessionCounter atomic.Int32

// Session represents one downstream client connection.
// It reads JSON-RPC requests and writes responses.
type Session struct {
	ID           int
	MuxSessionID string // unique session identifier for _meta injection (e.g., "sess_a1b2c3d4")
	reader       *jsonrpc.Scanner
	writer       io.Writer
	closer       io.Closer // underlying connection (net.Conn for IPC sessions, nil for stdio)
	mu           sync.Mutex // protects writer
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
	return &Session{
		ID:           int(sessionCounter.Add(1)),
		MuxSessionID: generateMuxSessionID(),
		reader:       jsonrpc.NewScanner(r),
		writer:       bufio.NewWriter(w),
		closer:       closer,
		done:         make(chan struct{}),
	}
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

// Done returns a channel that is closed when the session ends.
func (s *Session) Done() <-chan struct{} {
	return s.done
}
