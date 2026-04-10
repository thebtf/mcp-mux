package mux

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

	"github.com/thebtf/mcp-mux/internal/classify"
	"github.com/thebtf/mcp-mux/internal/control"
	"github.com/thebtf/mcp-mux/internal/ipc"
	"github.com/thebtf/mcp-mux/internal/jsonrpc"
	"github.com/thebtf/mcp-mux/internal/remap"
	"github.com/thebtf/mcp-mux/internal/serverid"
	"github.com/thebtf/mcp-mux/internal/upstream"
	"github.com/thejerf/suture/v4"
)

// Version is the mcp-mux build version, included in status output.
// Auto-detected from Go build info (vcs.revision + vcs.modified).
// Override at build time via: -ldflags "-X github.com/thebtf/mcp-mux/internal/mux.Version=..."
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
	cwd      string   // primary working directory (from first spawn)
	cwdSet   map[string]bool // all known cwds (for multi-project roots/list)
	command  string   // upstream command (for status/restart)
	args     []string // upstream args (for status/restart)
	serverID string   // server identity hash
	listener net.Listener
	logger   *log.Logger

	onZeroSessions       func(serverID string)
	onUpstreamExit       func(serverID string)
	onPersistentDetected func(serverID string)
	onCacheReady         func(serverID string)

	mu                   sync.RWMutex
	sessions             map[int]*Session
	cachedInitSessions   map[int]bool     // sessions that received a cached (replayed) initialize response
	initDone             bool
	initResp             []byte          // cached initialize response (raw JSON-RPC)
	initProtocolVersion  string          // protocolVersion from first initialize request (for fingerprint matching)
	toolList             []byte          // cached tools/list response (raw JSON-RPC)
	promptList           []byte          // cached prompts/list response (raw JSON-RPC)
	resourceList         []byte          // cached resources/list response (raw JSON-RPC)
	resourceTemplateList []byte          // cached resources/templates/list response (raw JSON-RPC)
	autoClassification   classify.SharingMode
	classificationSource string   // "capability" or "tools" — what determined classification
	classificationReason []string // tool names that triggered isolation
	classified           chan struct{} // closed when autoClassification is first set
	classifiedOnce       sync.Once
	initReady            chan struct{} // closed when initialize response is cached or upstream dies
	initReadyOnce        sync.Once

	sessionMgr       *SessionManager
	tokenHandshake   bool           // true when daemon manages this owner (shims send token)
	progressOwners   map[string]int // progressToken → session ID for targeted routing

	upstreamDead    atomic.Bool // set when upstream exits; prevents sending to dead pipe
	methodTags      sync.Map // remapped request ID (string) -> method name
	inflightTracker sync.Map // remapped request ID (string) -> *InflightRequest
	pendingRequests atomic.Int64
	drainTimeout    time.Duration // from x-mux.drainTimeout capability; 0 = use default
	startTime       time.Time
	controlServer   *control.Server

	shutdownOnce      sync.Once
	closeListenerOnce sync.Once
	listenerDone      chan struct{} // closed when IPC listener is intentionally stopped
	done              chan struct{}
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
		ipcPath:              cfg.IPCPath,
		cwd:                  cfg.Cwd,
		cwdSet:               cwdSet,
		command:              cfg.Command,
		args:                 cfg.Args,
		serverID:             cfg.ServerID,
		listener:             ln,
		logger:               logger,
		onZeroSessions:       cfg.OnZeroSessions,
		onUpstreamExit:       cfg.OnUpstreamExit,
		onPersistentDetected: cfg.OnPersistentDetected,
		onCacheReady:         cfg.OnCacheReady,
		sessions:             make(map[int]*Session),
		cachedInitSessions:   make(map[int]bool),
		sessionMgr:           NewSessionManager(),
		tokenHandshake:       cfg.TokenHandshake,
		autoClassification:   snap.Classification,
		classificationSource: snap.ClassificationSource,
		classificationReason: snap.ClassificationReason,
		classified:           make(chan struct{}),
		initReady:            make(chan struct{}),
		progressOwners:       make(map[string]int),
		startTime:            time.Now(),
		listenerDone:         make(chan struct{}),
		done:                 make(chan struct{}),
	}

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

		proc, err := upstream.Start(o.command, o.args, nil, o.cwd)
		if err != nil {
			o.logger.Printf("background upstream spawn failed: %v (serving stale cache)", err)
			return
		}

		// Check again after spawn — owner may have shut down while process was starting
		select {
		case <-o.done:
			proc.Close()
			return
		default:
		}

		o.mu.Lock()
		o.upstream = proc
		o.mu.Unlock()

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
func NewOwner(cfg OwnerConfig) (*Owner, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	// Spawn upstream with the client's cwd
	proc, err := upstream.Start(cfg.Command, cfg.Args, cfg.Env, cfg.Cwd)
	if err != nil {
		return nil, fmt.Errorf("owner: start upstream: %w", err)
	}

	// Start IPC listener
	ln, err := ipc.Listen(cfg.IPCPath)
	if err != nil {
		proc.Close()
		return nil, fmt.Errorf("owner: listen %s: %w", cfg.IPCPath, err)
	}

	o := &Owner{
		upstream:       proc,
		ipcPath:        cfg.IPCPath,
		cwd:            cfg.Cwd,
		cwdSet:         map[string]bool{cfg.Cwd: true},
		command:        cfg.Command,
		args:           cfg.Args,
		serverID:       cfg.ServerID,
		listener:       ln,
		logger:         logger,
		onZeroSessions:       cfg.OnZeroSessions,
		onUpstreamExit:       cfg.OnUpstreamExit,
		onPersistentDetected: cfg.OnPersistentDetected,
		onCacheReady:         cfg.OnCacheReady,
		sessions:           make(map[int]*Session),
		cachedInitSessions: make(map[int]bool),
		sessionMgr:         NewSessionManager(),
		tokenHandshake:     cfg.TokenHandshake,
		classified:         make(chan struct{}),
		initReady:          make(chan struct{}),
		progressOwners:     make(map[string]int),
		startTime:          time.Now(),
		listenerDone:       make(chan struct{}),
		done:               make(chan struct{}),
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
	go o.sendProactiveInit()

	// Start accepting IPC connections
	go o.acceptLoop()

	// Monitor upstream exit
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
	o.logger.Printf("session %d connected (cwd: %q)", s.ID, s.Cwd)

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
		// Forward other notifications as-is to upstream
		if o.upstream == nil {
			return nil // snapshot owner — upstream not yet spawned, drop notification
		}
		return o.upstream.WriteLine(msg.Raw)

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

		// Fail fast if upstream is dead or not yet spawned (snapshot owner without cache hit)
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

		// Inject _meta.muxSessionId, _meta.muxCwd, and _meta.muxEnv for session-aware servers
		o.mu.RLock()
		isSessionAware := o.autoClassification == classify.ModeSessionAware
		o.mu.RUnlock()
		if isSessionAware {
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
		o.trackProgressToken(s.ID, msg.Raw)

		// Record the active session so server→client requests can be routed back
		o.sessionMgr.TrackRequest(string(newID), s.ID)

		return o.upstream.WriteLine(remapped)

	case msg.IsResponse():
		// Client is responding to a server→client request (e.g., sampling/createMessage).
		// Forward as-is — the ID belongs to the upstream's request, no remapping needed.
		o.logger.Printf("session %d: forwarding client response to upstream (id=%s)", s.ID, string(msg.ID))
		return o.upstream.WriteLine(msg.Raw)

	default:
		return fmt.Errorf("unexpected message type from downstream: %s", msg.Type)
	}
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

	if err := o.upstream.WriteLine([]byte(initReq)); err != nil {
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
		if err := o.upstream.WriteLine([]byte(notif)); err != nil {
			o.logger.Printf("proactive init: notifications/initialized failed: %v", err)
			return
		}

		// Request tools/list to pre-populate tool cache + trigger classification
		toolsReq := `{"jsonrpc":"2.0","id":"mux-init-1","method":"tools/list","params":{}}`
		o.methodTags.Store(`"mux-init-1"`, "tools/list")
		o.pendingRequests.Add(1)
		if err := o.upstream.WriteLine([]byte(toolsReq)); err != nil {
			o.logger.Printf("proactive init: tools/list failed: %v", err)
		}
	}()
}

// readUpstream reads responses from the upstream and routes them to the correct session.
func (o *Owner) readUpstream() {
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
		// Route progress notifications to owning session instead of broadcast
		if msg.Method == "notifications/progress" {
			if err := o.routeProgressNotification(msg.Raw); err == nil {
				return nil
			}
			// Fallback to broadcast if routing fails
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

	// Decrement pending counter and remove inflight tracking
	o.pendingRequests.Add(-1)
	o.inflightTracker.Delete(string(msg.ID))
	o.sessionMgr.CompleteRequest(string(msg.ID))

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

	return o.upstream.WriteLine(data)
}

// respondToPing sends an empty result to the upstream in response to a ping.
func (o *Owner) respondToPing(id json.RawMessage) error {
	resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(id))
	return o.upstream.WriteLine([]byte(resp))
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
	return o.upstream.WriteLine(data)
}

// respondWithError sends a JSON-RPC error response to upstream.
func (o *Owner) respondWithError(id json.RawMessage, code int, message string) error {
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
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal error response: %w", err)
	}
	return o.upstream.WriteLine(data)
}

