// Package daemon implements a global daemon that manages all upstream MCP server
// processes. CC sessions connect as thin shims via IPC; the daemon handles
// lifecycle, GC, reaping, health monitoring, and persistence.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thebtf/mcp-mux/internal/control"
	"github.com/thebtf/mcp-mux/internal/mux"
	"github.com/thebtf/mcp-mux/internal/serverid"
	"github.com/thejerf/suture/v4"
)

// OwnerEntry tracks a single managed owner and its metadata.
// When creating != nil, the entry is a placeholder: Owner is nil and is being
// created by another goroutine. Waiters must read creating under d.mu, then
// release d.mu, block on <-creating, re-acquire d.mu, and re-check Owner.
type OwnerEntry struct {
	Owner       *mux.Owner
	ServerID    string
	Command     string
	Args        []string
	Cwd         string
	Mode        string
	Env         map[string]string
	Persistent  bool
	LastSession time.Time
	// IdleTimeout is the effective idle timeout for this owner (daemon
	// default or per-owner x-mux.idleTimeout override). The reaper uses
	// this to decide whether an idle owner is eligible for removal.
	// Replaces v0.10.x GracePeriod.
	IdleTimeout time.Duration
	// serviceToken is the suture.ServiceToken returned by supervisor.Add.
	// Used to remove the owner from the supervisor on Remove/Shutdown.
	// In-memory only — not serialized to snapshots.
	serviceToken suture.ServiceToken
	// creating is closed when Owner transitions from nil (placeholder) to a real
	// owner.  It is non-nil only while the placeholder is being created.
	creating chan struct{}
}

// Daemon manages N owners, handles spawn/remove, and implements control.DaemonHandler.
type Daemon struct {
	mu      sync.RWMutex
	owners  map[string]*OwnerEntry
	logger  *log.Logger
	ctlSrv  *control.Server
	done    chan struct{}

	// ownerIdleTimeout is the default time an owner may sit with no activity
	// (no MCP traffic, no sessions, no pending requests, no active progress
	// tokens, no busy declarations) before the reaper removes it. Default 10m.
	// Overridable per-owner via x-mux.idleTimeout capability.
	ownerIdleTimeout time.Duration
	idleTimeout      time.Duration // daemon-level auto-exit timeout (zero owners + zero sessions)
	templateCache    map[string]mux.OwnerSnapshot // command+args key → cached init data

	// supervisor manages owner lifecycle with exponential backoff on restart.
	// Owners are added via supervisor.Add in Spawn() and removed via
	// supervisor.Remove in daemon.Remove. Context-cancelled on Shutdown.
	supervisor    *suture.Supervisor
	supervisorCtx context.Context
	supervisorCancel context.CancelFunc
	supervisorErr <-chan error

	shutdownOnce sync.Once
}

// Config holds daemon startup parameters.
type Config struct {
	// ControlPath is the daemon's control socket path.
	ControlPath string

	// OwnerIdleTimeout is how long an owner may be idle (no MCP traffic, no
	// sessions, no pending JSON-RPC requests, no active progress tokens, no
	// busy declarations) before the reaper removes it. Default: 10 minutes.
	// Overridden by MCP_MUX_OWNER_IDLE env var and per-owner via the
	// x-mux.idleTimeout capability in the upstream initializeResult.
	// v0.10.x used GracePeriod (default 30s); replaced in v0.11.0 because
	// the grace-period semantic killed stateful async work that didn't
	// emit pending_requests (e.g. aimux background jobs).
	OwnerIdleTimeout time.Duration

	// GracePeriod is a v0.10.x legacy alias for OwnerIdleTimeout. Kept for
	// callers that haven't migrated. Ignored if OwnerIdleTimeout is set.
	//
	// Deprecated: use OwnerIdleTimeout.
	GracePeriod time.Duration

	// IdleTimeout is how long the daemon waits with zero owners before auto-exiting.
	// Default: 5 minutes.
	IdleTimeout time.Duration

	Logger *log.Logger

	// SkipSnapshot disables snapshot loading on startup. Used by tests
	// to prevent cross-test interference from stale snapshot files.
	SkipSnapshot bool
}

