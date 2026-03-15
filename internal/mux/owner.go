package mux

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

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
	listener net.Listener
	logger   *log.Logger

	mu       sync.RWMutex
	sessions map[int]*Session
	initDone bool
	initResp []byte // cached InitializeResult for replaying to new sessions
	toolList []byte // cached tools/list response

	done chan struct{}
}

// OwnerConfig holds parameters for creating an Owner.
type OwnerConfig struct {
	// Command and Args for spawning the upstream MCP server.
	Command string
	Args    []string
	Env     map[string]string

	// IPCPath for the Unix domain socket listener.
	IPCPath string

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

	// Spawn upstream
	proc, err := upstream.Start(cfg.Command, cfg.Args, cfg.Env)
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
		upstream: proc,
		ipcPath:  cfg.IPCPath,
		listener: ln,
		logger:   logger,
		sessions: make(map[int]*Session),
		done:     make(chan struct{}),
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
		// Remap ID and forward to upstream
		newID := remap.Remap(s.ID, msg.ID)
		remapped, err := jsonrpc.ReplaceID(msg.Raw, newID)
		if err != nil {
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
func (o *Owner) handleUpstreamMessage(msg *jsonrpc.Message) error {
	if msg.IsNotification() {
		// Broadcast to all sessions
		return o.broadcast(msg.Raw)
	}

	if !msg.IsResponse() {
		return fmt.Errorf("unexpected non-response from upstream: %s", msg.Type)
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

// Shutdown stops the owner, closing all sessions and the upstream.
func (o *Owner) Shutdown() {
	select {
	case <-o.done:
		return // already shut down
	default:
	}
	close(o.done)

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

// Status returns a JSON-serializable status summary.
func (o *Owner) Status() map[string]interface{} {
	o.mu.RLock()
	sessionIDs := make([]int, 0, len(o.sessions))
	for id := range o.sessions {
		sessionIDs = append(sessionIDs, id)
	}
	o.mu.RUnlock()

	return map[string]interface{}{
		"upstream_pid": o.upstream.PID(),
		"ipc_path":     o.ipcPath,
		"sessions":     sessionIDs,
		"session_count": len(sessionIDs),
	}
}

// Ensure json import is used
var _ = json.RawMessage{}
