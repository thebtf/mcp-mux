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
	OwnerKey  string
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
	Session  *Session
}

// BoundHistorySnapshot is the serializable subset of consumed-token history
// needed to refresh reconnecting shims after daemon restart.
type BoundHistorySnapshot struct {
	Token    string
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
	now        func() time.Time
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
		now:        time.Now,
	}
}

func (sm *Manager) currentTime() time.Time {
	if sm.now != nil {
		return sm.now()
	}
	return time.Now()
}

func (sm *Manager) boundIsLiveLocked(hist *boundHistory) bool {
	if hist == nil || hist.Session == nil {
		return false
	}
	ctx := sm.sessions[hist.Session.ID]
	return ctx != nil && ctx.Session == hist.Session
}

func (sm *Manager) boundExpiredLocked(hist *boundHistory, now time.Time) bool {
	return hist == nil || (!sm.boundIsLiveLocked(hist) && now.Sub(hist.LastUsed) > boundTokenTTL)
}

// ExportBoundHistory returns non-expired consumed-token history entries for
// snapshot restore. Pending tokens and inflight request maps are deliberately
// not exported; reconnect only needs the previous consumed token.
func (sm *Manager) ExportBoundHistory() []BoundHistorySnapshot {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	now := sm.currentTime()
	entries := make([]BoundHistorySnapshot, 0, len(sm.bound))
	for token, hist := range sm.bound {
		if hist == nil || token == "" || hist.OwnerKey == "" {
			continue
		}
		live := sm.boundIsLiveLocked(hist)
		if !live && now.Sub(hist.LastUsed) > boundTokenTTL {
			continue
		}
		lastUsed := hist.LastUsed
		if live {
			// Session pointers are process-local. Renew the exported lease so
			// the successor can refresh a token that was live at snapshot time.
			lastUsed = now
		}
		entries = append(entries, BoundHistorySnapshot{
			Token:    token,
			OwnerKey: hist.OwnerKey,
			Cwd:      hist.Cwd,
			Env:      cloneEnv(hist.Env),
			BoundAt:  hist.BoundAt,
			LastUsed: lastUsed,
		})
	}
	return entries
}