// New creates and starts a new Daemon with a control server.
func New(cfg Config) (*Daemon, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "[mcp-muxd] ", log.LstdFlags)
	}

	// Resolve the owner idle timeout with legacy fallback.
	// Priority: OwnerIdleTimeout → GracePeriod (legacy alias) → 10m default.
	ownerIdleTimeout := cfg.OwnerIdleTimeout
	if ownerIdleTimeout == 0 {
		ownerIdleTimeout = cfg.GracePeriod
	}
	if ownerIdleTimeout == 0 {
		ownerIdleTimeout = 10 * time.Minute
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = 5 * time.Minute
	}

	supCtx, supCancel := context.WithCancel(context.Background())
	d := &Daemon{
		owners:           make(map[string]*OwnerEntry),
		logger:           logger,
		done:             make(chan struct{}),
		ownerIdleTimeout: ownerIdleTimeout,
		idleTimeout:      idleTimeout,
		templateCache:    make(map[string]mux.OwnerSnapshot),
		supervisorCtx:    supCtx,
		supervisorCancel: supCancel,
	}

	// Create supervisor with exponential backoff on restart storms.
	// Tuning rationale:
	//   FailureDecay=30s    — old failures fade from the rate counter after 30s
	//   FailureThreshold=5  — 5 failures in 30s → permanent failure (stop retrying)
	//   FailureBackoff=15s  — wait 15s between restart attempts after threshold hit
	// These are sensible defaults inherited from suture. If an upstream crashes
	// 5 times in 30 seconds, we stop retrying — something is fundamentally broken
	// and endless retry would just spam logs (the bug we fixed in v0.9.2).
	d.supervisor = suture.New("mcp-mux-daemon", suture.Spec{
		EventHook: d.supervisorEventHook,
	})

	ctlSrv, err := control.NewServer(cfg.ControlPath, d, logger)
	if err != nil {
		// Cancel supervisor context to prevent leak of the context goroutine.
		supCancel()
		return nil, fmt.Errorf("daemon: control server: %w", err)
	}
	d.ctlSrv = ctlSrv
	logger.Printf("daemon started, control socket: %s", cfg.ControlPath)

	// Clean up stale socket files from previous daemon crashes/kills.
	cleaned := cleanStaleSockets(logger)
	if cleaned > 0 {
		logger.Printf("startup: cleaned %d stale socket files", cleaned)
	}

	// Load snapshot from graceful restart (if available)
	if !cfg.SkipSnapshot {
		if restored := d.loadSnapshot(); restored > 0 {
			logger.Printf("startup: restored %d owners from snapshot", restored)
		}
	}

	// Start supervisor AFTER snapshot load so restored owners are already added.
	// ServeBackground returns a channel that will receive the final error when
	// the supervisor exits (via context cancel or root termination).
	d.supervisorErr = d.supervisor.ServeBackground(d.supervisorCtx)

	return d, nil
}

// supervisorEventHook receives lifecycle events from the suture supervisor:
// service failures, restarts, backoffs, and permanent failures. Logs them
// for observability and debugging.
func (d *Daemon) supervisorEventHook(event suture.Event) {
	switch e := event.(type) {
	case suture.EventServicePanic:
		d.logger.Printf("supervisor: service %q PANIC: %v", e.ServiceName, e.PanicMsg)
	case suture.EventServiceTerminate:
		d.logger.Printf("supervisor: service %q terminated: %v (restarting=%v)",
			e.ServiceName, e.Err, e.Restarting)
	case suture.EventBackoff:
		d.logger.Printf("supervisor: backoff — too many failures, slowing restart rate")
	case suture.EventResume:
		d.logger.Printf("supervisor: resume — resuming normal operation after backoff")
	case suture.EventStopTimeout:
		d.logger.Printf("supervisor: service %q did not stop in time", e.ServiceName)
	default:
		// Unknown event type — ignore silently
	}
}

// cleanStaleSockets removes mcp-mux-*.ctl.sock and mcp-mux-*.sock files from
// the temp directory that are not reachable (leftover from daemon crash/kill).
func cleanStaleSockets(logger *log.Logger) int {
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return 0
	}
	cleaned := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "mcp-mux-") {
			continue
		}
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		path := filepath.Join(tmpDir, name)
		// Try to connect — if unreachable, it's stale
		if strings.HasSuffix(name, ".ctl.sock") {
			if _, err := control.Send(path, control.Request{Cmd: "ping"}); err != nil {
				os.Remove(path)
				cleaned++
			}
		} else {
			// IPC data socket — check if the corresponding .ctl.sock exists and is alive
			ctlName := strings.TrimSuffix(name, ".sock") + ".ctl.sock"
			ctlPath := filepath.Join(tmpDir, ctlName)
			if _, err := control.Send(ctlPath, control.Request{Cmd: "ping"}); err != nil {
				os.Remove(path)
				cleaned++
			}
		}
	}
	return cleaned
}

