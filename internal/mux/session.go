// Package mux implements the core multiplexer logic for mcp-mux.
package mux

import "github.com/thebtf/mcp-mux/internal/muxcore/session"

// Session is an alias for session.Session.
// Represents one downstream client connection.
type Session = session.Session

// NewSession creates a session from a reader/writer pair.
// For IPC sessions, pass the net.Conn as closer to enable forceful disconnect on shutdown.
var NewSession = session.NewSession

// NewSessionWithRawWriter creates a Session using w as-is (not wrapped in bufio.Writer).
// Intended for testing the non-buffered write path.
var NewSessionWithRawWriter = session.NewSessionWithRawWriter
