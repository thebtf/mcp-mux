// Package owner implements the core multiplexer routing logic for mcp-mux.
// It manages a single upstream process and routes requests from multiple
// downstream sessions through it.
package owner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/muxcore/listchanged"
	"github.com/thebtf/mcp-mux/muxcore/progress"
	"github.com/thebtf/mcp-mux/muxcore/remap"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/session"
	"github.com/thebtf/mcp-mux/muxcore/snapshot"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
	"github.com/thejerf/suture/v4"
)

// Type aliases for session and snapshot types used throughout owner.
type (
	Session        = session.Session
	SessionManager = session.Manager
	OwnerSnapshot  = snapshot.OwnerSnapshot
	SessionSnapshot = snapshot.SessionSnapshot
)

// Constructor aliases.
var (
	NewSession              = session.NewSession
	NewSessionWithRawWriter = session.NewSessionWithRawWriter
	NewSessionManager       = session.NewManager
)

// Version is the mcp-mux build version, included in status output.
// Auto-detected from Go build info (vcs.revision + vcs.modified).
// Override at build time via: -ldflags "-X github.com/thebtf/mcp-mux/muxcore/owner.Version=..."
var Version = initVersion()

func initVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 8 {
		rev = rev[:8]
	}
	return rev + dirty
}

// InflightRequest holds metadata about a request currently being processed by upstream.
// Used for observability: mux_list --verbose shows what's pending and for how long.
type InflightRequest struct {
	Method    string    `json:"method"`
	Tool      string    `json:"tool,omitempty"`
	SessionID int       `json:"session"`
	StartTime time.Time `json:"started_at"`
}

// Owner is the multiplexer core. It manages a single upstream process and
// routes requests from multiple downstream sessions through it.
type Owner struct {
	upstream *upstream.Process
	ipcPath  string
	cwd      string          // primary working directory (from first spawn)
	cwdSet   map[string]bool // all known cwds (for multi-project roots/list)
	command     string          // upstream command (for status/restart)
	args        []string        // upstream args (for status/restart)
	handlerFunc    func(ctx context.Context, stdin io.Reader, stdout io.Writer) error // in-process MCP handler (nil = subprocess)
	sessionHandler muxcore.SessionHandler                                            // structured in-process handler (nil = pipe or subprocess)
	serverID string          // server identity hash
	listener net.Listener
	logger   *log.Logger

	onZeroSessions       func(serverID string)
	onUpstreamExit       func(serverID string)
	onPersistentDetected func(serverID string)
	onCacheReady         func(serverID string)

	mu                   sync.RWMutex
	sessions             map[int]*Session
	cachedInitSessions   map[int]bool // sessions that received a cached (replayed) initialize response
	initDone             bool
	initResp             []byte // cached initialize response (raw JSON-RPC)
	initProtocolVersion  string // protocolVersion from first initialize request (for fingerprint matching)
	toolList             []byte // cached tools/list response (raw JSON-RPC)
	promptList           []byte // cached prompts/list response (raw JSON-RPC)
	resourceList         []byte // cached resources/list response (raw JSON-RPC)
	resourceTemplateList []byte // cached resources/templates/list response (raw JSON-RPC)
	autoClassification   classify.SharingMode
	classificationSource string        // "capability" or "tools" — what determined classification
	classificationReason []string      // tool names that triggered isolation
	classified           chan struct{} // closed when autoClassification is first set
	classifiedOnce       sync.Once
	initReady            chan struct{} // closed when initialize response is cached or upstream dies
	initReadyOnce        sync.Once

	sessionMgr             *SessionManager
	tokenHandshake         bool                // true when daemon manages this owner (shims send token)
	progressOwners         map[string]int      // progressToken → session ID for targeted routing
	progressTokenRequestID map[string]string   // progressToken → remapped request ID that registered it
	requestToTokens        map[string][]string // remapped request ID → list of progress tokens

	progressTracker *progress.Tracker // dedup state for synthetic progress emission

	upstreamDead     atomic.Bool // set when upstream exits; prevents sending to dead pipe
	methodTags       sync.Map    // remapped request ID (string) -> method name
	inflightTracker  sync.Map    // remapped request ID (string) -> *InflightRequest
	timedOutIDs      sync.Map    // remapped request ID (string) -> struct{} — watchdog-claimed IDs, late upstream responses are dropped
	pendingRequests  atomic.Int64
	drainTimeout     time.Duration // from x-mux.drainTimeout capability; 0 = use default
	toolTimeoutNs      atomic.Int64 // from x-mux.toolTimeout capability; stored as nanoseconds for atomic access
	idleTimeoutNs      atomic.Int64 // from x-mux.idleTimeout capability; 0 = use daemon default
	progressIntervalNs atomic.Int64 // from x-mux.progressInterval capability; stored as nanoseconds; 0 = use default (5s)
	lastActivityNs   atomic.Int64  // unix-nano of last inbound/outbound MCP message or session change
	busyMu           sync.Mutex
	busyDeclarations map[string]busyDeclaration // busy_id → declaration (long-running work signal)
	startTime        time.Time
	controlServer    *control.Server

	shutdownOnce      sync.Once
	closeListenerOnce sync.Once
	listenerDone      chan struct{} // closed when IPC listener is intentionally stopped
	done              chan struct{}

	// backgroundSpawnCh is non-nil for owners created via NewOwnerFromSnapshot,
	// where the upstream is started asynchronously via SpawnUpstreamBackground.
	// It is closed (under mu) when background spawn finishes (success or failure),
	// allowing Serve to block instead of treating nil upstream as dead.
	backgroundSpawnCh chan struct{}
}

// OwnerConfig holds parameters for creating an Owner.
type OwnerConfig struct {
	// Command and Args for spawning the upstream MCP server.
	Command string
	Args    []string
	Env     map[string]string

	// Cwd is the working directory for the upstream process.
	// If empty, inherits from the current process.
	Cwd string

	// IPCPath for the Unix domain socket listener.
	IPCPath string

	// ControlPath for the control plane socket. If empty, control plane is disabled.
	ControlPath string

	// ServerID is the server identity hash. Used in callbacks to identify this owner.
	ServerID string

	// OnZeroSessions is called when the last session disconnects.
	// If nil, the owner does not auto-shutdown on zero sessions (legacy behavior
	// shuts down only on upstream exit or signal).
	OnZeroSessions func(serverID string)

	// OnUpstreamExit is called when the upstream process exits.
	// If nil, the owner auto-shuts down (legacy behavior).
	OnUpstreamExit func(serverID string)

	// OnPersistentDetected is called when the upstream declares x-mux.persistent: true.
	// Used by the daemon to mark the owner entry as persistent.
	OnPersistentDetected func(serverID string)

	// OnCacheReady is called when the owner has cached both init and tools/list responses.
	// Used by the daemon to update the template cache for instant isolated server spawns.
	OnCacheReady func(serverID string)

	// TokenHandshake enables reading the shim's handshake token from each new IPC
	// connection before the MCP session begins. Only set true when the owner is
	// managed by the global daemon — shims always send a token in that mode.
	// Legacy and test connections do not send a token; leave this false (default).
	TokenHandshake bool

	// HandlerFunc is an in-process MCP server implementation.
	// When set, Owner runs the handler via io.Pipe instead of spawning a subprocess.
	// The handler receives stdin/stdout pipes and should speak JSON-RPC 2.0 on them.
	// Mutually exclusive with Command/Args: if HandlerFunc is set, Command is ignored.
	HandlerFunc func(ctx context.Context, stdin io.Reader, stdout io.Writer) error

	// SessionHandler is a structured in-process MCP server implementation.
	// When set, Owner calls HandleRequest directly for each downstream request
	// instead of routing through a pipe or subprocess.
	// Mutually exclusive with HandlerFunc and Command/Args.
	SessionHandler muxcore.SessionHandler

	// Logger for debug output. Uses log.Default() if nil.
	Logger *log.Logger
}

// NewOwnerFromSnapshot creates an Owner with pre-populated caches from a snapshot.
// Does NOT spawn the upstream process — caller must call SpawnUpstreamBackground()
// after the owner is registered in the daemon. The owner serves cached responses
// immediately (cache-first mode) while the upstream starts in the background.
func NewOwnerFromSnapshot(cfg OwnerConfig, snap OwnerSnapshot) (*Owner, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	// Start IPC listener (no upstream yet)
	ln, err := ipc.Listen(cfg.IPCPath)
	if err != nil {
		return nil, fmt.Errorf("owner from snapshot: listen %s: %w", cfg.IPCPath, err)
	}

	cwdSet := map[string]bool{}
	for _, c := range snap.CwdSet {
		cwdSet[c] = true
	}
	if cfg.Cwd != "" {
		cwdSet[cfg.Cwd] = true
	}

	o := &Owner{
		ipcPath:                cfg.IPCPath,
		cwd:                    cfg.Cwd,
		cwdSet:                 cwdSet,
		command:                cfg.Command,
		args:                   cfg.Args,
		handlerFunc:            cfg.HandlerFunc,
		sessionHandler:         cfg.SessionHandler,
		serverID:               cfg.ServerID,
		listener:               ln,
		logger:                 logger,
		onZeroSessions:         cfg.OnZeroSessions,
		onUpstreamExit:         cfg.OnUpstreamExit,
		onPersistentDetected:   cfg.OnPersistentDetected,
		onCacheReady:           cfg.OnCacheReady,
		sessions:               make(map[int]*Session),
		cachedInitSessions:     make(map[int]bool),
		sessionMgr:             NewSessionManager(),
		tokenHandshake:         cfg.TokenHandshake,
		autoClassification:     snap.Classification,
		classificationSource:   snap.ClassificationSource,
		classificationReason:   snap.ClassificationReason,
		classified:             make(chan struct{}),
		initReady:              make(chan struct{}),
		progressOwners:         make(map[string]int),
		progressTokenRequestID: make(map[string]string),
		requestToTokens:        make(map[string][]string),
		progressTracker:        progress.NewTracker(),
		startTime:              time.Now(),
		listenerDone:           make(chan struct{}),
		done:                   make(chan struct{}),
		backgroundSpawnCh:      make(chan struct{}),
	}
	o.progressIntervalNs.Store(int64(5 * time.Second))

	// Pre-populate caches from snapshot
	if snap.CachedInit != "" {
		if data, err := Base64Decode(snap.CachedInit); err == nil {
			o.initResp = data
			o.initDone = true
		}
	}
	if snap.CachedTools != "" {
		if data, err := Base64Decode(snap.CachedTools); err == nil {
			o.toolList = data
		}
	}
	if snap.CachedPrompts != "" {
		if data, err := Base64Decode(snap.CachedPrompts); err == nil {
			o.promptList = data
		}
	}
	if snap.CachedResources != "" {
		if data, err := Base64Decode(snap.CachedResources); err == nil {
			o.resourceList = data
		}
	}
	if snap.CachedResourceTemplates != "" {
		if data, err := Base64Decode(snap.CachedResourceTemplates); err == nil {
			o.resourceTemplateList = data
		}
	}

	// Close initReady and classified immediately — caches already populated
	o.initReadyOnce.Do(func() { close(o.initReady) })
	o.classifiedOnce.Do(func() { close(o.classified) })

	// Wire notifier into sessionHandler if it supports NotifierAware.
	if o.sessionHandler != nil {
		if na, ok := o.sessionHandler.(muxcore.NotifierAware); ok {
			na.SetNotifier(&ownerNotifier{owner: o})
		}
	}

	// Start control plane if configured
	if cfg.ControlPath != "" {
		ctlSrv, err := control.NewServer(cfg.ControlPath, o, logger)
		if err != nil {
			logger.Printf("warning: control server failed to start: %v", err)
		} else {
			o.controlServer = ctlSrv
		}
	}

	// Start accepting IPC connections (sessions get cached replay immediately)
	go o.acceptLoop()

	// Start synthetic progress reporter
	go o.runProgressReporter(doneContext(o.done))

	logger.Printf("owner restored from snapshot (cached: init=%v tools=%v prompts=%v resources=%v)",
		o.initDone, o.toolList != nil, o.promptList != nil, o.resourceList != nil)

	return o, nil
}