// generateToken creates a 16-character hex handshake token (8 random bytes).
func generateToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use a deterministic but unique value.
		return hex.EncodeToString([]byte(fmt.Sprintf("%016x", time.Now().UnixNano())))
	}
	return hex.EncodeToString(b)
}

// Spawn creates or returns an existing owner for the given server identity.
// Deduplication: if a shared owner for the same command+args already exists
// templateKey returns a cache key based on command+args only (ignoring cwd/env).
// All instances of the same server share identical init/tools responses.
func templateKey(command string, args []string) string {
	return serverid.GenerateContextKey(serverid.ModeGlobal, command, args, nil, "")
}

// updateTemplate stores an owner's cached state as a template for future isolated spawns.
func (d *Daemon) updateTemplate(command string, args []string, snap mux.OwnerSnapshot) {
	key := templateKey(command, args)
	d.mu.Lock()
	d.templateCache[key] = snap
	d.mu.Unlock()
	d.logger.Printf("template cache updated for %s (key=%s)", command, key[:8])
}

// getTemplate returns a cached template for the given command+args, if available.
func (d *Daemon) getTemplate(command string, args []string) (mux.OwnerSnapshot, bool) {
	key := templateKey(command, args)
	d.mu.RLock()
	snap, ok := d.templateCache[key]
	d.mu.RUnlock()
	return snap, ok
}

