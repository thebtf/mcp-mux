package jsonrpc

import (
	"bufio"
	"fmt"
	"io"
)

// Scanner reads newline-delimited JSON-RPC messages from an io.Reader.
// Each line is expected to be a complete JSON object.
type Scanner struct {
	scanner *bufio.Scanner
}

// NewScanner creates a Scanner that reads from r.
// It uses a 1MB buffer to handle large messages.
func NewScanner(r io.Reader) *Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 1024*1024), 1024*1024)
	return &Scanner{scanner: s}
}

// Scan reads the next JSON-RPC message.
// Returns io.EOF when the reader is exhausted.
func (s *Scanner) Scan() (*Message, error) {
	for s.scanner.Scan() {
		line := s.scanner.Bytes()

		// Skip empty lines
		if len(line) == 0 {
			continue
		}

		msg, err := Parse(line)
		if err != nil {
			return nil, fmt.Errorf("jsonrpc scanner: %w", err)
		}

		return msg, nil
	}

	if err := s.scanner.Err(); err != nil {
		return nil, fmt.Errorf("jsonrpc scanner: %w", err)
	}

	return nil, io.EOF
}
