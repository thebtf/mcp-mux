// Package daemon implements a global daemon that manages all upstream MCP server
// processes. CC sessions connect as thin shims via IPC; the daemon handles
// lifecycle, GC, reaping, health monitoring, and persistence.
package daemon

import (
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
	GracePeriod time.Duration
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

	gracePeriod time.Duration
	idleTimeout time.Duration

	shutdownOnce sync.Once
}

// Config holds daemon startup parameters.
type Config struct {
	// ControlPath is the daemon's control socket path.
	ControlPath string

	// GracePeriod is how long non-persistent owners survive with zero sessions.
	// Default: 30s. Overridden by MCP_MUX_GRACE env var.
	GracePeriod time.Duration

	// IdleTimeout is how long the daemon waits with zero owners before auto-exiting.
	// Default: 5 minutes.
	IdleTimeout time.Duration

	Logger *log.Logger
}

// New creates and starts a new Daemon with a control server.
func New(cfg Config) (*Daemon, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "[mcp-muxd] ", log.LstdFlags)
	}

	gracePeriod := cfg.GracePeriod
	if gracePeriod == 0 {
		gracePeriod = 30 * time.Second
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = 5 * time.Minute
	}

	d := &Daemon{
		owners:      make(map[string]*OwnerEntry),
		logger:      logger,
		done:        make(chan struct{}),
		gracePeriod: gracePeriod,
		idleTimeout: idleTimeout,
	}

	ctlSrv, err := control.NewServer(cfg.ControlPath, d, logger)
	if err != nil {
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
	if restored := d.loadSnapshot(); restored > 0 {
		logger.Printf("startup: restored %d owners from snapshot", restored)
	}

	return d, nil
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
			d.logger.Printf("reusing existing owner %s for %s", sid[:8], req.Command)
			return entry.Owner.IPCPath(), sid, token, nil
		}
		// Owner exists but IPC listener is closed (isolated server) — remove and re-spawn.
		entry.Owner.Shutdown()
		delete(d.owners, sid)
		d.logger.Printf("owner %s not accepting (isolated), re-spawning", sid[:8])
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
				existing.Owner.AddCwd(req.Cwd)
			}
			existing.Owner.SessionMgr().PreRegister(token, req.Cwd, req.Env)
			d.logger.Printf("dedup: reusing shared owner %s for %s (added cwd: %s)", existingSID[:8], req.Command, req.Cwd)
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

	// Create a new owner
	controlPath := serverid.ControlPath(sid)

	owner, err := mux.NewOwner(mux.OwnerConfig{
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
		Logger: log.New(d.logger.Writer(), fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags|log.Lmicroseconds),
	})
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

	// Promote the placeholder to a real entry and signal waiters.
	d.mu.Lock()
	placeholder.Owner = owner
	placeholder.Mode = req.Mode
	placeholder.Env = req.Env
	placeholder.LastSession = time.Now()
	placeholder.GracePeriod = d.gracePeriod
	close(placeholder.creating)
	placeholder.creating = nil // no longer a placeholder
	d.mu.Unlock()

	owner.SessionMgr().PreRegister(token, req.Cwd, req.Env)
	d.logger.Printf("spawned owner %s for %s %v", sid[:8], req.Command, req.Args)

	// Return immediately — proactive init runs in background (owner.sendProactiveInit).
	// When a session connects and sends initialize, the owner either:
	// (a) replays from cache if proactive init already completed, or
	// (b) waits on initReady then replays (blocks the init RESPONSE, not the process)
	// This ensures the shim process starts quickly — CC sees a responsive process
	// and waits for the init response (normal MCP behavior) instead of seeing a
	// silent process and timing out.
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
	d.mu.Unlock()

	entry.Owner.Shutdown()
	d.logger.Printf("removed owner %s", serverID[:8])
	return nil
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
		s["grace_period_s"] = entry.GracePeriod.Seconds()
		servers = append(servers, s)
	}

	return map[string]any{
		"daemon":       true,
		"owner_count":  len(servers), // excludes placeholders still being created
		"servers":      servers,
		"grace_period": d.gracePeriod.String(),
		"idle_timeout": d.idleTimeout.String(),
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
		// Wait for classification before dedup decision. Without this,
		// isolated servers get dedup'd during the classification window,
		// causing evict storms (evict → reconnect → dedup → classify → evict).
		classified := entry.Owner.Classified()
		d.mu.Unlock()
		select {
		case <-classified:
		case <-time.After(30 * time.Second):
			d.mu.Lock()
			return nil // timed out — caller will create new owner
		}
		d.mu.Lock()
		// Re-check after classification: may now be isolated or not accepting.
		if !entry.Owner.IsAccepting() {
			continue
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