// (from any cwd), it is reused — stateless servers don't need per-project copies.
// Returns the IPC path, server ID, and a one-time handshake token for session binding.
//
// Concurrent spawns for the same sid are serialised via a placeholder entry whose
// creating channel is closed once the real owner is available (or creation fails).
func (d *Daemon) Spawn(req control.Request) (string, string, string, error) {
	mode := serverid.ModeCwd
	switch req.Mode {
	case "global":
		mode = serverid.ModeGlobal
	case "isolated":
		mode = serverid.ModeIsolated
	case "cwd", "":
		mode = serverid.ModeCwd
	}

	// Generate handshake token upfront — valid for this spawn call only.
	token := generateToken()

	// Server identity is based on command+args+cwd only, NOT env.
	sid := serverid.GenerateContextKey(mode, req.Command, req.Args, nil, req.Cwd)

	d.mu.Lock()

	// 1. Exact match (same command+args+cwd)?
	if entry, ok := d.owners[sid]; ok {
		if entry.creating != nil {
			// Another goroutine is creating this owner — wait with timeout.
			creating := entry.creating
			d.mu.Unlock()
			select {
			case <-creating:
			case <-time.After(30 * time.Second):
				d.logger.Printf("timeout waiting for placeholder %s, creating new", sid[:8])
				return d.Spawn(req) // recursive — will create new placeholder
			}
			d.mu.Lock()
			// Re-check: creation may have succeeded or failed.
			if e, still := d.owners[sid]; still && e.Owner != nil && e.Owner.IsAccepting() {
				e.LastSession = time.Now()
				d.mu.Unlock()
				e.Owner.SessionMgr().PreRegister(token, req.Cwd, req.Env)
				d.logger.Printf("reusing owner %s for %s (waited for concurrent create)", sid[:8], req.Command)
				return e.Owner.IPCPath(), sid, token, nil
			}
			// Creation failed or entry was removed — fall through to create anew.
			d.mu.Unlock()
			return d.Spawn(req)
		}
		if entry.Owner.IsAccepting() {
			entry.LastSession = time.Now()
			d.mu.Unlock()
			entry.Owner.SessionMgr().PreRegister(token, req.Cwd, req.Env)
			// Note: no log here — this path is the hot path (every CC session reconnect).
			// Logging each reuse produced 500+ lines/minute during multi-session incidents.
			return entry.Owner.IPCPath(), sid, token, nil
		}
		// Owner exists but IPC listener is closed (isolated server).
		// If owner still has active sessions (in-flight requests), DON'T kill it —
		// that would break the active session's pipe mid-request (BrokenResourceError).
		// Leave the old owner alive and fall through to create a NEW isolated owner
		// with a fresh server ID.
		if entry.Owner.SessionCount() > 0 {
			d.logger.Printf("owner %s not accepting but has %d active sessions, leaving alive",
				sid[:8], entry.Owner.SessionCount())
			// DON'T delete or shutdown. Fall through — new owner will get a unique
			// isolated ID from serverid.ModeIsolated below.
			d.mu.Unlock()
			// Force isolated mode for the new spawn so it gets a unique server ID
			req.Mode = "isolated"
			return d.Spawn(req)
		}
		entry.Owner.Shutdown()
		delete(d.owners, sid)
		d.logger.Printf("owner %s not accepting (isolated, 0 sessions), re-spawning", sid[:8])
	}

	// 2. Global dedup: if an accepting owner for same command+args exists (any cwd), reuse it.
	//    Dedup is optimistic: unclassified owners are assumed shared/session-aware.
	//    If the owner later classifies as isolated, it closes its listener — extra
	//    sessions get EOF and reconnect, spawning their own owner.
	if mode == serverid.ModeCwd {
		if existing := d.findSharedOwner(req.Command, req.Args, req.Env); existing != nil {
			existing.LastSession = time.Now()
			existingSID := existing.ServerID
			d.mu.Unlock()
			if req.Cwd != "" {
				// AddCwd itself logs only when a new canonical cwd is added.
				// Dedup hot path is silent — logging every reuse produced 500+ lines/minute.
				existing.Owner.AddCwd(req.Cwd)
			}
			existing.Owner.SessionMgr().PreRegister(token, req.Cwd, req.Env)
			return existing.Owner.IPCPath(), existingSID, token, nil
		}
	}

	// Reserve the slot with a placeholder before releasing d.mu.
	// Any concurrent goroutine that arrives for the same sid will wait on the
	// creating channel instead of racing to spawn a duplicate owner.
	placeholder := &OwnerEntry{
		ServerID: sid,
		Command:  req.Command,
		Args:     req.Args,
		Cwd:      req.Cwd,
		creating: make(chan struct{}),
	}
	d.owners[sid] = placeholder
	d.mu.Unlock()

	ipcPath := serverid.IPCPath(sid)

	// Compute env diff: only vars that the shim has but daemon doesn't (CC-configured vars).
	envDiff := diffEnv(req.Env)
	if len(envDiff) > 0 {
		keys := make([]string, 0, len(envDiff))
		for k := range envDiff {
			keys = append(keys, k)
		}
		d.logger.Printf("owner %s: env diff %d vars: %v", sid[:8], len(envDiff), keys)
	}

	// Build the shared owner config (used by both template and fresh paths).
	controlPath := serverid.ControlPath(sid)
	ownerCfg := mux.OwnerConfig{
		Command:        req.Command,
		Args:           req.Args,
		Env:            envDiff,
		Cwd:            req.Cwd,
		IPCPath:        ipcPath,
		ControlPath:    controlPath,
		ServerID:       sid,
		TokenHandshake: true, // daemon-managed owners: shims send a handshake token
		OnZeroSessions: func(serverID string) {
			d.onZeroSessions(serverID)
		},
		OnUpstreamExit: func(serverID string) {
			d.onUpstreamExit(serverID)
		},
		OnPersistentDetected: func(serverID string) {
			d.SetPersistent(serverID, true)
		},
		OnCacheReady: func(serverID string) {
			d.mu.RLock()
			entry, ok := d.owners[serverID]
			d.mu.RUnlock()
			if !ok || entry.Owner == nil {
				return
			}
			snap := entry.Owner.ExportSnapshot()
			d.updateTemplate(req.Command, req.Args, snap)
		},
		Logger: log.New(d.logger.Writer(), fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags|log.Lmicroseconds),
	}

	// Try template-based spawn: if the daemon has seen this server before,
	// create the owner from cached init data (instant response to CC) and
	// start the real upstream process in the background.
	var owner *mux.Owner
	var err error
	fromTemplate := false
	if tmpl, ok := d.getTemplate(req.Command, req.Args); ok {
		// Adapt template for this specific owner instance
		tmpl.ServerID = sid
		tmpl.Cwd = req.Cwd
		tmpl.CwdSet = []string{req.Cwd}
		tmpl.Env = envDiff
		tmpl.Mode = req.Mode

		owner, err = mux.NewOwnerFromSnapshot(ownerCfg, tmpl)
		if err != nil {
			d.logger.Printf("template spawn failed for %s: %v, falling back to fresh spawn", sid[:8], err)
			owner = nil // fall through to fresh spawn
		} else {
			fromTemplate = true
			d.logger.Printf("spawned owner %s from template cache (instant init) for %s", sid[:8], req.Command)
		}
	}

	// Fresh spawn: no template available, or template spawn failed.
	if owner == nil {
		owner, err = mux.NewOwner(ownerCfg)
		if err != nil {
			// Remove the placeholder and unblock any waiters.
			d.mu.Lock()
			if d.owners[sid] == placeholder {
				delete(d.owners, sid)
			}
			close(placeholder.creating)
			d.mu.Unlock()
			return "", "", "", fmt.Errorf("spawn %s: %w", req.Command, err)
		}
		d.logger.Printf("spawned owner %s for %s %v (cold start)", sid[:8], req.Command, req.Args)
	}

	// Register owner with the supervisor for lifecycle management.
	// Suture will call owner.Serve(ctx) in its own goroutine and handle
	// restart with exponential backoff if Serve returns an error.
	serviceToken := d.supervisor.Add(owner)

	// Promote the placeholder to a real entry and signal waiters.
	d.mu.Lock()
	placeholder.Owner = owner
	placeholder.Mode = req.Mode
	placeholder.Env = req.Env
	placeholder.LastSession = time.Now()
	placeholder.IdleTimeout = d.ownerIdleTimeout
	placeholder.serviceToken = serviceToken
	close(placeholder.creating)
	placeholder.creating = nil // no longer a placeholder
	d.mu.Unlock()

	// For template-spawned owners, start the upstream process in the background.
	// The owner already serves cached responses; upstream refreshes caches when ready.
	if fromTemplate {
		owner.SpawnUpstreamBackground()
	}

	owner.SessionMgr().PreRegister(token, req.Cwd, req.Env)
	return ipcPath, sid, token, nil
}

