package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
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
	wg     sync.WaitGroup
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

		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// Bound the read phase: a silent/malicious client cannot hold the goroutine
	// open in dec.Decode forever.
	if err := conn.SetReadDeadline(time.Now().Add(clientDeadline)); err != nil {
		s.logger.Printf("control: set read deadline: %v", err)
		return
	}

	dec := json.NewDecoder(conn)
	var req Request
	if err := dec.Decode(&req); err != nil {
		// Bound the write phase on the error path too — a non-reading client
		// must not block writeResponse indefinitely.
		_ = conn.SetWriteDeadline(time.Now().Add(clientDeadline))
		s.writeResponse(conn, Response{OK: false, Message: fmt.Sprintf("invalid request: %v", err)})
		return
	}

	// Dispatch runs the handler (e.g. graceful-restart may take longer than
	// clientDeadline). Set the write deadline only after dispatch returns so
	// it bounds only the I/O write, not handler execution.
	resp, afterFn := s.dispatch(req)
	if err := conn.SetWriteDeadline(time.Now().Add(clientDeadline)); err != nil {
		s.logger.Printf("control: set write deadline: %v", err)
		return
	}
	s.writeResponse(conn, resp)
	if afterFn != nil {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		io.Copy(io.Discard, conn)
		conn.Close()
		afterFn()
	}
}

func (s *Server) dispatch(req Request) (Response, func()) {
	switch req.Cmd {
	case "ping":
		return Response{OK: true, Message: "pong"}, nil

	case "shutdown":
		msg := s.handler.HandleShutdown(req.DrainTimeoutMs)
		return Response{OK: true, Message: msg}, nil

	case "status":
		status := s.handler.HandleStatus()
		data, err := json.Marshal(status)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("marshal status: %v", err)}, nil
		}
		return Response{OK: true, Data: data}, nil

	case "spawn":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "spawn not supported (not a daemon)"}, nil
		}
		ipcPath, serverID, token, err := dh.HandleSpawn(req)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("spawn: %v", err)}, nil
		}
		return Response{OK: true, Message: "spawned", IPCPath: ipcPath, ServerID: serverID, Token: token}, nil

	case "remove":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "remove not supported (not a daemon)"}, nil
		}
		if err := dh.HandleRemove(req.Command); err != nil {
			return Response{OK: false, Message: fmt.Sprintf("remove: %v", err)}, nil
		}
		return Response{OK: true, Message: "removed"}, nil

	case "graceful-restart":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "graceful-restart not supported (not a daemon)"}, nil
		}
		snapshotPath, afterFn, err := dh.HandleGracefulRestart(req.DrainTimeoutMs)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("graceful-restart: %v", err)}, nil
		}
		return Response{OK: true, Message: "snapshot written, shutting down", IPCPath: snapshotPath}, afterFn

	case "refresh-token":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "refresh-token not supported (not a daemon)"}, nil
		}
		newToken, err := dh.HandleRefreshSessionToken(req.PrevToken)
		if err != nil {
			switch {
			case matchesControlError(err, errUnknownToken):
				return Response{OK: false, Message: "unknown token"}, nil
			case matchesControlError(err, errOwnerGone):
				return Response{OK: false, Message: "owner gone"}, nil
			default:
				return Response{OK: false, Message: err.Error()}, nil
			}
		}
		return Response{OK: true, Token: newToken}, nil

	case "reconnect-give-up":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "reconnect-give-up not supported (not a daemon)"}, nil
		}
		if err := dh.HandleReconnectGiveUp(req.ReconnectReason); err != nil {
			return Response{OK: false, Message: err.Error()}, nil
		}
		return Response{OK: true, Message: "recorded"}, nil

	default:
		return Response{OK: false, Message: fmt.Sprintf("unknown command: %s", req.Cmd)}, nil
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

var errUnknownToken = errors.New("unknown token")
var errOwnerGone = errors.New("owner gone")

func matchesControlError(err error, target error) bool {
	return errors.Is(err, target) || err.Error() == target.Error()
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
	s.wg.Wait()
}

// SocketPath returns the socket path from the underlying listener.
func (s *Server) SocketPath() string {
	return s.listener.Addr().String()
}
