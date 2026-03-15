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
	listener net.Listener
	logger   *log.Logger

	mu       sync.RWMutex
	sessions map[int]*Session
	initDone bool
	initResp []byte // cached InitializeResult for replaying to new sessions
	toolList []byte // cached tools/list response

	pendingRequests atomic.Int64
	startTime       time.Time
	controlServer   *control.Server

	done chan struct{}
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
		listener:  ln,
		logger:    logger,
		sessions:  make(map[int]*Session),
		startTime: time.Now(),
		done:      make(chan struct{}),
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
		// Forward notifications as-is to upstream
		return o.upstream.WriteLine(msg.Raw)

	case msg.IsRequest():
		// Track pending requests
		o.pendingRequests.Add(1)

		// Remap ID and forward to upstream
		newID := remap.Remap(s.ID, msg.ID)
		remapped, err := jsonrpc.ReplaceID(msg.Raw, newID)
		if err != nil {
			o.pendingRequests.Add(-1) // undo increment on error
			return fmt.Errorf("remap request: %w", err)
		}
		return o.upstream.WriteLine(remapped)

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
	default:
		// Unknown server→client request — log and ignore
		o.logger.Printf("upstream sent unhandled request: %s (ignoring)", msg.Method)
		return nil
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
				return // shutdown
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

// Shutdown stops the owner, closing all sessions and the upstream.
func (o *Owner) Shutdown() {
	select {
	case <-o.done:
		return // already shut down
	default:
	}
	close(o.done)

	// Close control server first and clean up its socket
	if o.controlServer != nil {
		socketPath := o.controlServer.SocketPath()
		o.controlServer.Close()
		ipc.Cleanup(socketPath)
	}

	o.listener.Close()
	ipc.Cleanup(o.ipcPath)

	o.mu.Lock()
	for _, s := range o.sessions {
		s.Close()
	}
	o.sessions = make(map[int]*Session)
	o.mu.Unlock()

	o.upstream.Close()

	o.logger.Printf("owner shut down")
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
	o.mu.RUnlock()

	return map[string]any{
		"upstream_pid":     o.upstream.PID(),
		"ipc_path":         o.ipcPath,
		"sessions":         sessionIDs,
		"session_count":    len(sessionIDs),
		"pending_requests": o.pendingRequests.Load(),
		"uptime_seconds":   time.Since(o.startTime).Seconds(),
	}
}