// Remove shuts down and removes an owner by server ID.
func (d *Daemon) Remove(serverID string) error {
	d.mu.Lock()
	entry, ok := d.owners[serverID]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("server %s not found", serverID)
	}
	if entry.Owner == nil {
		// Placeholder still being created — do not remove; caller should retry later.
		d.mu.Unlock()
		return fmt.Errorf("server %s is still being created", serverID)
	}
	delete(d.owners, serverID)
	token := entry.serviceToken
	d.mu.Unlock()

	// Remove from supervisor BEFORE Shutdown to prevent suture from
	// interpreting the shutdown as a failure and attempting restart.
	// Use RemoveAndWait with a short timeout to avoid blocking forever
	// if the service is stuck. We report the error to the caller but
	// still proceed with Owner.Shutdown to avoid leaking resources.
	var supErr error
	if d.supervisor != nil {
		if err := d.supervisor.RemoveAndWait(token, 2*time.Second); err != nil {
			supErr = fmt.Errorf("remove owner %s from supervisor: %w", serverID[:8], err)
			d.logger.Printf("warning: %v", supErr)
		}
	}
	entry.Owner.Shutdown()
	d.logger.Printf("removed owner %s", serverID[:8])
	return supErr
}

// HandleSpawn implements control.DaemonHandler.
func (d *Daemon) HandleSpawn(req control.Request) (string, string, string, error) {
	return d.Spawn(req)
}

// HandleRemove implements control.DaemonHandler.
func (d *Daemon) HandleRemove(serverID string) error {
	return d.Remove(serverID)
}

// HandleShutdown implements control.CommandHandler.
func (d *Daemon) HandleShutdown(drainTimeoutMs int) string {
	go d.Shutdown()
	return "daemon shutting down"
}

// HandleGracefulRestart implements control.DaemonHandler.
// Serializes state snapshot, then shuts down. The new daemon will load the snapshot
// on startup and restore owners with pre-populated caches.
func (d *Daemon) HandleGracefulRestart(drainTimeoutMs int) (string, error) {
	snapshotPath, err := d.SerializeSnapshot()
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	go d.Shutdown()
	return snapshotPath, nil
}