// SpawnUpstreamBackground starts the upstream process in a goroutine and runs
// proactive init to refresh caches. Called after NewOwnerFromSnapshot when the
// owner is registered in the daemon. If upstream fails, owner continues serving
// stale cached responses.
func (o *Owner) SpawnUpstreamBackground() {
	go func() {
		// Check if owner is already shutting down before spawning
		select {
		case <-o.done:
			return
		default:
		}

		var proc *upstream.Process
		var err error
		if o.handlerFunc != nil {
			proc = upstream.NewProcessFromHandler(context.Background(), o.handlerFunc)
			o.logger.Printf("background handler spawn: in-process (no subprocess)")
		} else {
			proc, err = upstream.Start(o.command, o.args, nil, o.cwd)
		}
		if err != nil {
			o.logger.Printf("background upstream spawn failed: %v (serving stale cache)", err)
			// Unblock Serve — spawn is done (failed), nil upstream will be observed via upstreamDeadCh.
			o.mu.Lock()
			if o.backgroundSpawnCh != nil {
				close(o.backgroundSpawnCh)
				o.backgroundSpawnCh = nil
			}
			o.mu.Unlock()
			return
		}

		// Check again after spawn — owner may have shut down while process was starting
		select {
		case <-o.done:
			proc.Close()
			// Still unblock Serve so it can observe o.done and exit cleanly.
			o.mu.Lock()
			if o.backgroundSpawnCh != nil {
				close(o.backgroundSpawnCh)
				o.backgroundSpawnCh = nil
			}
			o.mu.Unlock()
			return
		default:
		}

		o.mu.Lock()
		o.upstream = proc
		// Unblock Serve — upstream is now set; upstreamDeadCh will return proc.Done.
		if o.backgroundSpawnCh != nil {
			close(o.backgroundSpawnCh)
			o.backgroundSpawnCh = nil
		}
		// Invalidate stale list caches — the new upstream may have different
		// tools/prompts/resources. Proactive init will repopulate them.
		o.toolList = nil
		o.promptList = nil
		o.resourceList = nil
		o.resourceTemplateList = nil
		o.mu.Unlock()

		// Notify all connected sessions that lists have changed so CC re-fetches
		// its tool/prompt/resource lists from the fresh upstream.
		o.broadcastListChanged()

		// Start reading upstream responses
		go o.readUpstream()

		// Send proactive init to refresh caches from new upstream
		go o.sendProactiveInit()

		// Monitor upstream exit
		go func() {
			<-proc.Done
			o.logger.Printf("upstream exited: %v", proc.ExitErr)
			o.initReadyOnce.Do(func() {
				if o.initReady != nil {
					close(o.initReady)
				}
			})
			if o.onUpstreamExit != nil {
				o.onUpstreamExit(o.serverID)
			} else {
				o.Shutdown()
			}
		}()
	}()
}

// NewOwner creates and starts a new Owner.
// It spawns the upstream process and starts the IPC listener.
// If cfg.HandlerFunc is set, the handler is run in-process via io.Pipe instead
// of spawning a subprocess.
func NewOwner(cfg OwnerConfig) (*Owner, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	// Spawn upstream — either in-process handler, subprocess, or neither (SessionHandler-only).
	var proc *upstream.Process
	if cfg.SessionHandler != nil && cfg.HandlerFunc == nil {
		// SessionHandler-only: no upstream process needed.
		// Requests dispatch directly via dispatchToSessionHandler.
		logger.Printf("owner: SessionHandler-only mode (no upstream process)")
	} else if cfg.HandlerFunc != nil {
		proc = upstream.NewProcessFromHandler(context.Background(), cfg.HandlerFunc)
		logger.Printf("owner: started in-process handler (no subprocess)")
	} else {
		var err error
		proc, err = upstream.Start(cfg.Command, cfg.Args, cfg.Env, cfg.Cwd)
		if err != nil {
			return nil, fmt.Errorf("owner: start upstream: %w", err)
		}
	}

	// Start IPC listener
	ln, err := ipc.Listen(cfg.IPCPath)
	if err != nil {
		if proc != nil {
			proc.Close()
		}
		return nil, fmt.Errorf("owner: listen %s: %w", cfg.IPCPath, err)
	}

	o := &Owner{
		upstream:               proc,
		ipcPath:                cfg.IPCPath,
		cwd:                    cfg.Cwd,
		cwdSet:                 map[string]bool{serverid.CanonicalizePath(cfg.Cwd): true},
		command:                cfg.Command,
		args:                   cfg.Args,
		handlerFunc:            cfg.HandlerFunc,
		sessionHandler:         cfg.SessionHandler,
		serverID:               cfg.ServerID,
		listener:               ln,
		logger:                 logger,
		onZeroSessions:         cfg.OnZeroSessions,
		onUpstreamExit:         cfg.OnUpstreamExit,
		onPersistentDetected:   cfg.OnPersistentDetected,
		onCacheReady:           cfg.OnCacheReady,
		sessions:               make(map[int]*Session),
		cachedInitSessions:     make(map[int]bool),
		sessionMgr:             NewSessionManager(),
		tokenHandshake:         cfg.TokenHandshake,
		classified:             make(chan struct{}),
		initReady:              make(chan struct{}),
		progressOwners:         make(map[string]int),
		progressTokenRequestID: make(map[string]string),
		requestToTokens:        make(map[string][]string),
		progressTracker:        progress.NewTracker(),
		startTime:              time.Now(),
		listenerDone:           make(chan struct{}),
		done:                   make(chan struct{}),
	}
	o.progressIntervalNs.Store(int64(5 * time.Second))

	// Wire notifier into sessionHandler if it supports NotifierAware.
	if o.sessionHandler != nil {
		if na, ok := o.sessionHandler.(muxcore.NotifierAware); ok {
			na.SetNotifier(&ownerNotifier{owner: o})
		}
	}

	// Start control plane if configured
	if cfg.ControlPath != "" {
		ctlSrv, err := control.NewServer(cfg.ControlPath, o, logger)
		if err != nil {
			logger.Printf("warning: control server failed to start: %v", err)
		} else {
			o.controlServer = ctlSrv
			logger.Printf("control socket: %s", cfg.ControlPath)
		}
	}

	// Start reading from upstream
	go o.readUpstream()

	// Send proactive initialize to upstream so init response is cached
	// before any session connects. Without this, Daemon.Spawn blocks on
	// InitReady() but init requires a session to send the request — deadlock.
	// SessionHandler-only owners have no upstream, so skip the proactive init.
	if proc != nil {
		go o.sendProactiveInit()
	}

	// Start accepting IPC connections
	go o.acceptLoop()

	// Start synthetic progress reporter
	go o.runProgressReporter(doneContext(o.done))

	// Monitor upstream exit (skip in SessionHandler-only mode — proc is nil)
	if proc != nil {
		go func() {
			<-proc.Done
			logger.Printf("upstream exited: %v", proc.ExitErr)
			// Signal init-ready so Daemon.Spawn doesn't block forever on crashed upstream
			o.initReadyOnce.Do(func() {
				if o.initReady != nil {
					close(o.initReady)
				}
			})
			if o.onUpstreamExit != nil {
				o.onUpstreamExit(o.serverID)
			} else {
				o.Shutdown()
			}
		}()
	}

	return o, nil
}

// SessionMgr returns the owner's SessionManager.
// Used by the daemon to call PreRegister before the shim connects.
func (o *Owner) SessionMgr() *SessionManager {
	return o.sessionMgr
}

// readToken reads the handshake token sent by the shim immediately after connecting.
// The token is a hex string terminated by '\n'. Uses byte-by-byte reading to avoid
// consuming subsequent MCP messages that immediately follow the token line.
// Returns (token, conn) where conn may be wrapped with io.MultiReader if the first
// bytes were not a token (backward compat with old shims that send MCP directly).
func readToken(conn net.Conn) (string, io.Reader) {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return "", conn
	}
	defer conn.SetReadDeadline(time.Time{}) // clear deadline after read

	buf := make([]byte, 0, 32)
	one := make([]byte, 1)
	for {
		_, err := conn.Read(one)
		if err != nil {
			// Timeout or error — return whatever we read prepended back
			if len(buf) > 0 {
				return "", io.MultiReader(bytes.NewReader(buf), conn)
			}
			return "", conn
		}
		if one[0] == '\n' {
			break
		}
		// First byte check: hex tokens are [0-9a-f]. If first byte is '{' or '[',
		// this is an MCP message from an old shim — prepend and return.
		if len(buf) == 0 && !isHexChar(one[0]) {
			return "", io.MultiReader(bytes.NewReader(one), conn)
		}
		buf = append(buf, one[0])
		if len(buf) > 64 { // tokens are 16 hex chars; >64 means something is wrong
			return "", io.MultiReader(bytes.NewReader(buf), conn)
		}
	}
	return string(buf), conn
}