// routeProgressNotification sends a notifications/progress to the session that
// owns the progressToken, instead of broadcasting to all sessions.
func (o *Owner) routeProgressNotification(raw []byte) error {
	var notif struct {
		Params struct {
			ProgressToken json.RawMessage `json:"progressToken"`
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

	return session.WriteRaw(raw)
}

// trackProgressToken extracts _meta.progressToken from a request and records
// which session owns it, enabling targeted progress notification routing.
func (o *Owner) trackProgressToken(sessionID int, raw []byte) {
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
	o.mu.Unlock()
}

// sendRootsListChanged notifies the upstream that roots have changed.
func (o *Owner) sendRootsListChanged() {
	if o.upstream == nil {
		return // snapshot owner — upstream not yet spawned
	}
	notification := `{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`
	if err := o.upstream.WriteLine([]byte(notification)); err != nil {
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

// removeSession removes a session from the owner.
func (o *Owner) removeSession(s *Session) {
	o.mu.Lock()
	delete(o.sessions, s.ID)
	remaining := len(o.sessions)
	o.mu.Unlock()

	o.sessionMgr.RemoveSession(s.ID)
	o.logger.Printf("session %d disconnected (%d remaining)", s.ID, remaining)

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
		if s.closer == nil {
			// io.MultiReader doesn't implement io.Closer — set closer to conn explicitly
			s.closer = conn
		}
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
	// Non-blocking upstream check first — avoids race with concurrent Shutdown
	select {
	case <-o.upstreamDeadCh():
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
	case <-o.upstreamDeadCh():
		return o.upstreamDeathResult()
	}
}

// upstreamDeathResult determines the Serve return value when upstream has died.
// Checks classification: isolated owners return ErrDoNotRestart (no auto-restart),
// others return an error to trigger supervisor backoff+restart.
func (o *Owner) upstreamDeathResult() error {
	o.mu.RLock()
	mode := o.autoClassification
	o.mu.RUnlock()
	if mode == classify.ModeIsolated {
		// Isolated servers should not be restarted by the supervisor.
		// Each CC session spawns its own dedicated isolated owner.
		return suture.ErrDoNotRestart
	}
	return fmt.Errorf("owner %s: upstream exited", o.serverID[:8])
}

// upstreamDeadCh returns a channel that closes when the upstream process dies.
// If the owner has no upstream (nil, e.g. snapshot-restored before
// SpawnUpstreamBackground or after a failed background spawn), returns the
// package-level closedChan so Serve observes it as failure. This prevents
// Serve from hanging forever and lets suture apply backoff/restart.
func (o *Owner) upstreamDeadCh() <-chan struct{} {
	o.mu.RLock()
	up := o.upstream
	o.mu.RUnlock()
	if up == nil {
		return closedChan
	}
	return up.Done
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
		"mux_version":      Version,
		"upstream_pid":      func() int { if o.upstream != nil { return o.upstream.PID() }; return 0 }(),
		"ipc_path":          o.ipcPath,
		"command":           o.command,
		"args":              o.args,
		"cwd":               primaryCwd,
		"cwd_set":           cwds,
		"sessions":          sessionIDs,
		"session_count":     len(sessionIDs),
		"pending_requests":  o.pendingRequests.Load(),
		"uptime_seconds":    time.Since(o.startTime).Seconds(),
		"cached_init":       hasCachedInit,
		"cached_tools":      hasCachedTools,
		"cached_prompts":    hasCachedPrompts,
		"cached_resources":  hasCachedResources,
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
	o.inflightTracker.Range(func(key, value any) bool {
		req := value.(*InflightRequest)
		inflight = append(inflight, map[string]any{
			"method":          req.Method,
			"tool":            req.Tool,
			"session":         req.SessionID,
			"started_at":      req.StartTime.UTC().Format(time.RFC3339Nano),
			"elapsed_seconds": time.Since(req.StartTime).Seconds(),
		})
		return true
	})
	if len(inflight) > 0 {
		status["inflight"] = inflight
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
		return o.upstream.WriteLine(msg.Raw)
	}

	paramsRaw, hasParams := obj["params"]
	if !hasParams {
		return o.upstream.WriteLine(msg.Raw)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return o.upstream.WriteLine(msg.Raw)
	}

	requestIDRaw, hasRequestID := params["requestId"]
	if !hasRequestID {
		return o.upstream.WriteLine(msg.Raw)
	}

	// Remap the requestId to the upstream-facing ID
	remappedRequestID := remap.Remap(s.ID, requestIDRaw)
	params["requestId"] = remappedRequestID

	newParams, err := json.Marshal(params)
	if err != nil {
		return o.upstream.WriteLine(msg.Raw)
	}

	obj["params"] = newParams
	remapped, err := json.Marshal(obj)
	if err != nil {
		return o.upstream.WriteLine(msg.Raw)
	}

	o.logger.Printf("session %d: forwarding notifications/cancelled with remapped requestId", s.ID)
	return o.upstream.WriteLine(remapped)
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
		o.logger.Printf("auto-classification: ISOLATED (matched: %v)", matched)
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

