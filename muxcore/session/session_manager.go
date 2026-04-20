package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Context holds a session and its associated metadata.
type Context struct {
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

type boundHistory struct {
	OwnerKey string
	Cwd      string
	Env      map[string]string
	BoundAt  time.Time
	LastUsed time.Time
}

// pendingTokenTTL is the maximum time a pre-registered token lives before
// being swept. Must be long enough to cover slow CC startup + daemon spawn
// + shim dial, but short enough to prevent memory leaks from orphan spawns.
const pendingTokenTTL = 2 * time.Minute

const boundTokenTTL = 30 * time.Minute

// ErrUnknownToken indicates that a reconnect was requested for a token the
// manager has never seen or has already expired from history.
var ErrUnknownToken = errors.New("unknown token")

// ErrOwnerGone indicates that a reconnect was requested for a known token, but
// the original owner is no longer alive enough to accept a fresh bind.
var ErrOwnerGone = errors.New("owner gone")

// Manager tracks active sessions, their working directories,
// and in-flight upstream request correlations. It replaces the
// lastActiveSessionID heuristic with causal inflight correlation.
//
// Thread-safe: all methods are safe for concurrent use.
type Manager struct {
	sessions   map[int]*Context           // session ID → context
	inflight   map[string]int             // remapped request ID → session ID
	pending    map[string]*pendingSession // token → session data (pre-registered, consumed on Bind)
	bound      map[string]*boundHistory   // consumed token → owner/session history for reconnect refresh
	lastActive map[int]time.Time          // session ID → time of last TrackRequest call
	mu         sync.RWMutex
}

// NewManager creates an empty Manager.
func NewManager() *Manager {
	return &Manager{
		sessions:   make(map[int]*Context),
		inflight:   make(map[string]int),
		pending:    make(map[string]*pendingSession),
		bound:      make(map[string]*boundHistory),
		lastActive: make(map[int]time.Time),
	}
}

// RegisterSession adds a session with its working directory to the manager.
// If a session with the same ID already exists it is overwritten.
func (sm *Manager) RegisterSession(session *Session, cwd string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[session.ID] = &Context{Session: session, Cwd: cwd}
}

// RemoveSession removes a session and all of its inflight entries.
func (sm *Manager) RemoveSession(sessionID int) {
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
func (sm *Manager) PreRegister(token, cwd string, env map[string]string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.pending[token] = &pendingSession{
		Cwd:       cwd,
		Env:       cloneEnv(env),
		CreatedAt: time.Now(),
	}
}

// IsPreRegistered reports whether the given token has been pre-registered but
// not yet consumed by a successful Bind. This is a side-effect-free read used
// by Owner.acceptLoop (FR-28) to gate connections before session construction.
//
// Rejection does NOT consume the token (C2): only a successful Bind does. This
// allows transient-failure retry on the legitimate client path (e.g., client
// closes socket mid-handshake and reconnects) without forcing re-registration
// via the control socket.
func (sm *Manager) IsPreRegistered(token string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, ok := sm.pending[token]
	return ok
}

// SweepExpiredPending removes pending tokens older than pendingTokenTTL.
// Returns the number of swept entries. Called periodically by the daemon
// to prevent memory leaks from orphan tokens (shim never connected after
// Spawn — e.g., CC killed the process before shim could dial IPC).
func (sm *Manager) SweepExpiredPending() int {
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

// SweepExpiredBound removes consumed-token reconnect history older than
// boundTokenTTL since the last successful use. Returns the number of swept
// entries.
func (sm *Manager) SweepExpiredBound() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := time.Now()
	swept := 0
	for token, hist := range sm.bound {
		if now.Sub(hist.LastUsed) > boundTokenTTL {
			delete(sm.bound, token)
			swept++
		}
	}
	return swept
}

// PendingCount returns the number of pending (pre-registered but not yet bound) tokens.
// Exposed for observability (mux_list --verbose).
func (sm *Manager) PendingCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.pending)
}