// ImportBoundHistory restores consumed-token history from a daemon snapshot.
// Expired or malformed entries are ignored.
func (sm *Manager) ImportBoundHistory(entries []BoundHistorySnapshot) int {
	if len(entries) == 0 {
		return 0
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := sm.currentTime()
	imported := 0
	for _, entry := range entries {
		if entry.Token == "" || entry.OwnerKey == "" {
			continue
		}
		lastUsed := entry.LastUsed
		if lastUsed.IsZero() {
			lastUsed = entry.BoundAt
		}
		if lastUsed.IsZero() || now.Sub(lastUsed) > boundTokenTTL {
			continue
		}
		boundAt := entry.BoundAt
		if boundAt.IsZero() {
			boundAt = lastUsed
		}
		sm.bound[entry.Token] = &boundHistory{
			OwnerKey: entry.OwnerKey,
			Cwd:      entry.Cwd,
			Env:      cloneEnv(entry.Env),
			BoundAt:  boundAt,
			LastUsed: lastUsed,
		}
		imported++
	}
	return imported
}

// RegisterSession adds a session with its working directory to the manager.
// If a session with the same ID already exists it is overwritten.
func (sm *Manager) RegisterSession(session *Session, cwd string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if previous := sm.sessions[session.ID]; previous != nil && previous.Session != session {
		now := sm.currentTime()
		for _, hist := range sm.bound {
			if hist.Session == previous.Session {
				hist.Session = nil
				hist.LastUsed = now
			}
		}
	}
	sm.sessions[session.ID] = &Context{Session: session, Cwd: cwd}
}

// RemoveSession removes a session and all of its inflight entries.
func (sm *Manager) RemoveSession(sessionID int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	removed := sm.sessions[sessionID]
	delete(sm.sessions, sessionID)
	delete(sm.lastActive, sessionID)
	if removed != nil {
		now := sm.currentTime()
		for _, hist := range sm.bound {
			if hist.Session == removed.Session {
				hist.Session = nil
				// Inactive history expires from disconnect, not original bind.
				hist.LastUsed = now
			}
		}
	}

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
	sm.PreRegisterForOwner(token, "", cwd, env)
}

// PreRegisterForOwner stores a token→(owner,cwd,env) mapping before the shim connects.
// ownerKey is optional for legacy callers; owner-keyless pending tokens are
// cleaned only by TTL expiry, not by owner removal.
func (sm *Manager) PreRegisterForOwner(token, ownerKey, cwd string, env map[string]string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.pending[token] = &pendingSession{
		OwnerKey:  ownerKey,
		Cwd:       cwd,
		Env:       env,
		CreatedAt: sm.currentTime(),
	}
}

// RemovePendingForOwner removes pending tokens associated with ownerKey.
// Owner-keyless legacy pending tokens are intentionally TTL-only.
func (sm *Manager) RemovePendingForOwner(ownerKey string) int {
	return sm.RemovePendingForOwnerExcept(ownerKey, "")
}

// RemovePendingForOwnerExcept removes pending tokens associated with ownerKey,
// preserving keepToken when non-empty. Isolated classification uses this to
// revoke provisional fan-in without invalidating the creating shim's initial
// admission token.
func (sm *Manager) RemovePendingForOwnerExcept(ownerKey, keepToken string) int {
	if ownerKey == "" {
		return 0
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	removed := 0
	for token, ps := range sm.pending {
		if ps.OwnerKey == ownerKey && token != keepToken {
			delete(sm.pending, token)
			removed++
		}
	}
	return removed
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
	now := sm.currentTime()
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
	now := sm.currentTime()
	swept := 0
	for token, hist := range sm.bound {
		if sm.boundExpiredLocked(hist, now) {
			delete(sm.bound, token)
			swept++
		}
	}
	return swept
}

// RemoveBoundForOwner removes consumed-token reconnect history associated with ownerKey.
func (sm *Manager) RemoveBoundForOwner(ownerKey string) int {
	if ownerKey == "" {
		return 0
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	removed := 0
	for token, hist := range sm.bound {
		if hist.OwnerKey == ownerKey {
			delete(sm.bound, token)
			removed++
		}
	}
	return removed
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
	if ps.OwnerKey != "" && ps.OwnerKey != ownerKey {
		return false
	}
	delete(sm.pending, token)

	now := sm.currentTime()
	env := cloneEnv(ps.Env)
	historyOwnerKey := ps.OwnerKey
	if historyOwnerKey == "" {
		historyOwnerKey = ownerKey
	}
	sm.bound[token] = &boundHistory{
		OwnerKey: historyOwnerKey,
		Cwd:      ps.Cwd,
		Env:      env,
		BoundAt:  now,
		LastUsed: now,
		Session:  session,
	}

	session.Cwd = ps.Cwd
	session.Env = cloneEnv(env)

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
	if !exists || sm.boundExpiredLocked(hist, sm.currentTime()) {
		return "", "", nil, false
	}
	return hist.OwnerKey, hist.Cwd, cloneEnv(hist.Env), true
}

// RegisterReconnect mints a fresh pending token for a previously bound owner
// if that owner is still alive. The pending reservation is visible before
// ownerAlive runs so idle cleanup cannot remove the owner after validating it.
// ownerAlive is invoked without holding sm.mu.
func (sm *Manager) RegisterReconnect(prev string, ownerAlive func(key string) bool) (string, error) {
	sm.mu.Lock()
	hist, exists := sm.bound[prev]
	now := sm.currentTime()
	if !exists || sm.boundExpiredLocked(hist, now) {
		if exists {
			delete(sm.bound, prev)
		}
		sm.mu.Unlock()
		return "", ErrUnknownToken
	}
	token, err := generateToken()
	if err != nil {
		sm.mu.Unlock()
		return "", err
	}
	ownerKey := hist.OwnerKey
	cwd := hist.Cwd
	env := cloneEnv(hist.Env)
	sm.pending[token] = &pendingSession{
		OwnerKey:  ownerKey,
		Cwd:       cwd,
		Env:       cloneEnv(env),
		CreatedAt: now,
	}
	sm.mu.Unlock()

	if !ownerAlive(ownerKey) {
		sm.mu.Lock()
		delete(sm.pending, token)
		sm.mu.Unlock()
		return "", ErrOwnerGone
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	hist, exists = sm.bound[prev]
	now = sm.currentTime()
	if _, reserved := sm.pending[token]; !reserved {
		return "", ErrUnknownToken
	}
	if !exists || sm.boundExpiredLocked(hist, now) {
		delete(sm.pending, token)
		if exists {
			delete(sm.bound, prev)
		}
		return "", ErrUnknownToken
	}
	hist.LastUsed = now
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
	sm.lastActive[sessionID] = sm.currentTime()
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
