package mux

import "github.com/thebtf/mcp-mux/internal/muxcore/session"

// SessionContext is an alias for session.Context.
// Holds a session and its associated metadata.
type SessionContext = session.Context

// SessionManager is an alias for session.Manager.
// Tracks active sessions, inflight requests, and pending token registrations.
type SessionManager = session.Manager

// InflightEntry is an alias for session.InflightEntry.
// Represents an in-flight request that needs an error response.
type InflightEntry = session.InflightEntry

// NewSessionManager creates an empty SessionManager.
var NewSessionManager = session.NewManager