// Bind resolves a token to session metadata (cwd, env) and sets them on the session.
// Returns false if the token was not found (already consumed or never registered).
// The token is consumed on first use.
func (sm *Manager) Bind(token, ownerKey string, session *Session) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ps, ok := sm.pending[token]
	if !ok {
		return false
	}
	delete(sm.pending, token)

	now := time.Now()
	env := cloneEnv(ps.Env)
	sm.bound[token] = &boundHistory{
		OwnerKey: ownerKey,
		Cwd:      ps.Cwd,
		Env:      env,
		BoundAt:  now,
		LastUsed: now,
	}

	session.Cwd = ps.Cwd
	session.Env = cloneEnv(env)

	// Update Context if the session is already registered.
	if ctx, exists := sm.sessions[session.ID]; exists {
		ctx.Cwd = ps.Cwd
		ctx.Env = cloneEnv(env)
	}
	return true
}

// LookupHistory returns reconnect history for a previously bound token.
// Returns ok=false when the token is unknown or expired.
func (sm *Manager) LookupHistory(prev string) (ownerKey, cwd string, env map[string]string, ok bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	hist, exists := sm.bound[prev]
	if !exists {
		return "", "", nil, false
	}
	if time.Since(hist.LastUsed) > boundTokenTTL {
		return "", "", nil, false
	}
	return hist.OwnerKey, hist.Cwd, cloneEnv(hist.Env), true
}

// RegisterReconnect mints a fresh pending token for a previously bound owner
// if that owner is still alive. ownerAlive is invoked without holding sm.mu.
func (sm *Manager) RegisterReconnect(prev string, ownerAlive func(key string) bool) (string, error) {
	sm.mu.Lock()
	hist, exists := sm.bound[prev]
	if !exists || time.Since(hist.LastUsed) > boundTokenTTL {
		if exists && time.Since(hist.LastUsed) > boundTokenTTL {
			delete(sm.bound, prev)
		}
		sm.mu.Unlock()
		return "", ErrUnknownToken
	}
	ownerKey := hist.OwnerKey
	cwd := hist.Cwd
	env := cloneEnv(hist.Env)
	sm.mu.Unlock()

	if !ownerAlive(ownerKey) {
		return "", ErrOwnerGone
	}

	token, err := generateToken()
	if err != nil {
		return "", err
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	hist, exists = sm.bound[prev]
	if !exists || time.Since(hist.LastUsed) > boundTokenTTL {
		if exists && time.Since(hist.LastUsed) > boundTokenTTL {
			delete(sm.bound, prev)
		}
		return "", ErrUnknownToken
	}
	now := time.Now()
	hist.LastUsed = now
	sm.pending[token] = &pendingSession{
		Cwd:       cwd,
		Env:       cloneEnv(env),
		CreatedAt: now,
	}
	sm.bound[token] = &boundHistory{
		OwnerKey: ownerKey,
		Cwd:      cwd,
		Env:      cloneEnv(env),
		BoundAt:  now,
		LastUsed: now,
	}
	return token, nil
}

// TrackRequest records that a remapped request ID belongs to a session.
// Also updates the session's lastActive timestamp for ResolveCallback tie-breaking.
func (sm *Manager) TrackRequest(remappedID string, sessionID int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.inflight[remappedID] = sessionID
	sm.lastActive[sessionID] = time.Now()
}

// CompleteRequest removes a remapped request ID from the inflight map.
func (sm *Manager) CompleteRequest(remappedID string) {
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
func (sm *Manager) DrainInflight() []InflightEntry {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	entries := make([]InflightEntry, 0, len(sm.inflight))
	for remappedID, sessionID := range sm.inflight {
		entries = append(entries, InflightEntry{RemappedID: remappedID, SessionID: sessionID})
	}
	sm.inflight = make(map[string]int)
	return entries
}

// ResolveCallback returns the Context that should handle a server→client
// callback (roots/list, sampling, elicitation). Resolution logic:
//  1. Collect unique session IDs from the inflight map.
//  2. If exactly 1 → return it (deterministic).
//  3. If 0 → return nil (caller uses fallback).
//  4. If N > 1 → return the session with the most recent TrackRequest call.
func (sm *Manager) ResolveCallback() *Context {
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

// GetSession returns the Context for a session ID, or nil if not found.
func (sm *Manager) GetSession(sessionID int) *Context {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[sessionID]
}

// SessionCount returns the number of registered sessions.
func (sm *Manager) SessionCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	cloned := make(map[string]string, len(env))
	for k, v := range env {
		cloned[k] = v
	}
	return cloned
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
