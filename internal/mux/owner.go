package mux

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitswan-space/mcp-mux/internal/classify"
	"github.com/bitswan-space/mcp-mux/internal/control"
	"github.com/bitswan-space/mcp-mux/internal/ipc"
	"github.com/bitswan-space/mcp-mux/internal/jsonrpc"
	"github.com/bitswan-space/mcp-mux/internal/remap"
	"github.com/bitswan-space/mcp-mux/internal/upstream"
)

// Owner is the multiplexer core. It manages a single upstream process and
// routes requests from multiple downstream sessions through it.
type Owner struct {
	upstream *upstream.Process
	ipcPath  string
	cwd      string // working directory for this owner instance
	command  string // upstream command (for status/restart)
	args     []string // upstream args (for status/restart)
	listener net.Listener
	logger   *log.Logger

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

	lastActiveSessionID int // ID of the session that last sent a request to upstream

	methodTags      sync.Map // remapped request ID (string) -> method name
	pendingRequests atomic.Int64
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

	// Logger for debug output. Uses log.Default() if nil.
	Logger *log.Logger
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
		upstream:  proc,
		ipcPath:   cfg.IPCPath,
		cwd:       cfg.Cwd,
		command:   cfg.Command,
		args:      cfg.Args,
		listener:  ln,
		logger:    logger,
		sessions:           make(map[int]*Session),
		cachedInitSessions: make(map[int]bool),
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

	// Start accepting IPC connections
	go o.acceptLoop()

	// Monitor upstream exit
	go func() {
		<-proc.Done
		logger.Printf("upstream exited: %v", proc.ExitErr)
		o.Shutdown()
	}()

	return o, nil
}

// AddSession registers a new downstream session and starts routing its messages.
// This is used for the owner's own stdio session (first client).
func (o *Owner) AddSession(s *Session) {
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()

	o.logger.Printf("session %d connected", s.ID)

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
		return o.upstream.WriteLine(msg.Raw)

	case msg.IsRequest():
		// Replay from cache if available (avoids upstream round-trip)
		if cached := o.getCachedResponse(msg.Method); cached != nil {
			// For initialize: verify protocolVersion matches before replaying
			if msg.Method == "initialize" && !o.initFingerprintMatches(msg.Raw) {
				o.logger.Printf("session %d: initialize fingerprint mismatch, forwarding to upstream", s.ID)
				// Fall through to forward to upstream
			} else {
				return o.replayFromCache(s, msg, cached)
			}
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

		// Inject _meta.muxSessionId for session-aware servers
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
		}

		// Record the active session so server→client requests can be routed back
		o.mu.Lock()
		o.lastActiveSessionID = s.ID
		o.mu.Unlock()

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

// readUpstream reads responses from the upstream and routes them to the correct session.
func (o *Owner) readUpstream() {
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
		// Broadcast to all sessions
		return o.broadcast(msg.Raw)
	}

	if msg.IsRequest() {
		// Server→client request (e.g., roots/list, sampling/createMessage)
		return o.handleUpstreamRequest(msg)
	}

	if !msg.IsResponse() {
		return fmt.Errorf("unexpected message type from upstream: %s", msg.Type)
	}

	// Decrement pending counter before routing (handles disconnected sessions too)
	o.pendingRequests.Add(-1)

	// Cache response if this was a tagged cacheable request
	if methodRaw, ok := o.methodTags.LoadAndDelete(string(msg.ID)); ok {
		o.cacheResponse(methodRaw.(string), msg.Raw)
	}

	// Deremap the ID to find the target session
	result, err := remap.Deremap(msg.ID)
	if err != nil {
		return fmt.Errorf("deremap response id: %w", err)
	}

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

	return session.WriteRaw(restored)
}

// handleUpstreamRequest handles server→client requests from the upstream.
// The most important one is roots/list — the server asks what filesystem
// boundaries are available. We respond with the owner's cwd as a file:// URI.
func (o *Owner) handleUpstreamRequest(msg *jsonrpc.Message) error {
	switch msg.Method {
	case "roots/list":
		o.logger.Printf("upstream requested roots/list, responding with cwd: %s", o.cwd)
		return o.respondToRootsList(msg.ID)
	case "ping":
		// Respond locally with empty result — no client involvement needed
		o.logger.Printf("upstream sent ping, responding locally")
		return o.respondToPing(msg.ID)
	default:
		// Route to the last active session (e.g., sampling/createMessage, elicitation/create)
		return o.routeToLastActiveSession(msg)
	}
}

