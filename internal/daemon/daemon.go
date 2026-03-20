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
	"strings"
	"sync"
	"time"

	"github.com/thebtf/mcp-mux/internal/control"
	"github.com/thebtf/mcp-mux/internal/mux"
	"github.com/thebtf/mcp-mux/internal/serverid"
)

// OwnerEntry tracks a single managed owner and its metadata.
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

	return d, nil
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
		entry.LastSession = time.Now()
		d.mu.Unlock()
		entry.Owner.SessionMgr().PreRegister(token, req.Cwd)
		d.logger.Printf("reusing existing owner %s for %s", sid[:8], req.Command)
		return entry.Owner.IPCPath(), sid, token, nil
	}

	// 2. Global dedup: if a shared owner for same command+args exists (any cwd), reuse it.
	//    This prevents N copies of stateless servers (tavily, time, nia) across projects.
	if mode == serverid.ModeCwd {
		if existing := d.findSharedOwner(req.Command, req.Args); existing != nil {
			existing.LastSession = time.Now()
			existingSID := existing.ServerID
			d.mu.Unlock()
			// Register this project's cwd so roots/list includes it
			if req.Cwd != "" {
				existing.Owner.AddCwd(req.Cwd)
			}
			existing.Owner.SessionMgr().PreRegister(token, req.Cwd)
			d.logger.Printf("dedup: reusing shared owner %s for %s (added cwd: %s)", existingSID[:8], req.Command, req.Cwd)
			return existing.Owner.IPCPath(), existingSID, token, nil
		}
	}

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
		return "", "", "", fmt.Errorf("spawn %s: %w", req.Command, err)
	}

	entry := &OwnerEntry{
		Owner:       owner,
		ServerID:    sid,
		Command:     req.Command,
		Args:        req.Args,
		Cwd:         req.Cwd,
		Mode:        req.Mode,
		Env:         req.Env,
		LastSession: time.Now(),
		GracePeriod: d.gracePeriod,
	}

	d.mu.Lock()
	d.owners[sid] = entry
	d.mu.Unlock()

	owner.SessionMgr().PreRegister(token, req.Cwd)
	d.logger.Printf("spawned owner %s for %s %v", sid[:8], req.Command, req.Args)
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
		s := entry.Owner.Status()
		s["server_id"] = sid
		s["persistent"] = entry.Persistent
		s["last_session"] = entry.LastSession.Format(time.RFC3339)
		s["grace_period_s"] = entry.GracePeriod.Seconds()
		servers = append(servers, s)
	}

	return map[string]any{
		"daemon":       true,
		"owner_count":  len(d.owners),
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
// that is classified as shared (stateless — no per-session state).
// Session-aware and isolated servers are NOT deduped: they need per-cwd owners
// because upstream uses roots/list to determine the working directory.
// Must be called with d.mu held.
func (d *Daemon) findSharedOwner(command string, args []string) *OwnerEntry {
	needle := command + " " + strings.Join(args, " ")
	for _, entry := range d.owners {
		candidate := entry.Command + " " + strings.Join(entry.Args, " ")
		if candidate == needle {
			status := entry.Owner.Status()
			classification, _ := status["auto_classification"].(string)
			// Only truly stateless (shared) servers can be deduped across cwds.
			// Session-aware servers need per-cwd roots for correct upstream behavior.
			if classification == "" || classification == "shared" {
				return entry
			}
		}
	}
	return nil
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
			e.Owner.Shutdown()
		}

		d.logger.Printf("daemon stopped (%d owners shut down)", len(entries))
		close(d.done)
	})
}

// Done returns a channel closed when the daemon has shut down.
func (d *Daemon) Done() <-chan struct{} {
	return d.done
}

// OwnerCount returns the number of managed owners.
func (d *Daemon) OwnerCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.owners)
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