// HandleStatus implements control.CommandHandler.
func (d *Daemon) HandleStatus() map[string]any {
	d.mu.RLock()
	defer d.mu.RUnlock()

	servers := make([]map[string]any, 0, len(d.owners))
	for sid, entry := range d.owners {
		if entry.Owner == nil {
			continue // placeholder still being created
		}
		s := entry.Owner.Status()
		s["server_id"] = sid
		s["persistent"] = entry.Persistent
		s["last_session"] = entry.LastSession.Format(time.RFC3339)
		// Prefer the per-owner override from x-mux.idleTimeout capability
		// (set via Owner.SetIdleTimeout after init); fall back to the
		// daemon-wide default captured at spawn time.
		effectiveIdleTimeout := entry.IdleTimeout
		if override := entry.Owner.IdleTimeout(); override > 0 {
			effectiveIdleTimeout = override
		}
		s["idle_timeout_s"] = effectiveIdleTimeout.Seconds()
		if !entry.Owner.LastActivity().IsZero() {
			s["last_activity"] = entry.Owner.LastActivity().Format(time.RFC3339)
		}
		s["active_progress_tokens"] = entry.Owner.ActiveProgressTokens()
		s["busy"] = entry.Owner.HasActiveBusyWork()
		servers = append(servers, s)
	}

	return map[string]any{
		"daemon":             true,
		"owner_count":        len(servers), // excludes placeholders still being created
		"servers":            servers,
		"owner_idle_timeout": d.ownerIdleTimeout.String(),
		"idle_timeout":       d.idleTimeout.String(),
	}
}

// SetPersistent marks an owner as persistent (survives zero-session periods).
func (d *Daemon) SetPersistent(serverID string, persistent bool) {
	d.mu.Lock()
	if entry, ok := d.owners[serverID]; ok {
		entry.Persistent = persistent
		d.logger.Printf("owner %s persistent=%v", serverID[:8], persistent)
	}
	d.mu.Unlock()
}

// findSharedOwner searches for an existing owner with the same command+args
// that is still accepting connections. Dedup is optimistic: unclassified owners
// are assumed shareable. If an owner later classifies as isolated, it closes its
// IPC listener — extra sessions get EOF and reconnect with their own owner.
//
// Must be called with d.mu held. May transiently release and re-acquire d.mu
// when it encounters a placeholder (Owner == nil) for a matching command+args —
// it waits for that creation to complete before returning.
func (d *Daemon) findSharedOwner(command string, args []string, env map[string]string) *OwnerEntry {
	needle := command + " " + strings.Join(args, " ")
	for _, entry := range d.owners {
		candidate := entry.Command + " " + strings.Join(entry.Args, " ")
		if candidate != needle {
			continue
		}
		if entry.Owner == nil {
			// Matching command+args but still being created — wait with timeout.
			creating := entry.creating
			d.mu.Unlock()
			select {
			case <-creating:
			case <-time.After(30 * time.Second):
				d.mu.Lock()
				return nil // timed out waiting for placeholder — caller will create new
			}
			d.mu.Lock()
			// Re-check after wait: creation may have succeeded or failed.
			if entry.Owner != nil && entry.Owner.IsAccepting() && envCompatible(entry.Env, env) {
				return entry
			}
			return nil
		}
		// Skip owners with incompatible env — different API keys, tokens, etc.
		if !envCompatible(entry.Env, env) {
			continue
		}
		if !entry.Owner.IsAccepting() {
			continue
		}
		// Non-blocking classification check: if already classified, respect it.
		// If not yet classified, return optimistically (assume shareable).
		// This avoids both extremes:
		// - Blocking wait (8s+ for slow upstreams → CC timeout)
		// - Blind optimistic dedup (returns isolated owners → evict storm)
		select {
		case <-entry.Owner.Classified():
			// Classification known — re-check IsAccepting (may have closed listener)
			if !entry.Owner.IsAccepting() {
				continue
			}
		default:
			// Not yet classified — optimistic: assume shareable.
			// If later classified isolated → evict extra sessions.
		}
		return entry
	}
	return nil
}

// envCompatible returns true if two env maps have no conflicting values
// for semantically significant keys (API tokens, config paths, etc.).
// Transient per-session vars (CLAUDE_CODE_*, WT_SESSION, etc.) are ignored.
func envCompatible(a, b map[string]string) bool {
	for k, va := range a {
		if envTransient(k) {
			continue
		}
		if vb, ok := b[k]; ok && va != vb {
			return false
		}
	}
	return true
}

