package control

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/thebtf/mcp-mux/internal/ipc"
)

// Server listens on a Unix domain socket for control commands.
// Each connection handles one Request → Response, then closes.
type Server struct {
	listener net.Listener
	handler  CommandHandler
	logger   *log.Logger

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// NewServer creates and starts a control server at the given socket path.
func NewServer(socketPath string, handler CommandHandler, logger *log.Logger) (*Server, error) {
	ln, err := ipc.Listen(socketPath)
	if err != nil {
		return nil, fmt.Errorf("control: listen %s: %w", socketPath, err)
	}

	s := &Server{
		listener: ln,
		handler:  handler,
		logger:   logger,
		done:     make(chan struct{}),
	}

	go s.acceptLoop()

	return s, nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			s.logger.Printf("control: accept error: %v", err)
			continue
		}

		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	dec := json.NewDecoder(conn)
	var req Request
	if err := dec.Decode(&req); err != nil {
		s.writeResponse(conn, Response{OK: false, Message: fmt.Sprintf("invalid request: %v", err)})
		return
	}

	resp := s.dispatch(req)
	s.writeResponse(conn, resp)
}

func (s *Server) dispatch(req Request) Response {
	switch req.Cmd {
	case "ping":
		return Response{OK: true, Message: "pong"}

	case "shutdown":
		msg := s.handler.HandleShutdown(req.DrainTimeoutMs)
		return Response{OK: true, Message: msg}

	case "status":
		status := s.handler.HandleStatus()
		data, err := json.Marshal(status)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("marshal status: %v", err)}
		}
		return Response{OK: true, Data: data}

	case "spawn":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "spawn not supported (not a daemon)"}
		}
		ipcPath, serverID, err := dh.HandleSpawn(req)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("spawn: %v", err)}
		}
		return Response{OK: true, Message: "spawned", IPCPath: ipcPath, ServerID: serverID}

	case "remove":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "remove not supported (not a daemon)"}
		}
		if err := dh.HandleRemove(req.Command); err != nil {
			return Response{OK: false, Message: fmt.Sprintf("remove: %v", err)}
		}
		return Response{OK: true, Message: "removed"}

	default:
		return Response{OK: false, Message: fmt.Sprintf("unknown command: %s", req.Cmd)}
	}
}

func (s *Server) writeResponse(conn net.Conn, resp Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		s.logger.Printf("control: marshal response: %v", err)
		return
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		s.logger.Printf("control: write response: %v", err)
	}
}

// Close stops the control server and removes the socket file.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.listener.Close()
}

// SocketPath returns the socket path from the underlying listener.
func (s *Server) SocketPath() string {
	return s.listener.Addr().String()
}