// respondToRootsList sends a roots/list response to the upstream with the owner's cwd.
func (o *Owner) respondToRootsList(id json.RawMessage) error {
	cwd := o.cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// Convert to file:// URI
	uri := pathToFileURI(cwd)

	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Roots []struct {
				URI  string `json:"uri"`
				Name string `json:"name,omitempty"`
			} `json:"roots"`
		} `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
	}
	resp.Result.Roots = []struct {
		URI  string `json:"uri"`
		Name string `json:"name,omitempty"`
	}{
		{URI: uri, Name: filepath.Base(cwd)},
	}

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

// routeToLastActiveSession forwards a server→client request to the last session
// that sent a request upstream. Used for sampling/createMessage, elicitation/create, etc.
func (o *Owner) routeToLastActiveSession(msg *jsonrpc.Message) error {
	o.mu.RLock()
	sessionID := o.lastActiveSessionID
	session, ok := o.sessions[sessionID]
	o.mu.RUnlock()

	if !ok {
		o.logger.Printf("no active session for server request %s, returning error", msg.Method)
		if msg.Method == "elicitation/create" {
			return o.respondToElicitationCancel(msg.ID)
		}
		return o.respondWithError(msg.ID, -32603, "no active session available")
	}

	o.logger.Printf("routing server request %s to session %d", msg.Method, sessionID)
	return session.WriteRaw(msg.Raw)
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

// sendRootsListChanged notifies the upstream that roots have changed.
func (o *Owner) sendRootsListChanged() {
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

	var firstErr error
	for _, s := range sessions {
		if err := s.WriteRaw(data); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			o.logger.Printf("broadcast to session %d error: %v", s.ID, err)
		}
	}
	return firstErr
}

// removeSession removes a session from the owner.
func (o *Owner) removeSession(s *Session) {
	o.mu.Lock()
	delete(o.sessions, s.ID)
	remaining := len(o.sessions)
	o.mu.Unlock()

	o.logger.Printf("session %d disconnected (%d remaining)", s.ID, remaining)

	// Notify upstream that roots may have changed (client left)
	if remaining > 0 {
		o.sendRootsListChanged()
	}
}

// acceptLoop accepts new IPC connections and creates sessions for them.
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
				o.logger.Printf("accept error: %v", err)
				continue
			}
		}

		s := NewSession(conn, conn)
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
		// Close IPC listener to stop new connections
		o.listener.Close()

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

// closeListener stops the IPC listener and removes the socket file.
// Safe to call multiple times (uses sync.Once).
func (o *Owner) closeListener() {
	o.closeListenerOnce.Do(func() {
		close(o.listenerDone)
		o.listener.Close()
		ipc.Cleanup(o.ipcPath)
	})
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

		o.upstream.Close()

		o.logger.Printf("owner shut down")

		// Signal done AFTER cleanup, so main goroutine doesn't exit early
		close(o.done)
	})
}

// Done returns a channel closed when the owner has shut down.
func (o *Owner) Done() <-chan struct{} {
	return o.done
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
	o.mu.RUnlock()

	status := map[string]any{
		"upstream_pid":      o.upstream.PID(),
		"ipc_path":          o.ipcPath,
		"command":           o.command,
		"args":              o.args,
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

	return status
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
		o.classifyFromCapabilities(cached)
	}
	if method == "tools/list" {
		o.classifyFromToolList(cached)
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
func (o *Owner) classifyFromCapabilities(initJSON []byte) {
	mode, ok := classify.ClassifyCapabilities(initJSON)
	if !ok {
		return // no x-mux capability — will fall back to tool classification
	}

	o.mu.Lock()
	o.autoClassification = mode
	o.classificationSource = "capability"
	o.classificationReason = nil
	o.mu.Unlock()

	o.logger.Printf("x-mux capability: %s", mode)

	if mode == classify.ModeIsolated {
		o.logger.Printf("closing IPC listener — server declares isolated via x-mux")
		o.closeListener()
	}
}

// classifyFromToolList runs the auto-classifier on a cached tools/list response.
// Skipped if x-mux capability already provided classification (capability has priority).
func (o *Owner) classifyFromToolList(toolsJSON []byte) {
	o.mu.RLock()
	alreadyClassified := o.classificationSource == "capability"
	o.mu.RUnlock()
	if alreadyClassified {
		o.logger.Printf("skipping tool classification — x-mux capability takes priority")
		return
	}

	mode, matched := classify.ClassifyTools(toolsJSON)

	o.mu.Lock()
	o.autoClassification = mode
	o.classificationSource = "tools"
	o.classificationReason = matched
	o.mu.Unlock()

	if mode == classify.ModeIsolated {
		o.logger.Printf("auto-classification: ISOLATED (matched: %v)", matched)
		o.logger.Printf("closing IPC listener — server requires per-session isolation")
		o.closeListener()
	} else {
		o.logger.Printf("auto-classification: SHARED")
	}
}

