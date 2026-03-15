// Package mux implements the core multiplexer logic for mcp-mux.
package mux

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/bitswan-space/mcp-mux/internal/jsonrpc"
)

var sessionCounter atomic.Int32

// Session represents one downstream client connection.
// It reads JSON-RPC requests and writes responses.
type Session struct {
	ID     int
	reader *jsonrpc.Scanner
	writer io.Writer
	mu     sync.Mutex // protects writer
	done   chan struct{}
	closed atomic.Bool
}

// NewSession creates a session from a reader/writer pair.
func NewSession(r io.Reader, w io.Writer) *Session {
	return &Session{
		ID:     int(sessionCounter.Add(1)),
		reader: jsonrpc.NewScanner(r),
		writer: bufio.NewWriter(w),
		done:   make(chan struct{}),
	}
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

// Close marks the session as closed.
func (s *Session) Close() {
	if s.closed.CompareAndSwap(false, true) {
		close(s.done)
	}
}

// Done returns a channel that is closed when the session ends.
func (s *Session) Done() <-chan struct{} {
	return s.done
}
