package mux

import (
	"sync"
	"time"
)

// SessionContext holds a session and its associated working directory.
type SessionContext struct {
	Session *Session
	Cwd     string
}

// SessionManager tracks active sessions, their working directories,
// and in-flight upstream request correlations. It replaces the
// lastActiveSessionID heuristic with causal inflight correlation.
//
// Thread-safe: all methods are safe for concurrent use.
type SessionManager struct {
	sessions   map[int]*SessionContext // session ID → context
	inflight   map[string]int          // remapped request ID → session ID
	pending    map[string]string       // token → cwd (pre-registered, consumed on Bind)
	lastActive map[int]time.Time       // session ID → time of last TrackRequest call
	mu         sync.RWMutex
}

// NewSessionManager creates an empty SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions:   make(map[int]*SessionContext),
		inflight:   make(map[string]int),
		pending:    make(map[string]string),
		lastActive: make(map[int]time.Time),
	}
}

// RegisterSession adds a session with its working directory to the manager.
// If a session with the same ID already exists it is overwritten.
func (sm *SessionManager) RegisterSession(session *Session, cwd string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[session.ID] = &SessionContext{Session: session, Cwd: cwd}
}

// RemoveSession removes a session and all of its inflight entries.
func (sm *SessionManager) RemoveSession(sessionID int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	delete(sm.sessions, sessionID)
	delete(sm.lastActive, sessionID)

	// Clean up any inflight entries that belong to this session.
	for id, sid := range sm.inflight {
		if sid == sessionID {
			delete(sm.inflight, id)
		}
	}
}

// PreRegister stores a token→cwd mapping before the shim connects.
// Called by the daemon after generating a handshake token.
func (sm *SessionManager) PreRegister(token, cwd string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.pending[token] = cwd
}

// Bind resolves a token to a cwd and sets session.Cwd accordingly.
// Returns false if the token was not found (already consumed or never registered).
// The token is consumed on first use.
func (sm *SessionManager) Bind(token string, session *Session) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	cwd, ok := sm.pending[token]
	if !ok {
		return false
	}
	delete(sm.pending, token)

	session.Cwd = cwd

	// Update SessionContext if the session is already registered.
	if ctx, exists := sm.sessions[session.ID]; exists {
		ctx.Cwd = cwd
	}
	return true
}

// TrackRequest records that a remapped request ID belongs to a session.
// Also updates the session's lastActive timestamp for ResolveCallback tie-breaking.
func (sm *SessionManager) TrackRequest(remappedID string, sessionID int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.inflight[remappedID] = sessionID
	sm.lastActive[sessionID] = time.Now()
}

// CompleteRequest removes a remapped request ID from the inflight map.
func (sm *SessionManager) CompleteRequest(remappedID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.inflight, remappedID)
}

// ResolveCallback returns the SessionContext that should handle a server→client
// callback (roots/list, sampling, elicitation). Resolution logic:
//  1. Collect unique session IDs from the inflight map.
//  2. If exactly 1 → return it (deterministic).
//  3. If 0 → return nil (caller uses fallback).
//  4. If N > 1 → return the session with the most recent TrackRequest call.
func (sm *SessionManager) ResolveCallback() *SessionContext {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Collect unique session IDs that have inflight requests.
	seen := make(map[int]struct{})
	for _, sid := range sm.inflight {
		seen[sid] = struct{}{}
	}

	switch len(seen) {
	case 0:
		return nil
	case 1:
		for sid := range seen {
			return sm.sessions[sid]
		}
	}

	// N > 1: pick the session with the most recent TrackRequest timestamp.
	var bestID int
	var bestTime time.Time
	for sid := range seen {
		t := sm.lastActive[sid]
		if t.After(bestTime) {
			bestTime = t
			bestID = sid
		}
	}
	return sm.sessions[bestID]
}

// GetSession returns the SessionContext for a session ID, or nil if not found.
func (sm *SessionManager) GetSession(sessionID int) *SessionContext {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[sessionID]
}

// SessionCount returns the number of registered sessions.
func (sm *SessionManager) SessionCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}
