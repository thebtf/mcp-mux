package muxcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// ProjectContext identifies a CC session's project environment.
// Value object — safe to copy, store, compare by ID.
//
// ID is a deterministic hash of the worktree root (the directory containing .git).
// Same worktree = same ID across CC restarts. Different worktree = different ID.
// For linked worktrees (.git is a file), root = the worktree directory itself
// (NOT resolved back to main repo). No .git found = falls back to canonical CWD.
// CWD subdirectories within the same worktree produce the same ID.
type ProjectContext struct {
	// ID is deterministic from worktree root. Safe as persistent key.
	ID string
	// Cwd is the raw working directory of the CC session.
	Cwd string
	// Env contains per-session environment variables that differ from daemon.
	Env map[string]string
}

// SessionHandler processes MCP requests with project context.
// Owner calls HandleRequest concurrently from multiple goroutines.
// Implementations must be safe for concurrent use.
type SessionHandler interface {
	// HandleRequest processes one MCP JSON-RPC request and returns the response.
	// request contains original JSON-RPC (not remapped). ctx is cancelled when
	// the CC session disconnects or owner shuts down. Owner applies toolTimeout
	// as a context deadline safety net.
	HandleRequest(ctx context.Context, project ProjectContext, request []byte) (response []byte, err error)
}

// ProjectLifecycle is optionally implemented by SessionHandler.
// Owner calls these when CC sessions connect/disconnect.
type ProjectLifecycle interface {
	OnProjectConnect(project ProjectContext)
	OnProjectDisconnect(projectID string)
}

// NotificationHandler is optionally implemented by SessionHandler.
// Receives client-to-server notifications (e.g. notifications/cancelled).
// Handlers that don't implement this: notifications handled by Owner or dropped.
type NotificationHandler interface {
	HandleNotification(ctx context.Context, project ProjectContext, notification []byte)
}

// Notifier allows the handler to push notifications to specific CC sessions.
type Notifier interface {
	// Notify sends a JSON-RPC notification to a specific project session.
	// Returns error if projectID is unknown or disconnected.
	Notify(projectID string, notification []byte) error
	// Broadcast sends a JSON-RPC notification to ALL connected sessions.
	Broadcast(notification []byte)
}

// NotifierAware is optionally implemented by SessionHandler.
// Owner calls SetNotifier once before the first HandleRequest.
type NotifierAware interface {
	SetNotifier(n Notifier)
}

// ProjectContextID returns a deterministic session identifier from the given CWD.
// Uses WorktreeRoot to find the project root, then hashes it.
func ProjectContextID(cwd string) string {
	root := serverid.WorktreeRoot(cwd)
	h := sha256.Sum256([]byte(root))
	return hex.EncodeToString(h[:])[:16]
}
