package mux

import (
	"sync"
	"time"
)

// SessionContext holds a session and its associated metadata.
type SessionContext struct {
	Session *Session
	Cwd     string
	Env     map[string]string // per-session env diff (from project-scope .mcp.json)
}

// pendingSession holds pre-registered session data waiting for a shim to connect.
type pendingSession struct {
	Cwd       string
	Env       map[string]string
	CreatedAt time.Time // for TTL expiry — orphan tokens are swept if shim never connects
}

// pendingTokenTTL is the maximum time a pre-registered token lives before
// being swept. Must be long enough to cover slow CC startup + daemon spawn
// + shim dial, but short enough to prevent memory leaks from orphan spawns.
const pendingTokenTTL = 2 * time.Minute

// SessionManager tracks active sessions, their working directories,
// and in-flight upstream request correlations. It replaces the
// lastActiveSessionID heuristic with causal inflight correlation.
//
// Thread-safe: all methods are safe for concurrent use.
type SessionManager struct {
	sessions   map[int]*SessionContext // session ID → context
	inflight   map[string]int          // remapped request ID → session ID
	pending    map[string]*pendingSession // token → session data (pre-registered, consumed on Bind)
	lastActive map[int]time.Time       // session ID → time of last TrackRequest call
	mu         sync.RWMutex
}

// NewSessionManager creates an empty SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions:   make(map[int]*SessionContext),
		inflight:   make(map[string]int),
		pending:    make(map[string]*pendingSession),
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

// PreRegister stores a token→(cwd,env) mapping before the shim connects.
// Called by the daemon after generating a handshake token.
// Pending entries have a TTL (pendingTokenTTL) — orphan tokens from failed
// spawns are swept by SweepExpiredPending to prevent unbounded growth.
func (sm *SessionManager) PreRegister(token, cwd string, env map[string]string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.pending[token] = &pendingSession{
		Cwd:       cwd,
		Env:       env,
		CreatedAt: time.Now(),
	}
}

// SweepExpiredPending removes pending tokens older than pendingTokenTTL.
// Returns the number of swept entries. Called periodically by the daemon
// to prevent memory leaks from orphan tokens (shim never connected after
// Spawn — e.g., CC killed the process before shim could dial IPC).
func (sm *SessionManager) SweepExpiredPending() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := time.Now()
	swept := 0
	for token, ps := range sm.pending {
		if now.Sub(ps.CreatedAt) > pendingTokenTTL {
			delete(sm.pending, token)
			swept++
		}
	}
	return swept
}

// PendingCount returns the number of pending (pre-registered but not yet bound) tokens.
// Exposed for observability (mux_list --verbose).
func (sm *SessionManager) PendingCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.pending)
}

// Bind resolves a token to session metadata (cwd, env) and sets them on the session.
// Returns false if the token was not found (already consumed or never registered).
// The token is consumed on first use.
func (sm *SessionManager) Bind(token string, session *Session) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ps, ok := sm.pending[token]
	if !ok {
		return false
	}
	delete(sm.pending, token)

	session.Cwd = ps.Cwd
	session.Env = ps.Env

	// Update SessionContext if the session is already registered.
	if ctx, exists := sm.sessions[session.ID]; exists {
		ctx.Cwd = ps.Cwd
		ctx.Env = ps.Env
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

// InflightEntry represents an in-flight request that needs an error response.
type InflightEntry struct {
	RemappedID string
	SessionID  int
}

// DrainInflight removes all in-flight requests and returns them.
// Used when upstream dies to send error responses to originating sessions.
func (sm *SessionManager) DrainInflight() []InflightEntry {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	entries := make([]InflightEntry, 0, len(sm.inflight))
	for remappedID, sessionID := range sm.inflight {
		entries = append(entries, InflightEntry{RemappedID: remappedID, SessionID: sessionID})
	}
	sm.inflight = make(map[string]int)
	return entries
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