// envTransient returns true for env vars that are per-session/transient
// and should NOT affect dedup decisions.
func envTransient(key string) bool {
	switch {
	case strings.HasPrefix(key, "CLAUDE_CODE_"):
		return true
	case strings.HasPrefix(key, "CLAUDE_AUTO"):
		return true
	case strings.HasPrefix(key, "WT_"):
		return true
	case key == "SESSIONNAME" || key == "WSLENV":
		return true
	case key == "CLAUDE_CODE_ENTRYPOINT":
		return true
	}
	return false
}

// diffEnv returns only the env vars from shim that differ from daemon's own env.
// This extracts CC-configured vars (API keys, config paths) without duplicating
// the ~100 standard OS vars that daemon already has.
func diffEnv(shimEnv map[string]string) map[string]string {
	if len(shimEnv) == 0 {
		return nil
	}
	diff := make(map[string]string)
	for k, v := range shimEnv {
		if daemonVal, ok := os.LookupEnv(k); !ok || daemonVal != v {
			diff[k] = v
		}
	}
	if len(diff) == 0 {
		return nil
	}
	return diff
}

// Shutdown gracefully stops all owners and the daemon.
func (d *Daemon) Shutdown() {
	d.shutdownOnce.Do(func() {
		d.logger.Printf("daemon shutting down...")

		// Cancel supervisor context first — prevents suture from restarting
		// services as we're tearing down owners one by one. We wait briefly
		// for the supervisor goroutine to exit before proceeding.
		if d.supervisorCancel != nil {
			d.supervisorCancel()
		}
		if d.supervisorErr != nil {
			select {
			case <-d.supervisorErr:
				// Supervisor exited cleanly
			case <-time.After(2 * time.Second):
				d.logger.Printf("supervisor did not exit within 2s, proceeding anyway")
			}
		}

		// Close control server
		if d.ctlSrv != nil {
			d.ctlSrv.Close()
		}

		// Shutdown all owners
		d.mu.Lock()
		entries := make([]*OwnerEntry, 0, len(d.owners))
		for _, e := range d.owners {
			entries = append(entries, e)
		}
		d.owners = make(map[string]*OwnerEntry)
		d.mu.Unlock()

		for _, e := range entries {
			if e.Owner != nil {
				e.Owner.Shutdown()
			}
		}

		d.logger.Printf("daemon stopped (%d owners shut down)", len(entries))
		close(d.done)
	})
}

// Done returns a channel closed when the daemon has shut down.
func (d *Daemon) Done() <-chan struct{} {
	return d.done
}

// OwnerCount returns the number of fully-created owners (excludes placeholders).
func (d *Daemon) OwnerCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := 0
	for _, e := range d.owners {
		if e.Owner != nil {
			n++
		}
	}
	return n
}

// Entry returns the OwnerEntry for the given server ID, or nil.
func (d *Daemon) Entry(serverID string) *OwnerEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.owners[serverID]
}

// onZeroSessions is called by an owner when its last session disconnects.
func (d *Daemon) onZeroSessions(serverID string) {
	d.mu.Lock()
	entry, ok := d.owners[serverID]
	if ok {
		entry.LastSession = time.Now()
	}
	d.mu.Unlock()

	if ok {
		d.logger.Printf("owner %s: zero sessions (grace period starts)", serverID[:8])
	}
	// Reaper will handle cleanup after grace period
}

// onUpstreamExit is called by an owner when its upstream process exits.
func (d *Daemon) onUpstreamExit(serverID string) {
	d.mu.Lock()
	entry, ok := d.owners[serverID]
	if ok {
		if entry.Persistent {
			d.mu.Unlock()
			d.logger.Printf("owner %s: upstream exited, will re-spawn (persistent)", serverID[:8])
			// Reaper handles re-spawn for persistent owners
			return
		}
		delete(d.owners, serverID)
	}
	d.mu.Unlock()

	if ok {
		entry.Owner.Shutdown()
		d.logger.Printf("owner %s: upstream exited, removed", serverID[:8])
	}
}

// StatusJSON returns the daemon status as a JSON-encoded byte slice.
func (d *Daemon) StatusJSON() (json.RawMessage, error) {
	status := d.HandleStatus()
	return json.Marshal(status)
}
