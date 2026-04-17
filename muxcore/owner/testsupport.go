package owner

import (
	"io"
	"log"
	"net"

	"github.com/thebtf/mcp-mux/muxcore/progress"
)

// NewTestOwner returns a minimal Owner suitable for unit-test fixtures that
// need to exercise the public liveness API (IsAccepting, IsReachable, IPCPath,
// ServerID) without spinning up upstream processes or IPC listeners.
//
// The returned owner has an open listenerDone channel (so IsAccepting returns
// true) and the supplied ipcPath. Callers that want IsReachable to return
// false can either bind-and-close a listener at that path (zombie shape) or
// leave the path unbound (no-file shape). Callers that want IsReachable to
// return true should use NewTestOwnerWithListener.
//
// Intended for cross-package tests in the muxcore tree (primarily
// muxcore/daemon). Do NOT use in production code paths.
func NewTestOwner(ipcPath, serverID string) *Owner {
	return &Owner{
		sessions:               make(map[int]*Session),
		cachedInitSessions:     make(map[int]bool),
		progressOwners:         make(map[string]int),
		progressTokenRequestID: make(map[string]string),
		requestToTokens:        make(map[string][]string),
		progressTracker:        progress.NewTracker(),
		sessionMgr:             NewSessionManager(),
		ipcPath:                ipcPath,
		serverID:               serverID,
		logger:                 log.New(io.Discard, "", 0),
		done:                   make(chan struct{}),
		listenerDone:           make(chan struct{}),
	}
}

// NewTestOwnerWithListener is like NewTestOwner but also wires in a live
// net.Listener so IsReachable's dial probe succeeds. Used by tests that want
// to verify happy-path behaviour (spawn RPC reuses a healthy owner, restore
// sweep preserves a healthy owner, etc.).
//
// The caller owns the listener's lifetime and MUST Close() it during test
// cleanup to avoid leaking accept goroutines.
func NewTestOwnerWithListener(ipcPath, serverID string, ln net.Listener) *Owner {
	o := NewTestOwner(ipcPath, serverID)
	o.listener = ln
	return o
}