// isHexChar returns true if b is a valid hex character [0-9a-f].
func isHexChar(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// Base64Decode decodes a base64-encoded string. Exported for snapshot loading.
func Base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// extractToolName extracts params.name from a tools/call JSON-RPC request.
// Returns empty string for non-tools/call requests or malformed params.
func extractToolName(raw []byte) string {
	var req struct {
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &req) == nil && req.Params.Name != "" {
		return req.Params.Name
	}
	return ""
}

// AddSession registers a new downstream session and starts routing its messages.
// This is used for the owner's own stdio session (first client).
func (o *Owner) AddSession(s *Session) {
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()

	o.sessionMgr.RegisterSession(s, s.Cwd)
	o.touchActivity()
	o.logger.Printf("session %d connected (cwd: %q)", s.ID, s.Cwd)

	if o.sessionHandler != nil {
		if lc, ok := o.sessionHandler.(muxcore.ProjectLifecycle); ok {
			project := muxcore.ProjectContext{
				ID:  muxcore.ProjectContextID(s.Cwd),
				Cwd: s.Cwd,
				Env: s.Env,
			}
			go lc.OnProjectConnect(project)
		}
	}

	// For template-restored isolated owners, close the IPC listener after the first
	// session connects. Normal owners close the listener during classification
	// (classifyFromCapabilities/classifyFromToolList), but template owners skip
	// classification (already set from snapshot) so the listener stays open.
	o.mu.RLock()
	isIsolated := o.autoClassification == classify.ModeIsolated && o.classificationSource != ""
	o.mu.RUnlock()
	if isIsolated {
		o.closeListener()
	}

	// Notify upstream that roots may have changed (new client = new potential root)
	o.sendRootsListChanged()

	go o.readSession(s)
}

// readSession reads messages from a session and forwards them to the upstream.
func (o *Owner) readSession(s *Session) {
	defer func() {
		o.removeSession(s)
		s.Close()
	}()

	for {
		msg, err := s.ReadMessage()
		if err != nil {
			if err != io.EOF {
				o.logger.Printf("session %d read error: %v", s.ID, err)
			}
			return
		}

		if err := o.handleDownstreamMessage(s, msg); err != nil {
			o.logger.Printf("session %d handle error: %v", s.ID, err)
		}
	}
}

// handleDownstreamMessage processes a message from a downstream session.
func (o *Owner) handleDownstreamMessage(s *Session, msg *jsonrpc.Message) error {
	switch {
	case msg.IsNotification():
		// Suppress notifications/initialized for sessions that received a cached initialize response
		if msg.Method == "notifications/initialized" {
			o.mu.RLock()
			wasCached := o.cachedInitSessions[s.ID]
			o.mu.RUnlock()
			if wasCached {
				o.logger.Printf("session %d: suppressing notifications/initialized (cached init)", s.ID)
				return nil
			}
		}
		// Remap requestId for cancellation notifications so upstream can find the right in-flight request
		if msg.Method == "notifications/cancelled" {
			return o.forwardCancelledNotification(s, msg)
		}
		// NotificationHandler dispatch for SessionHandler mode
		if o.sessionHandler != nil {
			if nh, ok := o.sessionHandler.(muxcore.NotificationHandler); ok {
				project := muxcore.ProjectContext{
					ID:  muxcore.ProjectContextID(s.Cwd),
					Cwd: s.Cwd,
					Env: s.Env,
				}
				go nh.HandleNotification(context.Background(), project, msg.Raw)
			}
			return nil // notifications don't need forwarding to upstream
		}
		// Forward other notifications as-is to upstream
		if o.upstream == nil {
			return nil // snapshot owner — upstream not yet spawned, drop notification
		}
		return o.writeUpstream(msg.Raw)

	case msg.IsRequest():
		// Replay from cache if available (avoids upstream round-trip).
		// Check cache BEFORE upstream-dead check: snapshot owners have nil upstream
		// but valid caches — they must serve from cache, not return error.
		if cached := o.getCachedResponse(msg.Method); cached != nil {
			// For initialize: verify protocolVersion matches before replaying
			if msg.Method == "initialize" && !o.initFingerprintMatches(msg.Raw) {
				o.logger.Printf("session %d: initialize fingerprint mismatch, forwarding to upstream", s.ID)
				// Fall through to forward to upstream
			} else {
				return o.replayFromCache(s, msg, cached)
			}
		}

		// SessionHandler dispatch: structured path without pipe/remap/_meta.
		// Must come BEFORE the upstream nil check so SessionHandler-only owners
		// (where o.upstream == nil by design) can serve requests normally.
		if o.sessionHandler != nil {
			return o.dispatchToSessionHandler(s, msg)
		}

		// Fail fast if upstream is dead or not yet spawned (pipe-based mode only)
		if o.upstream == nil || o.upstreamDead.Load() {
			errResp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"upstream process exited"}}`, string(msg.ID))
			return s.WriteRaw([]byte(errResp))
		}

		// Track pending requests
		o.pendingRequests.Add(1)

		// Remap ID and forward to upstream
		newID := remap.Remap(s.ID, msg.ID)
		remapped, err := jsonrpc.ReplaceID(msg.Raw, newID)
		if err != nil {
			o.pendingRequests.Add(-1) // undo increment on error
			return fmt.Errorf("remap request: %w", err)
		}

		// Track inflight request for observability (mux_list --verbose)
		o.inflightTracker.Store(string(newID), &InflightRequest{
			Method:    msg.Method,
			Tool:      extractToolName(msg.Raw),
			SessionID: s.ID,
			StartTime: time.Now(),
		})

		// Tag cacheable methods for response interception
		if msg.Method == "initialize" || msg.Method == "tools/list" ||
			msg.Method == "prompts/list" || msg.Method == "resources/list" ||
			msg.Method == "resources/templates/list" {
			o.methodTags.Store(string(newID), msg.Method)
		}

		// Capture protocolVersion from the first initialize request for fingerprint matching
		if msg.Method == "initialize" {
			o.captureInitFingerprint(msg.Raw)
		}

		// Inject _meta.muxSessionId, _meta.muxCwd, and _meta.muxEnv when the upstream
		// needs to distinguish between CC sessions. Two cases:
		// 1. session-aware: upstream declared x-mux.sharing: session-aware
		// 2. in-process handler: single handler serves all CC sessions, always needs session identity
		o.mu.RLock()
		needsMeta := o.autoClassification == classify.ModeSessionAware || o.handlerFunc != nil
		o.mu.RUnlock()
		if needsMeta {
			injected, err := jsonrpc.InjectMeta(remapped, "muxSessionId", s.MuxSessionID)
			if err != nil {
				o.logger.Printf("session %d: failed to inject muxSessionId: %v", s.ID, err)
			} else {
				remapped = injected
			}
			// Inject per-session cwd so upstream knows the CC session's project directory
			if s.Cwd != "" {
				injected, err := jsonrpc.InjectMeta(remapped, "muxCwd", s.Cwd)
				if err != nil {
					o.logger.Printf("session %d: failed to inject muxCwd: %v", s.ID, err)
				} else {
					remapped = injected
				}
			}
			// Inject per-session env diff so upstream can use project-scope credentials
			if len(s.Env) > 0 {
				injected, err := jsonrpc.InjectMeta(remapped, "muxEnv", s.Env)
				if err != nil {
					o.logger.Printf("session %d: failed to inject muxEnv: %v", s.ID, err)
				} else {
					remapped = injected
				}
			}
		}

		// Track progressToken ownership for targeted notification routing
		o.trackProgressToken(s.ID, string(newID), msg.Raw)

		// Record the active session so server→client requests can be routed back
		o.sessionMgr.TrackRequest(string(newID), s.ID)

		// Start tool-call watchdog if the upstream declared x-mux.toolTimeout.
		// The watchdog synthesizes a timeout error response if the upstream
		// doesn't respond within the declared window. Prevents eternal hangs
		// when upstream deadlocks or crashes silently during a tool call.
		if msg.Method == "tools/call" && o.toolTimeoutNs.Load() > 0 {
			o.startToolWatchdog(string(newID), msg.ID, s, msg.Method)
		}

		return o.writeUpstream(remapped)

	case msg.IsResponse():
		// Client is responding to a server→client request (e.g., sampling/createMessage).
		// Forward as-is — the ID belongs to the upstream's request, no remapping needed.
		o.logger.Printf("session %d: forwarding client response to upstream (id=%s)", s.ID, string(msg.ID))
		return o.writeUpstream(msg.Raw)

	default:
		return fmt.Errorf("unexpected message type from downstream: %s", msg.Type)
	}
}

// dispatchToSessionHandler calls the SessionHandler directly instead of writing
// to the pipe. Runs in a goroutine for concurrent request handling.
func (o *Owner) dispatchToSessionHandler(s *Session, msg *jsonrpc.Message) error {
	project := muxcore.ProjectContext{
		ID:  muxcore.ProjectContextID(s.Cwd),
		Cwd: s.Cwd,
		Env: s.Env,
	}

	o.pendingRequests.Add(1)

	go func() {
		defer o.pendingRequests.Add(-1)
		defer func() {
			if r := recover(); r != nil {
				o.logger.Printf("session %d: HandleRequest panic: %v", s.ID, r)
				if errResp, marshalErr := buildJSONRPCErrorBytes(msg.ID, -32603, "internal error: handler panic"); marshalErr == nil {
					s.WriteRaw(errResp)
				}
			}
		}()

		// Build context: cancel on session disconnect or owner shutdown, and
		// optionally apply the per-call tool timeout. The WithTimeout wrapping
		// MUST happen before the watcher goroutine is spawned — otherwise the
		// goroutine captures the outer ctx and races the parent's ctx
		// reassignment on the `ctx, timeoutCancel = context.WithTimeout(...)`
		// line (caught by -race as a data race on the ctx variable,
		// dispatch_test.go:TestDispatchToSessionHandler_Timeout on CI).
		// The semantic bug is real too: if toolTimeout fires before s.Done()
		// or o.done, the watcher is still blocking on the OLD ctx.Done() and
		// leaks until one of the other two triggers.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if timeout := o.toolTimeoutNs.Load(); timeout > 0 {
			var timeoutCancel context.CancelFunc
			ctx, timeoutCancel = context.WithTimeout(ctx, time.Duration(timeout))
			defer timeoutCancel()
		}

		go func() {
			select {
			case <-s.Done():
				cancel()
			case <-o.done:
				cancel()
			case <-ctx.Done():
			}
		}()

		resp, err := o.sessionHandler.HandleRequest(ctx, project, msg.Raw)
		if err != nil {
			// Route through buildJSONRPCErrorBytes so error messages with JSON-special
			// characters (quotes, backslashes, newlines, Windows paths, control bytes)
			// are properly escaped and produce valid JSON for the CC client.
			var errMsg string
			if ctx.Err() != nil {
				errMsg = "request timeout"
			} else {
				errMsg = err.Error()
			}
			if errBytes, marshalErr := buildJSONRPCErrorBytes(msg.ID, -32603, errMsg); marshalErr == nil {
				resp = errBytes
			} else {
				o.logger.Printf("session %d: failed to marshal error response: %v", s.ID, marshalErr)
				resp = nil
			}
		}

		if resp != nil {
			s.WriteRaw(resp)
		}
	}()

	return nil
}

// sendProactiveInit sends an initialize request to the upstream immediately after
// starting, without waiting for a session to connect. This enables the init-ready
// gate: Daemon.Spawn blocks until init is cached, but init requires a request.
// The response is cached by handleUpstreamMessage → cacheResponse("initialize"),
// which closes initReady. Subsequent sessions get instant cached replay.
//
// Also sends notifications/initialized and tools/list to pre-populate caches.
func (o *Owner) sendProactiveInit() {
	initReq := `{"jsonrpc":"2.0","id":"mux-init-0","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"mcp-mux","version":"1.0.0"}}}`

	// Tag the request so handleUpstreamMessage caches the response.
	// Key must match string(msg.ID) which includes JSON quotes: "\"mux-init-0\"".
	o.methodTags.Store(`"mux-init-0"`, "initialize")
	o.pendingRequests.Add(1)

	if err := o.writeUpstream([]byte(initReq)); err != nil {
		o.logger.Printf("proactive init: write failed: %v", err)
		return
	}
	// Do NOT capture fingerprint from proactive init — let the first real
	// session's initialize capture it. Our synthetic protocolVersion may
	// differ from what CC actually sends, causing fingerprint mismatches.
	o.logger.Printf("proactive init: sent initialize request")

	// After init response is cached (signaled by initReady), send the
	// followup notifications/initialized + tools/list to pre-populate caches.
	go func() {
		select {
		case <-o.initReady:
		case <-o.done:
			return
		}
		if !o.InitSuccess() {
			return
		}

		// Send notifications/initialized (required by MCP after initialize)
		notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
		if err := o.writeUpstream([]byte(notif)); err != nil {
			o.logger.Printf("proactive init: notifications/initialized failed: %v", err)
			return
		}

		// Request tools/list to pre-populate tool cache + trigger classification
		toolsReq := `{"jsonrpc":"2.0","id":"mux-init-1","method":"tools/list","params":{}}`
		o.methodTags.Store(`"mux-init-1"`, "tools/list")
		o.pendingRequests.Add(1)
		if err := o.writeUpstream([]byte(toolsReq)); err != nil {
			o.logger.Printf("proactive init: tools/list failed: %v", err)
		}
	}()
}

// readUpstream reads responses from the upstream and routes them to the correct session.
func (o *Owner) readUpstream() {
	if o.upstream == nil {
		return // SessionHandler-only mode — no upstream to read
	}
	defer func() {
		o.upstreamDead.Store(true)
		o.drainInflightRequests() // send error responses for all pending requests
	}()
	for {
		line, err := o.upstream.ReadLine()
		if err != nil {
			if err != io.EOF {
				o.logger.Printf("upstream read error: %v", err)
			}
			return
		}

		// Any line from upstream counts as activity for reaper idle tracking.
		o.touchActivity()

		msg, err := jsonrpc.Parse(line)
		if err != nil {
			o.logger.Printf("upstream parse error: %v", err)
			continue
		}

		if err := o.handleUpstreamMessage(msg); err != nil {
			o.logger.Printf("upstream handle error: %v", err)
		}
	}
}

// handleUpstreamMessage routes a message from the upstream to the correct session.
// It also intercepts server→client requests like roots/list.
func (o *Owner) handleUpstreamMessage(msg *jsonrpc.Message) error {
	if msg.IsNotification() {
		// Route progress notifications to owning session instead of broadcast.
		//
		// If the token is NOT in progressOwners (e.g. the originating request
		// has already completed and its token was cleared, or the upstream
		// server invented a token instead of echoing the client-supplied one),
		// we DROP the notification. Broadcasting to all sessions was the old
		// fallback and is actively harmful: the MCP client receives a
		// notifications/progress with a token it has no record of and logs
		// "Received a progress notification for an unknown token" — which
		// some clients (Claude Code) treat as a transport-level protocol
		// error and tear the stdio connection down. Observed in production
		// with netcoredbg-mcp debugger long-polls: the first unknown-token
		// progress arriving after a tool completed caused CC to close the
		// stdio transport, which in turn killed the shim, which in turn
		// required a manual `/mcp` reconnect. Dropping silently (with a log
		// line) is strictly safer than broadcasting.
		if msg.Method == "notifications/progress" {
			if err := o.routeProgressNotification(msg.Raw); err != nil {
				o.logger.Printf("drop notifications/progress: %v (preventing transport tear-down in MCP client)", err)
			}
			return nil
		}
		// x-mux busy protocol: upstream declares long-running background work
		// so the reaper does not idle-kill it. Consumed at the mux layer —
		// not forwarded to sessions (the busy signal is a mux contract, not
		// an MCP-client concern).
		if msg.Method == "notifications/x-mux/busy" {
			o.handleBusyNotification(msg.Raw)
			return nil
		}
		if msg.Method == "notifications/x-mux/idle" {
			o.handleIdleNotification(msg.Raw)
			return nil
		}
		return o.broadcast(msg.Raw)
	}

	if msg.IsRequest() {
		// Server→client request (e.g., roots/list, sampling/createMessage)
		return o.handleUpstreamRequest(msg)
	}

	if !msg.IsResponse() {
		return fmt.Errorf("unexpected message type from upstream: %s", msg.Type)
	}

	// If the watchdog already claimed this request as timed-out, drop the
	// late upstream response to prevent duplicate delivery to the session.
	// Note: we keep methodTags caching (upstream response is still useful
	// for future cached replays), just skip the session delivery path.
	if _, timedOut := o.timedOutIDs.LoadAndDelete(string(msg.ID)); timedOut {
		o.logger.Printf("dropping late response for timed-out request id=%s", string(msg.ID))
		// Still cache if tagged — the response body is valid even if late
		if methodRaw, ok := o.methodTags.LoadAndDelete(string(msg.ID)); ok {
			o.cacheResponse(methodRaw.(string), msg.Raw)
		}
		// Clean up progress tokens for the (watchdog-timed-out) request (FIX 1).
		o.clearProgressTokensForRequest(string(msg.ID))
		return nil
	}

	// Atomically claim the inflight entry. If the watchdog claimed it first,
	// the timedOutIDs check above would have caught it — but we double-check
	// via LoadAndDelete here to handle any concurrent claim race safely.
	if _, claimed := o.inflightTracker.LoadAndDelete(string(msg.ID)); !claimed {
		// Inflight already gone — either watchdog claimed it between checks,
		// or this is a proactive/untracked response (mux-init-0, mux-init-1).
		// Still process cache + routing because proactive responses aren't
		// tracked in inflight but DO need caching.
	}
	o.pendingRequests.Add(-1)
	o.sessionMgr.CompleteRequest(string(msg.ID))

	// Clean up any progress tokens registered for this request (FIX 1).
	o.clearProgressTokensForRequest(string(msg.ID))

	// Cache response if this was a tagged cacheable request
	if methodRaw, ok := o.methodTags.LoadAndDelete(string(msg.ID)); ok {
		o.cacheResponse(methodRaw.(string), msg.Raw)
	}

	// Deremap the ID to find the target session
	result, err := remap.Deremap(msg.ID)
	if err != nil {
		return fmt.Errorf("deremap response id: %w", err)
	}

	o.logger.Printf("response routing: id=%s → session %d (original id=%s)",
		string(msg.ID), result.SessionID, string(result.OriginalID))

	// Replace the remapped ID with the original
	restored, err := jsonrpc.ReplaceID(msg.Raw, result.OriginalID)
	if err != nil {
		return fmt.Errorf("restore id: %w", err)
	}

	// Send to the correct session
	o.mu.RLock()
	session, ok := o.sessions[result.SessionID]
	o.mu.RUnlock()

	if !ok {
		o.logger.Printf("session %d not found for response (may have disconnected)", result.SessionID)
		return nil
	}

	if err := session.WriteRaw(restored); err != nil {
		o.logger.Printf("session %d: response write error: %v", result.SessionID, err)
		return err
	}
	o.logger.Printf("session %d: response delivered (%d bytes)", result.SessionID, len(restored))
	return nil
}

// handleUpstreamRequest handles server→client requests from the upstream.
// Most requests (roots/list, sampling, elicitation) are forwarded to the active
// session — CC itself responds with correct per-session data.
func (o *Owner) handleUpstreamRequest(msg *jsonrpc.Message) error {
	switch msg.Method {
	case "roots/list":
		// Forward to the active session — CC knows its own roots.
		// Falls back to local respondToRootsList if no active session.
		return o.routeToLastActiveSession(msg)
	case "ping":
		// Respond locally with empty result — no client involvement needed
		o.logger.Printf("upstream sent ping, responding locally")
		return o.respondToPing(msg.ID)
	default:
		// Route to the last active session (e.g., sampling/createMessage, elicitation/create)
		return o.routeToLastActiveSession(msg)
	}
}

// respondToRootsList sends a roots/list response to the upstream with ALL known cwds.
// Used as fallback when no active session can handle roots/list directly.
// In normal operation, roots/list is forwarded to the active session (CC answers).
func (o *Owner) respondToRootsList(id json.RawMessage) error {
	type rootEntry struct {
		URI  string `json:"uri"`
		Name string `json:"name,omitempty"`
	}

	o.mu.RLock()
	var roots []rootEntry
	for cwd := range o.cwdSet {
		if cwd != "" {
			roots = append(roots, rootEntry{URI: pathToFileURI(cwd), Name: filepath.Base(cwd)})
		}
	}
	o.mu.RUnlock()

	// Fallback if no cwds registered
	if len(roots) == 0 {
		fallback, _ := os.Getwd()
		roots = []rootEntry{{URI: pathToFileURI(fallback), Name: filepath.Base(fallback)}}
	}

	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Roots []rootEntry `json:"roots"`
		} `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
	}
	resp.Result.Roots = roots

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal roots response: %w", err)
	}

	return o.writeUpstream(data)
}

// respondToPing sends an empty result to the upstream in response to a ping.
func (o *Owner) respondToPing(id json.RawMessage) error {
	resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(id))
	return o.writeUpstream([]byte(resp))
}

// routeToLastActiveSession forwards a server→client request to the most recently
// active session with an in-flight request. Uses SessionManager.ResolveCallback()
// for causal routing. Falls back to local responses when no session is available.
func (o *Owner) routeToLastActiveSession(msg *jsonrpc.Message) error {
	ctx := o.sessionMgr.ResolveCallback()
	if ctx == nil {
		o.logger.Printf("no active session for server request %s", msg.Method)
		switch msg.Method {
		case "roots/list":
			// Fallback: answer locally from cwdSet when no session can handle it
			o.logger.Printf("roots/list fallback: responding locally with cwdSet")
			return o.respondToRootsList(msg.ID)
		case "elicitation/create":
			return o.respondToElicitationCancel(msg.ID)
		default:
			return o.respondWithError(msg.ID, -32603, "no active session available")
		}
	}

	session := ctx.Session
	o.logger.Printf("routing server request %s to session %d (async)", msg.Method, session.ID)
	// Use async SendNotification to prevent blocking readUpstream.
	// Server→client requests (roots/list, sampling) go through the notification
	// channel so upstream can continue producing output. The session's drain
	// goroutine delivers it to CC, and CC responds when ready.
	session.SendNotification(msg.Raw)
	return nil
}

// respondToElicitationCancel sends an elicitation cancel response to upstream.
func (o *Owner) respondToElicitationCancel(id json.RawMessage) error {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Action string `json:"action"`
		} `json:"result"`
	}{JSONRPC: "2.0", ID: id}
	resp.Result.Action = "cancel"
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal elicitation cancel: %w", err)
	}
	return o.writeUpstream(data)
}

// respondWithError sends a JSON-RPC error response to upstream.
func (o *Owner) respondWithError(id json.RawMessage, code int, message string) error {
	data, err := buildJSONRPCErrorBytes(id, code, message)
	if err != nil {
		return fmt.Errorf("marshal error response: %w", err)
	}
	return o.writeUpstream(data)
}

// buildJSONRPCErrorBytes constructs a JSON-RPC 2.0 error response as bytes.
// Uses json.Marshal on a struct to guarantee valid escaping for any message
// content — plain strings, quotes, backslashes, newlines, Windows paths,
// and control characters all round-trip correctly.
//
// Callers that need the response bytes (e.g., to send to a downstream session
// rather than upstream) use this directly; callers that want the full
// write-to-upstream side effect use respondWithError instead.
func buildJSONRPCErrorBytes(id json.RawMessage, code int, message string) ([]byte, error) {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	resp.Error.Code = code
	resp.Error.Message = message
	return json.Marshal(resp)
}

// routeProgressNotification sends a notifications/progress to the session that
// owns the progressToken, instead of broadcasting to all sessions.
//
// Uses the session's async notification channel (SendNotification) rather than
// synchronous WriteRaw. This matches how broadcast() and server-to-client
// requests are delivered elsewhere in the codebase, and prevents a single slow
// session from stalling the upstream reader loop for up to the write-deadline
// (30 s). Progress notifications are strictly informational — dropping under
// backpressure is preferable to blocking the upstream multiplexer.
func (o *Owner) routeProgressNotification(raw []byte) error {
	var notif struct {
		Params struct {
			ProgressToken json.RawMessage `json:"progressToken"`
			Total         *float64        `json:"total"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &notif); err != nil {
		return fmt.Errorf("parse progress notification: %w", err)
	}
	if notif.Params.ProgressToken == nil {
		return fmt.Errorf("no progressToken in notification")
	}

	token := string(notif.Params.ProgressToken)

	o.mu.RLock()
	sessionID, ok := o.progressOwners[token]
	session := o.sessions[sessionID]
	o.mu.RUnlock()

	if !ok || session == nil {
		return fmt.Errorf("no owner for progressToken %s", token)
	}

	session.SendNotification(raw)

	// Record that real progress arrived so the synthetic reporter can back off.
	o.recordRealProgress(token, notif.Params.Total != nil)
	return nil
}

// trackProgressToken extracts _meta.progressToken from a request and records
// which session owns it, enabling targeted progress notification routing.
// requestID is the remapped (upstream-facing) request ID; it is stored so that
// clearProgressTokensForRequest can clean up when the response arrives.
func (o *Owner) trackProgressToken(sessionID int, requestID string, raw []byte) {
	var req struct {
		Params struct {
			Meta struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	if req.Params.Meta.ProgressToken == nil {
		return
	}
	token := string(req.Params.Meta.ProgressToken)
	o.mu.Lock()
	o.progressOwners[token] = sessionID
	o.progressTokenRequestID[token] = requestID
	o.requestToTokens[requestID] = append(o.requestToTokens[requestID], token)
	o.mu.Unlock()
}

// clearProgressTokensForRequest removes all progress tokens associated with the
// given remapped request ID. Called when:
//   - the upstream response for that request arrives (normal completion),
//   - notifications/cancelled is received for that request (client abort), or
//   - the owning session is removed (session died before the call completed).
//
// Must be called with o.mu held for write, or acquire it internally.
// This variant acquires the lock itself (safe to call from any goroutine).
func (o *Owner) clearProgressTokensForRequest(requestID string) {
	o.mu.Lock()
	tokens := o.requestToTokens[requestID]
	for _, token := range tokens {
		delete(o.progressOwners, token)
		delete(o.progressTokenRequestID, token)
	}
	delete(o.requestToTokens, requestID)
	o.mu.Unlock()

	// Clean up dedup state via the Tracker so no stale suppression remains.
	o.progressTracker.Cleanup(tokens)
}

// sendRootsListChanged notifies the upstream that roots have changed.
func (o *Owner) sendRootsListChanged() {
	if o.upstream == nil {
		return // snapshot owner — upstream not yet spawned
	}
	notification := `{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`
	if err := o.writeUpstream([]byte(notification)); err != nil {
		o.logger.Printf("failed to send roots/list_changed: %v", err)
	}
}

// pathToFileURI converts a filesystem path to a file:// URI.
func pathToFileURI(p string) string {
	// Normalize path separators
	p = filepath.ToSlash(p)
	// Windows paths like C:/foo → file:///C:/foo
	if len(p) >= 2 && p[1] == ':' {
		return "file:///" + p
	}
	// Unix paths like /home/user → file:///home/user
	return "file://" + p
}

// broadcast sends a message to all connected sessions.
func (o *Owner) broadcast(data []byte) error {
	// Invalidate caches for list-changed notifications before forwarding
	o.invalidateCacheIfNeeded(data)

	o.mu.RLock()
	sessions := make([]*Session, 0, len(o.sessions))
	for _, s := range o.sessions {
		sessions = append(sessions, s)
	}
	o.mu.RUnlock()

	// Async broadcast: queue notifications via SendNotification (non-blocking).
	// Each session has a drain goroutine that writes to IPC at its own pace.
	// This prevents the upstream reader goroutine from deadlocking when a
	// session's IPC write is slow — upstream can keep producing output.
	for _, s := range sessions {
		s.SendNotification(data)
	}
	return nil
}

// broadcastListChanged sends tools/prompts/resources list_changed notifications
// to all connected sessions. Called after upstream restarts to prompt CC to
// re-fetch its tool/prompt/resource lists. Uses async delivery (SendNotification)
// so the caller is never blocked by a slow session.
func (o *Owner) broadcastListChanged() {
	notifications := []string{
		`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`,
		`{"jsonrpc":"2.0","method":"notifications/prompts/list_changed"}`,
		`{"jsonrpc":"2.0","method":"notifications/resources/list_changed"}`,
	}
	o.mu.RLock()
	sessions := make([]*Session, 0, len(o.sessions))
	for _, s := range o.sessions {
		sessions = append(sessions, s)
	}
	o.mu.RUnlock()

	if len(sessions) == 0 {
		return
	}

	for _, s := range sessions {
		for _, notif := range notifications {
			s.SendNotification([]byte(notif))
		}
	}
	o.logger.Printf("broadcast list_changed to %d sessions", len(sessions))
}

// ownerNotifier implements muxcore.Notifier by routing notifications through
// the Owner's session manager.
type ownerNotifier struct {
	owner *Owner
}

func (n *ownerNotifier) Notify(projectID string, notification []byte) error {
	// Collect ALL sessions matching projectID under RLock, then release before
	// WriteRaw — s.WriteRaw may block up to 30 s on a slow IPC consumer (session
	// write deadline). Holding o.mu.RLock() during that wait stalls every goroutine
	// needing o.mu.Lock() (addSession, removeSession, cacheResponse, progress token
	// cleanup). This pattern mirrors Broadcast below.
	//
	// In Shared mode multiple CC sessions can share the same Cwd (same project),
	// so we must notify ALL of them, not just the first one found (which would be
	// random due to Go map iteration order).
	n.owner.mu.RLock()
	var targets []*Session
	for _, s := range n.owner.sessions {
		if muxcore.ProjectContextID(s.Cwd) == projectID {
			targets = append(targets, s)
		}
	}
	n.owner.mu.RUnlock()
	if len(targets) == 0 {
		return fmt.Errorf("no session found for project %s", projectID)
	}
	var lastErr error
	for _, s := range targets {
		if err := s.WriteRaw(notification); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (n *ownerNotifier) Broadcast(notification []byte) {
	n.owner.mu.RLock()
	sessions := make([]*Session, 0, len(n.owner.sessions))
	for _, s := range n.owner.sessions {
		sessions = append(sessions, s)
	}
	n.owner.mu.RUnlock()
	for _, s := range sessions {
		s.WriteRaw(notification)
	}
}

// removeSession removes a session from the owner.
func (o *Owner) removeSession(s *Session) {
	o.mu.Lock()
	delete(o.sessions, s.ID)
	remaining := len(o.sessions)
	// Clean up progress tokens owned by this session (FIX 1: session died
	// before its tool call completed — prevent permanent reaper veto).
	for token, ownerID := range o.progressOwners {
		if ownerID == s.ID {
			reqID := o.progressTokenRequestID[token]
			delete(o.progressOwners, token)
			delete(o.progressTokenRequestID, token)
			delete(o.requestToTokens, reqID)
		}
	}
	o.mu.Unlock()

	o.sessionMgr.RemoveSession(s.ID)
	o.touchActivity()
	o.logger.Printf("session %d disconnected (%d remaining)", s.ID, remaining)

	if o.sessionHandler != nil {
		if lc, ok := o.sessionHandler.(muxcore.ProjectLifecycle); ok {
			go lc.OnProjectDisconnect(muxcore.ProjectContextID(s.Cwd))
		}
	}

	if remaining > 0 {
		// Notify upstream that roots may have changed (client left)
		o.sendRootsListChanged()
	} else if o.onZeroSessions != nil {
		o.onZeroSessions(o.serverID)
	}
}

// acceptLoop accepts new IPC connections and creates sessions for them.
//
// If listener.Close() is called WITHOUT closing listenerDone, Accept() returns
// "use of closed network connection" forever — previously this caused a tight
// spin loop (711 errors/sec observed) that killed the daemon with log spam.
// Now we detect the "closed" error class and exit cleanly; other errors get a
// small backoff to prevent CPU saturation.
func (o *Owner) acceptLoop() {
	for {
		conn, err := o.listener.Accept()
		if err != nil {
			select {
			case <-o.done:
				return // full shutdown
			case <-o.listenerDone:
				return // listener closed (auto-isolation or shutdown)
			default:
			}
			// Detect "listener closed" by error type — if we can't Accept again,
			// don't spin. This catches the case where listener.Close() was called
			// but listenerDone wasn't signaled (bug safety net).
			if errors.Is(err, net.ErrClosed) {
				o.logger.Printf("accept loop exiting: listener closed (%v)", err)
				return
			}
			o.logger.Printf("accept error: %v", err)
			time.Sleep(100 * time.Millisecond) // backoff to prevent CPU saturation
			continue
		}

		var token string
		var reader io.Reader = conn
		if o.tokenHandshake {
			token, reader = readToken(conn)
		}
		// reader may be io.MultiReader if readToken prepended unconsumed bytes.
		// conn is always the writer and closer.
		s := NewSession(reader, conn)
		// io.MultiReader doesn't implement io.Closer — set closer to conn explicitly
		// so Close() can forcefully disconnect the IPC connection.
		s.SetCloser(conn)
		if token != "" {
			o.sessionMgr.Bind(token, s) // sets s.Cwd from pre-registered token
		}
		o.AddSession(s)
	}
}

// HandleShutdown implements control.CommandHandler.
// It performs a graceful drain: stops accepting new connections,
// waits for pending requests to complete (up to timeout), then shuts down.
func (o *Owner) HandleShutdown(drainTimeoutMs int) string {
	if drainTimeoutMs <= 0 {
		// Force shutdown — no drain
		go o.Shutdown()
		return "force shutdown initiated"
	}

	go func() {
		// Close IPC listener to stop new connections.
		// Use closeListener() to signal listenerDone — otherwise acceptLoop spins
		// in a tight error loop ("use of closed network connection") at CPU speed,
		// producing 700+ log lines/sec and eventually killing the daemon.
		o.closeListener()

		timeout := time.Duration(drainTimeoutMs) * time.Millisecond
		deadline := time.After(timeout)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-deadline:
				pending := o.pendingRequests.Load()
				if pending > 0 {
					o.logger.Printf("drain timeout: %d requests still pending, forcing shutdown", pending)
				}
				o.Shutdown()
				return
			case <-ticker.C:
				if o.pendingRequests.Load() <= 0 {
					o.logger.Printf("drain complete, shutting down")
					o.Shutdown()
					return
				}
			}
		}
	}()

	return fmt.Sprintf("draining (timeout %dms)", drainTimeoutMs)
}

// HandleStatus implements control.CommandHandler.
func (o *Owner) HandleStatus() map[string]any {
	return o.Status()
}

// evictExtraSessions disconnects all sessions except the first one.
// Called when an owner is reclassified as isolated after optimistic dedup
// connected multiple sessions. Evicted sessions get IPC EOF → their shims
// reconnect → daemon spawns separate owners for each.
func (o *Owner) evictExtraSessions() {
	o.mu.Lock()
	if len(o.sessions) <= 1 {
		o.mu.Unlock()
		return
	}

	// Find the lowest session ID (first connected) — keep it.
	keepID := -1
	for id := range o.sessions {
		if keepID == -1 || id < keepID {
			keepID = id
		}
	}

	// Collect sessions to evict.
	var evict []*Session
	for id, s := range o.sessions {
		if id != keepID {
			evict = append(evict, s)
		}
	}
	o.mu.Unlock()

	if len(evict) > 0 {
		o.logger.Printf("evicting %d extra sessions (keeping session %d) — server classified isolated after dedup", len(evict), keepID)
	}
	for _, s := range evict {
		s.Close() // triggers IPC EOF → shim reconnects → gets own owner
	}
}

func (o *Owner) resetCwdSetToPrimary() (string, int) {
	primaryCwd := serverid.CanonicalizePath(o.cwd)
	o.cwdSet = map[string]bool{primaryCwd: true}
	return primaryCwd, 1
}

// closeListener stops the IPC listener and removes the socket file.
// Safe to call multiple times (uses sync.Once).
// Nil-safe: owners constructed for tests may not have a listener.
func (o *Owner) closeListener() {
	o.closeListenerOnce.Do(func() {
		close(o.listenerDone)
		if o.listener != nil {
			o.listener.Close()
		}
		if o.ipcPath != "" {
			ipc.Cleanup(o.ipcPath)
		}
	})
}

// drainInflightRequests sends JSON-RPC error responses for all in-flight requests
// when the upstream process dies. Without this, clients wait forever for responses
// that will never come.
func (o *Owner) drainInflightRequests() {
	entries := o.sessionMgr.DrainInflight()
	if len(entries) == 0 {
		return
	}
	o.logger.Printf("upstream died: sending error responses for %d in-flight requests", len(entries))

	o.mu.RLock()
	defer o.mu.RUnlock()

	for _, entry := range entries {
		result, err := remap.Deremap(json.RawMessage(`"` + entry.RemappedID + `"`))
		if err != nil {
			o.logger.Printf("drainInflight: deremap error for %s: %v", entry.RemappedID, err)
			continue
		}
		session, ok := o.sessions[result.SessionID]
		if !ok {
			continue // session already disconnected
		}
		errResp := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"upstream process exited"}}`,
			string(result.OriginalID),
		)
		if writeErr := session.WriteRaw([]byte(errResp)); writeErr != nil {
			o.logger.Printf("drainInflight: write error to session %d: %v", result.SessionID, writeErr)
		}
		o.pendingRequests.Add(-1)
		o.inflightTracker.Delete(entry.RemappedID)
	}
}

// Shutdown stops the owner, closing all sessions and the upstream.
// Cleanup completes before Done() is signaled, ensuring sockets are removed
// before the process exits.
func (o *Owner) Shutdown() {
	o.shutdownOnce.Do(func() {
		// Close control server first and clean up its socket
		if o.controlServer != nil {
			socketPath := o.controlServer.SocketPath()
			o.controlServer.Close()
			ipc.Cleanup(socketPath)
		}

		o.closeListener()

		o.mu.Lock()
		for _, s := range o.sessions {
			s.Close()
		}
		o.sessions = make(map[int]*Session)
		o.mu.Unlock()

		o.mu.Lock()
		up := o.upstream
		o.mu.Unlock()
		if up != nil {
			up.Close()
		}

		o.logger.Printf("owner shut down")

		// Signal done AFTER cleanup, so main goroutine doesn't exit early
		close(o.done)
	})
}

// Done returns a channel closed when the owner has shut down.
func (o *Owner) Done() <-chan struct{} {
	return o.done
}

// String implements fmt.Stringer to provide a compact identifier for
// the suture supervisor's logging. Without this, suture falls back to
// fmt.Sprintf("%#v", o) which dumps all fields — including raw []byte
// caches (initResp, toolList can be 10s of KB) — on every termination
// event. The reflected dump was 25 KB per log line and consumed 93%
// of log volume during supervisor restart cycles (see aimux→mcp-mux
// issue #5 comment 2026-04-10).
func (o *Owner) String() string {
	if o == nil {
		return "owner[<nil>]"
	}
	sid := o.serverID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	return fmt.Sprintf("owner[%s %s]", sid, o.command)
}

// closedChan is a pre-closed channel used by upstreamDeadCh when the owner
// has no upstream process. Returning a closed channel lets Serve observe
// upstream-missing as a failure (so suture can backoff/restart) instead of
// blocking forever. Package-level to avoid per-call allocation.
var closedChan = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// Serve implements the suture.Service interface, enabling owner lifecycle
// management via supervisor trees with exponential backoff on restart.
//
// Serve blocks until one of:
//   - ctx is cancelled (external shutdown → returns ctx.Err() after Shutdown)
//   - owner.done is closed via Shutdown (clean exit → returns nil, no restart)
//   - upstream dies fatally (returns error → suture restarts with backoff,
//     unless classification is isolated or ErrDoNotRestart is returned)
//
// Isolated owners intentionally return suture.ErrDoNotRestart on upstream
// death — they should not be auto-restarted because each CC session creates
// its own dedicated isolated owner.
//
// Race ordering: upstream death is checked FIRST (non-blocking) before entering
// the select. This handles the case where o.done closes concurrently with
// upstream death (daemon's onUpstreamExit callback may Shutdown before Serve
// observes the upstream Done channel). Without this check, Serve would
// non-deterministically return nil instead of the expected error/ErrDoNotRestart,
// making restart/backoff policies unreliable.
func (o *Owner) Serve(ctx context.Context) error {
	// SessionHandler-only: no upstream process to monitor.
	// Block until ctx is cancelled or Shutdown is called, then return cleanly.
	if o.sessionHandler != nil && o.upstream == nil {
		select {
		case <-ctx.Done():
			o.Shutdown()
			return ctx.Err()
		case <-o.done:
			return nil
		}
	}

	for {
		// Snapshot the current dead/pending channel under a single lock read.
		// If backgroundSpawnCh is non-nil, upstreamDeadCh returns it (blocking Serve
		// until the spawn goroutine finishes). If nil and upstream is also nil,
		// upstreamDeadCh returns closedChan (immediate death). If upstream is set,
		// upstreamDeadCh returns proc.Done.
		deadCh := o.upstreamDeadCh()

		// Non-blocking priority checks: done/ctx take precedence over upstream death.
		// Without these, a shutdown owner with nil upstream (deadCh == closedChan)
		// would return error → suture restart → immediate return → tight CPU spin.
		select {
		case <-o.done:
			return nil // owner already shut down — clean exit, no restart
		default:
		}
		select {
		case <-ctx.Done():
			o.Shutdown()
			return ctx.Err()
		default:
		}
		select {
		case <-deadCh:
			// If this was backgroundSpawnCh (now closed and cleared), loop to
			// re-evaluate — the upstream may now be set or definitively absent.
			o.mu.RLock()
			bgCh := o.backgroundSpawnCh
			o.mu.RUnlock()
			if bgCh == nil && deadCh != closedChan {
				// backgroundSpawnCh just fired; upstream may be set now — loop.
				continue
			}
			return o.upstreamDeathResult()
		default:
		}

		select {
		case <-ctx.Done():
			// External cancellation (daemon shutdown, supervisor stop)
			o.Shutdown()
			return ctx.Err()
		case <-o.done:
			// Owner already shut down cleanly (external Shutdown call, mux_stop, etc.)
			return nil
		case <-deadCh:
			// Same logic as the non-blocking check above.
			o.mu.RLock()
			bgCh := o.backgroundSpawnCh
			o.mu.RUnlock()
			if bgCh == nil && deadCh != closedChan {
				// backgroundSpawnCh just fired; loop to re-evaluate.
				continue
			}
			return o.upstreamDeathResult()
		}
	}
}

// upstreamDeathResult determines the Serve return value when upstream has died.
// Checks classification: isolated owners return ErrDoNotRestart (no auto-restart),
// others return an error to trigger supervisor backoff+restart.
//
// Special case (BUG-001 / FR-1): if the owner has NEVER had a live upstream
// (up == nil and no background spawn pending), the "death" signal came from
// closedChan — meaning background spawn failed before upstream was ever set.
// Returning an error here would cause suture to restart Serve, which would
// immediately re-observe the same nil state and return the same error — a
// tight CPU spin bounded only by FailureThreshold. Return ErrDoNotRestart
// for this case so the supervisor stops cycling. The owner is effectively
// broken and will be cleaned up by the reaper on the next sweep.
func (o *Owner) upstreamDeathResult() error {
	o.mu.RLock()
	mode := o.autoClassification
	hasUpstream := o.upstream != nil
	hasPendingSpawn := o.backgroundSpawnCh != nil
	o.mu.RUnlock()
	if !hasUpstream && !hasPendingSpawn {
		// Owner never had a live upstream (failed background spawn, or no
		// spawn ever attempted). No restart — otherwise suture spins.
		return suture.ErrDoNotRestart
	}
	if mode == classify.ModeIsolated {
		// Isolated servers should not be restarted by the supervisor.
		// Each CC session spawns its own dedicated isolated owner.
		return suture.ErrDoNotRestart
	}
	return fmt.Errorf("owner %s: upstream exited", o.serverID[:8])
}

// upstreamDeadCh returns a channel that closes when the upstream process dies.
//
// Three cases:
//  1. Upstream is set: return proc.Done (normal path).
//  2. Background spawn is pending (backgroundSpawnCh != nil): return backgroundSpawnCh.
//     Serve blocks on it; when it closes Serve loops and re-checks the upstream.
//  3. Upstream is nil and no spawn pending (spawn failed or never started): return
//     closedChan so Serve observes it as failure and suture applies backoff/restart.
func (o *Owner) upstreamDeadCh() <-chan struct{} {
	o.mu.RLock()
	up := o.upstream
	bgCh := o.backgroundSpawnCh
	o.mu.RUnlock()
	if up != nil {
		return up.Done
	}
	if bgCh != nil {
		// Template owner: upstream is starting in background. Block Serve until done.
		return bgCh
	}
	return closedChan
}

// ServerID returns the server identity hash for this owner.
func (o *Owner) ServerID() string {
	return o.serverID
}

// IPCPath returns the IPC socket path for this owner.
func (o *Owner) IPCPath() string {
	return o.ipcPath
}

// IsAccepting returns true if the IPC listener is still active (not closed).
// Isolated owners close their listener after the first session connects.
func (o *Owner) IsAccepting() bool {
	select {
	case <-o.listenerDone:
		return false
	default:
		return true
	}
}

// InitReady returns a channel that is closed when the owner's upstream has responded
// to the first initialize request (response cached for replay) OR the upstream has
// exited. Used by Daemon.Spawn to block until the server is ready, preventing CC
// from timing out on slow-starting upstreams.
func (o *Owner) InitReady() <-chan struct{} {
	return o.initReady
}

// InitSuccess returns true if the initialize response was successfully cached.
// Returns false if the upstream exited before responding to initialize.
// Must only be called after InitReady() is closed.
func (o *Owner) InitSuccess() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.initDone
}

// Classified returns a channel that is closed when the owner's auto-classification
// is determined (shared, isolated, or session-aware). Used by daemon's findSharedOwner
// to wait for classification before dedup decision.
func (o *Owner) Classified() <-chan struct{} {
	return o.classified
}

// IsClassifiedShareable returns true if the owner has been classified AND
// the classification allows cross-CWD sharing (shared or session-aware).
// Returns false if not yet classified or classified as isolated.
func (o *Owner) IsClassifiedShareable() bool {
	select {
	case <-o.classified:
	default:
		return false // not yet classified
	}
	o.mu.RLock()
	mode := o.autoClassification
	o.mu.RUnlock()
	return mode == classify.ModeShared || mode == classify.ModeSessionAware
}

// MarkClassified forces the classified channel closed. Used by tests that need
// to bypass the normal init/tools handshake classification flow.
func (o *Owner) MarkClassified() {
	o.classifiedOnce.Do(func() {
		if o.classified != nil {
			close(o.classified)
		}
	})
}

// AddCwd registers an additional project cwd for this owner.
// Used by dedup: when a second project reuses a shared owner,
// its cwd is added so roots/list includes all project roots.
//
// Paths are canonicalized (lowercased on Windows, symlinks resolved) to
// prevent memory leaks from case/slash variants being treated as distinct.
// Before normalization: D:\Dev\foo, D:/Dev/foo, d:\dev\foo all grew cwdSet.
// sendRootsListChanged is called ONLY when a new cwd is actually added,
// preventing log spam and upstream notification storms on duplicate calls.
func (o *Owner) AddCwd(cwd string) {
	if cwd == "" {
		return
	}
	canonical := serverid.CanonicalizePath(cwd)
	o.mu.Lock()
	if o.cwdSet[canonical] {
		o.mu.Unlock()
		return // already registered — silent no-op, no log, no notify
	}
	o.cwdSet[canonical] = true
	count := len(o.cwdSet)
	o.mu.Unlock()
	o.logger.Printf("added cwd: %s (now %d roots)", canonical, count)
	// Notify upstream ONLY on actual change
	o.sendRootsListChanged()
}

// Command returns the upstream command.
func (o *Owner) Command() string {
	return o.command
}

// Args returns the upstream command arguments.
func (o *Owner) Args() []string {
	return o.args
}

// SessionCount returns the number of connected sessions.
func (o *Owner) SessionCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.sessions)
}

// PendingRequests returns the number of in-flight requests.
func (o *Owner) PendingRequests() int64 {
	return o.pendingRequests.Load()
}

// ActiveProgressTokens returns the number of tracked progressTokens —
// long-running tool calls that are still streaming notifications/progress
// from upstream. While non-zero, the reaper must not kill this owner.
func (o *Owner) ActiveProgressTokens() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.progressOwners)
}

// UpstreamDead returns true if the upstream process has exited.
func (o *Owner) UpstreamDead() bool {
	return o.upstreamDead.Load()
}

// LastActivity returns the unix-nano timestamp of the last activity seen by
// the owner (inbound upstream message, outbound session→upstream message, or
// a session connect/disconnect event).
func (o *Owner) LastActivity() time.Time {
	ns := o.lastActivityNs.Load()
	if ns == 0 {
		return o.startTime
	}
	return time.Unix(0, ns)
}

// touchActivity records the current time as the last activity timestamp.
// Called on every MCP message (both directions), session add/remove, and
// upstream init. The reaper uses this to decide whether an owner is idle.
//
// Throttled to one atomic write per second: the reaper only needs
// second-level precision, and avoiding a store on every streaming message
// reduces cache-line contention on high-traffic owners (e.g. LLM responses).
func (o *Owner) touchActivity() {
	now := time.Now().UnixNano()
	last := o.lastActivityNs.Load()
	if now-last >= int64(time.Second) {
		o.lastActivityNs.Store(now)
	}
}

// writeUpstream is the single choke point for sending a line to upstream
// stdin. Wrapping upstream.WriteLine lets us touch activity on every
// outbound message without scattering the call across 15+ sites.
//
// SessionHandler-only owners never call writeUpstream — requests dispatch
// directly via dispatchToSessionHandler, and notification forwarding
// returns early when o.upstream == nil.
func (o *Owner) writeUpstream(data []byte) error {
	o.touchActivity()
	return o.upstream.WriteLine(data)
}

// busyDeclaration is a signal from upstream that it is doing long-running
// work without an active JSON-RPC request or progress token. Used by the
// reaper to exempt such owners from idle-based kill.
type busyDeclaration struct {
	StartedAt     time.Time
	EstimatedEnd  time.Time // StartedAt + estimatedDuration
	HardExpiresAt time.Time // safety cap: StartedAt + estimatedDuration * 2
	Task          string
	SessionID     int // session that declared the work (-1 if unattributed)
}

// HasActiveBusyWork returns true if the owner has any unexpired busy
// declarations. The reaper must not kill an owner while this is true.
// Expired declarations are garbage-collected inside the call.
func (o *Owner) HasActiveBusyWork() bool {
	now := time.Now()
	o.busyMu.Lock()
	defer o.busyMu.Unlock()
	active := false
	for id, d := range o.busyDeclarations {
		if now.After(d.HardExpiresAt) {
			delete(o.busyDeclarations, id)
			continue
		}
		active = true
	}
	return active
}

// RegisterBusy records a long-running work declaration from upstream.
// The reaper will not kill the owner while any non-expired declaration
// exists. estimatedDuration of 0 falls back to 10 minutes; values above
// 24 hours are clamped to prevent overflow and forgotten declarations from
// living indefinitely.
const maxBusyDuration = 24 * time.Hour

func (o *Owner) RegisterBusy(id string, startedAt time.Time, estimatedDuration time.Duration, task string, sessionID int) {
	if estimatedDuration <= 0 {
		estimatedDuration = 10 * time.Minute
	}
	if estimatedDuration > maxBusyDuration {
		estimatedDuration = maxBusyDuration
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	d := busyDeclaration{
		StartedAt:     startedAt,
		EstimatedEnd:  startedAt.Add(estimatedDuration),
		HardExpiresAt: startedAt.Add(estimatedDuration * 2),
		Task:          task,
		SessionID:     sessionID,
	}
	o.busyMu.Lock()
	if o.busyDeclarations == nil {
		o.busyDeclarations = map[string]busyDeclaration{}
	}
	o.busyDeclarations[id] = d
	o.busyMu.Unlock()
	o.touchActivity()
}

// ClearBusy removes a busy declaration (upstream signalled the long-running
// work is finished).
func (o *Owner) ClearBusy(id string) {
	o.busyMu.Lock()
	delete(o.busyDeclarations, id)
	o.busyMu.Unlock()
	o.touchActivity()
}

// IdleTimeout returns the owner-specific idle timeout override from
// x-mux.idleTimeout capability, or 0 if not declared (caller uses default).
func (o *Owner) IdleTimeout() time.Duration {
	return time.Duration(o.idleTimeoutNs.Load())
}

// SetIdleTimeout stores the x-mux.idleTimeout override. Called by the
// cache-init path after parsing the upstream initializeResult.
func (o *Owner) SetIdleTimeout(d time.Duration) {
	if d < 0 {
		return
	}
	o.idleTimeoutNs.Store(int64(d))
}

// Status returns a JSON-serializable status summary.
func (o *Owner) Status() map[string]any {
	o.mu.RLock()
	sessionIDs := make([]int, 0, len(o.sessions))
	for id := range o.sessions {
		sessionIDs = append(sessionIDs, id)
	}
	classification := string(o.autoClassification)
	classificationSource := o.classificationSource
	reason := o.classificationReason
	hasCachedInit := o.initResp != nil
	hasCachedTools := o.toolList != nil
	hasCachedPrompts := o.promptList != nil
	hasCachedResources := o.resourceList != nil
	cwds := make([]string, 0, len(o.cwdSet))
	for c := range o.cwdSet {
		cwds = append(cwds, c)
	}
	primaryCwd := o.cwd
	o.mu.RUnlock()

	status := map[string]any{
		"mux_version": Version,
		"upstream_pid": func() int {
			if o.upstream != nil {
				return o.upstream.PID()
			}
			return 0
		}(),
		"ipc_path":         o.ipcPath,
		"command":          o.command,
		"args":             o.args,
		"cwd":              primaryCwd,
		"cwd_set":          cwds,
		"sessions":         sessionIDs,
		"session_count":    len(sessionIDs),
		"pending_requests": o.pendingRequests.Load(),
		"uptime_seconds":   time.Since(o.startTime).Seconds(),
		"cached_init":      hasCachedInit,
		"cached_tools":     hasCachedTools,
		"cached_prompts":   hasCachedPrompts,
		"cached_resources": hasCachedResources,
	}

	if classification != "" {
		status["auto_classification"] = classification
		status["classification_source"] = classificationSource
		if len(reason) > 0 {
			status["classification_reason"] = reason
		}
	}

	// Include inflight request details when requests are pending
	var inflight []map[string]any
	var oldestMs int64
	o.inflightTracker.Range(func(key, value any) bool {
		req := value.(*InflightRequest)
		inflight = append(inflight, map[string]any{
			"method":          req.Method,
			"tool":            req.Tool,
			"session":         req.SessionID,
			"started_at":      req.StartTime.UTC().Format(time.RFC3339Nano),
			"elapsed_seconds": time.Since(req.StartTime).Seconds(),
		})
		age := time.Since(req.StartTime).Milliseconds()
		if age > oldestMs {
			oldestMs = age
		}
		return true
	})
	if len(inflight) > 0 {
		status["inflight"] = inflight
	}
	if oldestMs > 0 {
		status["oldest_request_age_ms"] = oldestMs
	}

	return status
}

// ExportSnapshot captures the owner's serializable state for graceful restart.
// Thread-safe: acquires RLock. Cached responses are base64-encoded.
func (o *Owner) ExportSnapshot() OwnerSnapshot {
	o.mu.RLock()
	cwds := make([]string, 0, len(o.cwdSet))
	for c := range o.cwdSet {
		cwds = append(cwds, c)
	}
	snap := OwnerSnapshot{
		Command:              o.command,
		Args:                 o.args,
		Cwd:                  o.cwd,
		CwdSet:               cwds,
		Classification:       o.autoClassification,
		ClassificationSource: o.classificationSource,
		ClassificationReason: o.classificationReason,
	}
	if o.initResp != nil {
		snap.CachedInit = base64Encode(o.initResp)
	}
	if o.toolList != nil {
		snap.CachedTools = base64Encode(o.toolList)
	}
	if o.promptList != nil {
		snap.CachedPrompts = base64Encode(o.promptList)
	}
	if o.resourceList != nil {
		snap.CachedResources = base64Encode(o.resourceList)
	}
	if o.resourceTemplateList != nil {
		snap.CachedResourceTemplates = base64Encode(o.resourceTemplateList)
	}
	o.mu.RUnlock()
	return snap
}

// ExportSessions returns snapshot metadata for all active sessions.
func (o *Owner) ExportSessions() []SessionSnapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	sessions := make([]SessionSnapshot, 0, len(o.sessions))
	for _, s := range o.sessions {
		sessions = append(sessions, SessionSnapshot{
			MuxSessionID: s.MuxSessionID,
			Cwd:          s.Cwd,
			Env:          s.Env,
		})
	}
	return sessions
}

// getCachedResponse returns the cached response for the given method, or nil.
func (o *Owner) getCachedResponse(method string) []byte {
	o.mu.RLock()
	defer o.mu.RUnlock()

	switch method {
	case "initialize":
		return o.initResp
	case "tools/list":
		return o.toolList
	case "prompts/list":
		return o.promptList
	case "resources/list":
		return o.resourceList
	case "resources/templates/list":
		return o.resourceTemplateList
	default:
		return nil
	}
}

// replayFromCache sends a cached response to the session with the client's request ID.
func (o *Owner) replayFromCache(s *Session, msg *jsonrpc.Message, cached []byte) error {
	replaced, err := jsonrpc.ReplaceID(cached, msg.ID)
	if err != nil {
		return fmt.Errorf("replay %s: replace id: %w", msg.Method, err)
	}

	// Track sessions that received a cached initialize so we can suppress
	// the subsequent notifications/initialized they send to upstream.
	if msg.Method == "initialize" {
		o.mu.Lock()
		o.cachedInitSessions[s.ID] = true
		o.mu.Unlock()
	}

	o.logger.Printf("session %d: replaying cached %s response", s.ID, msg.Method)
	return s.WriteRaw(replaced)
}

// cacheResponse stores a raw JSON-RPC response for later replay.
func (o *Owner) cacheResponse(method string, raw []byte) {
	cached := make([]byte, len(raw))
	copy(cached, raw)

	o.mu.Lock()
	switch method {
	case "initialize":
		o.initResp = cached
		o.initDone = true
		if injected, ok := listchanged.InjectInitializeCapability(cached); ok {
			o.initResp = injected
			o.logger.Printf("injected listChanged:true into cached initialize response")
		}
	case "tools/list":
		o.toolList = cached
	case "prompts/list":
		o.promptList = cached
	case "resources/list":
		o.resourceList = cached
	case "resources/templates/list":
		o.resourceTemplateList = cached
	}
	o.mu.Unlock()

	o.logger.Printf("cached %s response (%d bytes)", method, len(cached))

	if method == "initialize" {
		o.initReadyOnce.Do(func() {
			if o.initReady != nil {
				close(o.initReady)
			}
		})
		o.classifyFromCapabilities(cached)
		o.checkPersistent(cached)
		o.parseDrainTimeout(cached)
		o.parseToolTimeout(cached)
		o.parseIdleTimeout(cached)
		if sec := classify.ParseProgressInterval(cached); sec > 0 {
			o.progressIntervalNs.Store(int64(time.Duration(sec) * time.Second))
			o.logger.Printf("using x-mux.progressInterval: %ds", sec)
		}
	}
	if method == "tools/list" {
		o.classifyFromToolList(cached)
		// Notify daemon that a complete template is available (init + tools cached).
		// Daemon stores this as a template for instant future isolated spawns.
		if o.onCacheReady != nil && o.initDone {
			go o.onCacheReady(o.serverID)
		}
	}
}

// forwardCancelledNotification remaps the requestId in a notifications/cancelled
// notification so upstream can match it to the in-flight remapped request.
func (o *Owner) forwardCancelledNotification(s *Session, msg *jsonrpc.Message) error {
	// Parse the raw notification into a generic map
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(msg.Raw, &obj); err != nil {
		// Fallback: forward as-is
		return o.writeUpstream(msg.Raw)
	}

	paramsRaw, hasParams := obj["params"]
	if !hasParams {
		return o.writeUpstream(msg.Raw)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return o.writeUpstream(msg.Raw)
	}

	requestIDRaw, hasRequestID := params["requestId"]
	if !hasRequestID {
		return o.writeUpstream(msg.Raw)
	}

	// Remap the requestId to the upstream-facing ID
	remappedRequestID := remap.Remap(s.ID, requestIDRaw)
	params["requestId"] = remappedRequestID

	newParams, err := json.Marshal(params)
	if err != nil {
		return o.writeUpstream(msg.Raw)
	}

	obj["params"] = newParams
	remapped, err := json.Marshal(obj)
	if err != nil {
		return o.writeUpstream(msg.Raw)
	}

	// Clean up progress tokens for the cancelled request (FIX 1).
	o.clearProgressTokensForRequest(string(remappedRequestID))

	o.logger.Printf("session %d: forwarding notifications/cancelled with remapped requestId", s.ID)
	return o.writeUpstream(remapped)
}

// invalidateCacheIfNeeded checks if a notification from upstream signals that a
// cached list has changed, and clears the relevant cache entry.
func (o *Owner) invalidateCacheIfNeeded(data []byte) {
	msg, err := jsonrpc.Parse(data)
	if err != nil || !msg.IsNotification() {
		return
	}
	o.mu.Lock()
	switch msg.Method {
	case "notifications/tools/list_changed":
		o.toolList = nil
		o.logger.Printf("cache invalidated: tools/list")
	case "notifications/prompts/list_changed":
		o.promptList = nil
		o.logger.Printf("cache invalidated: prompts/list")
	case "notifications/resources/list_changed":
		o.resourceList = nil
		o.resourceTemplateList = nil
		o.logger.Printf("cache invalidated: resources/list + templates")
	}
	o.mu.Unlock()
}

// captureInitFingerprint extracts protocolVersion from an initialize request
// and stores it for fingerprint matching against later clients.
func (o *Owner) captureInitFingerprint(raw []byte) {
	var req struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.Params.ProtocolVersion == "" {
		return
	}

	o.mu.Lock()
	if o.initProtocolVersion == "" {
		o.initProtocolVersion = req.Params.ProtocolVersion
		o.logger.Printf("captured init fingerprint: protocolVersion=%s", req.Params.ProtocolVersion)
	}
	o.mu.Unlock()
}

// initFingerprintMatches checks if a new initialize request has the same
// protocolVersion as the cached one. If no fingerprint was captured, matches by default.
func (o *Owner) initFingerprintMatches(raw []byte) bool {
	o.mu.RLock()
	cached := o.initProtocolVersion
	o.mu.RUnlock()

	if cached == "" {
		return true // no fingerprint captured — allow replay
	}

	var req struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return true // can't parse — allow replay
	}

	return req.Params.ProtocolVersion == cached
}

// classifyFromCapabilities extracts x-mux capability from the cached initialize
// response and sets the classification. Has priority over tool-name classification.
// Skipped if already classified (snapshot restore, duplicate proactive init).
func (o *Owner) classifyFromCapabilities(initJSON []byte) {
	o.mu.RLock()
	alreadyClassified := o.classificationSource != ""
	o.mu.RUnlock()
	if alreadyClassified {
		return
	}

	mode, ok := classify.ClassifyCapabilities(initJSON)
	if !ok {
		return // no x-mux capability — will fall back to tool classification
	}

	o.mu.Lock()
	o.autoClassification = mode
	o.classificationSource = "capability"
	o.classificationReason = nil
	o.mu.Unlock()

	o.classifiedOnce.Do(func() {
		if o.classified != nil {
			close(o.classified)
		}
	})
	o.logger.Printf("x-mux capability: %s", mode)

	if mode == classify.ModeIsolated {
		o.mu.Lock()
		primaryCwd, cwdCount := o.resetCwdSetToPrimary()
		o.mu.Unlock()

		if primaryCwd != "" {
			o.logger.Printf("reset cwd_set to primary cwd: %s (now %d roots)", primaryCwd, cwdCount)
		}
		o.logger.Printf("closing IPC listener — server declares isolated via x-mux")
		o.closeListener()
		o.evictExtraSessions()
	}
}

// classifyFromToolList runs the auto-classifier on a cached tools/list response.
// Skipped if classification already determined (capability has priority, snapshot
// restores classification, duplicate proactive init responses are ignored).
func (o *Owner) classifyFromToolList(toolsJSON []byte) {
	o.mu.RLock()
	alreadyClassified := o.classificationSource != ""
	o.mu.RUnlock()
	if alreadyClassified {
		o.logger.Printf("skipping tool classification — already classified via %s", o.classificationSource)
		return
	}

	mode, matched := classify.ClassifyTools(toolsJSON)

	o.mu.Lock()
	o.autoClassification = mode
	o.classificationSource = "tools"
	o.classificationReason = matched
	o.mu.Unlock()

	o.classifiedOnce.Do(func() {
		if o.classified != nil {
			close(o.classified)
		}
	})

	if mode == classify.ModeIsolated {
		o.mu.Lock()
		primaryCwd, cwdCount := o.resetCwdSetToPrimary()
		o.mu.Unlock()

		o.logger.Printf("auto-classification: ISOLATED (matched: %v)", matched)
		if primaryCwd != "" {
			o.logger.Printf("reset cwd_set to primary cwd: %s (now %d roots)", primaryCwd, cwdCount)
		}
		o.logger.Printf("closing IPC listener — server requires per-session isolation")
		o.closeListener()
		// Disconnect extra sessions that were dedup'd before classification.
		// Keep only the first session — others will reconnect and get their own owner.
		o.evictExtraSessions()
	} else {
		o.logger.Printf("auto-classification: SHARED")
	}
}

// checkPersistent checks if the upstream declares x-mux.persistent: true
// and notifies the daemon via callback.
func (o *Owner) checkPersistent(initJSON []byte) {
	if !classify.ParsePersistent(initJSON) {
		return
	}
	o.logger.Printf("x-mux capability: persistent=true")
	if o.onPersistentDetected != nil {
		o.onPersistentDetected(o.serverID)
	}
}

// parseToolTimeout extracts x-mux.toolTimeout from the init response
// and stores it atomically for use during tools/call watchdog.
func (o *Owner) parseToolTimeout(initJSON []byte) {
	seconds := classify.ParseToolTimeout(initJSON)
	if seconds > 0 {
		timeout := time.Duration(seconds) * time.Second
		o.toolTimeoutNs.Store(int64(timeout))
		o.logger.Printf("x-mux capability: toolTimeout=%ds", seconds)
	}
}

// parseIdleTimeout extracts x-mux.idleTimeout from the init response
// and stores it atomically for use by the daemon reaper. Overrides
// the daemon-wide default (MCP_MUX_OWNER_IDLE or 10m).
func (o *Owner) parseIdleTimeout(initJSON []byte) {
	seconds := classify.ParseIdleTimeout(initJSON)
	if seconds > 0 {
		timeout := time.Duration(seconds) * time.Second
		o.SetIdleTimeout(timeout)
		o.logger.Printf("x-mux capability: idleTimeout=%ds", seconds)
	}
}

// startToolWatchdog launches a goroutine that fires after toolTimeout.
// If the inflight request is still tracked when the watchdog fires, it
// synthesizes a JSON-RPC error response and sends it to the session,
// preventing eternal hangs when upstream deadlocks during a tool call.
//
// Race design (prevents double response delivery):
//   - The watchdog and handleUpstreamMessage both call LoadAndDelete on
//     inflightTracker. Whichever wins delivers the response.
//   - Watchdog wins → records ID in timedOutIDs → handleUpstreamMessage
//     checks timedOutIDs and drops the late response.
//   - handleUpstreamMessage wins → watchdog's LoadAndDelete misses → exits.
//
// Uses time.NewTimer (with defer Stop) instead of time.After to avoid
// goroutine leaks when the tool call completes naturally before timeout.
//
// Only fires for tools/call requests — other methods (initialize, tools/list)
// have their own timing semantics (cached replays are instant).
func (o *Owner) startToolWatchdog(remappedID string, originalID json.RawMessage, s *Session, method string) {
	timeoutNs := o.toolTimeoutNs.Load()
	if timeoutNs <= 0 {
		return
	}
	timeout := time.Duration(timeoutNs)
	// Copy originalID to avoid aliasing the caller's buffer after this goroutine returns.
	origIDCopy := make(json.RawMessage, len(originalID))
	copy(origIDCopy, originalID)

	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-o.done:
			return
		}
		// Try to claim the inflight. If another path already claimed it, exit.
		if _, ok := o.inflightTracker.LoadAndDelete(remappedID); !ok {
			return
		}
		// Mark this ID as timed-out so handleUpstreamMessage drops any
		// late upstream response (prevents duplicate delivery to session).
		o.timedOutIDs.Store(remappedID, struct{}{})
		o.methodTags.Delete(remappedID)
		o.sessionMgr.CompleteRequest(remappedID)
		o.pendingRequests.Add(-1)
		errResp := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"mux: %s timed out after %s (upstream did not respond)"}}`,
			string(origIDCopy), method, timeout,
		)
		if err := s.WriteRaw([]byte(errResp)); err != nil {
			o.logger.Printf("watchdog: session %d write error: %v", s.ID, err)
			return
		}
		o.logger.Printf("watchdog: %s (id=%s) timed out after %s, sent error to session %d",
			method, remappedID, timeout, s.ID)
	}()
}

// parseDrainTimeout extracts x-mux.drainTimeout from the init response
// and stores it for use during graceful shutdown.
func (o *Owner) parseDrainTimeout(initJSON []byte) {
	seconds := classify.ParseDrainTimeout(initJSON)
	if seconds > 0 {
		o.drainTimeout = time.Duration(seconds) * time.Second
		o.logger.Printf("x-mux capability: drainTimeout=%ds", seconds)
		// Propagate to upstream process so Close() uses the declared timeout
		o.mu.RLock()
		up := o.upstream
		o.mu.RUnlock()
		if up != nil {
			up.SetDrainTimeout(o.drainTimeout)
		}
	}
}

// DrainTimeout returns the upstream's declared drain timeout, or 0 if not declared.
func (o *Owner) DrainTimeout() time.Duration {
	return o.drainTimeout
}
