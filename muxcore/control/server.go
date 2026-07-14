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
	listener   net.Listener
	socketPath string
	handler    CommandHandler
	logger     *log.Logger

	mu     sync.Mutex
	closed bool
	done   chan struct{}
	wg     sync.WaitGroup
}

var (
	acceptErrorBackoff    = 100 * time.Millisecond
	sleepAfterAcceptError = time.Sleep
)

// NewServer creates and starts a control server at the given socket path.
func NewServer(socketPath string, handler CommandHandler, logger *log.Logger) (*Server, error) {
	ln, err := ipc.Listen(socketPath)
	if err != nil {
		return nil, fmt.Errorf("control: listen %s: %w", socketPath, err)
	}

	s := &Server{
		listener:   ln,
		socketPath: socketPath,
		handler:    handler,
		logger:     logger,
		done:       make(chan struct{}),
	}

	go s.acceptLoop()

	return s, nil
}

func (s *Server) acceptLoop() {
	defer func() {
		if r := recover(); r != nil {
			if s.isClosed() {
				s.logger.Printf("control: accept loop exiting after listener close panic: %v", r)
				return
			}
			panic(r)
		}
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.isClosed() {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				s.logger.Printf("control: accept loop exiting after unexpected listener close: %v", err)
				return
			}
			s.logger.Printf("control: accept error: %v", err)
			sleepAfterAcceptError(acceptErrorBackoff)
			continue
		}
		if s.isClosed() {
			conn.Close()
			return
		}

		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
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
		_ = s.writeResponse(conn, Response{OK: false, Message: fmt.Sprintf("invalid request: %v", err)})
		return
	}

	// Dispatch runs the handler (e.g. graceful-restart may take longer than
	// clientDeadline). Set the write deadline only after dispatch returns so
	// it bounds only the I/O write, not handler execution.
	resp, afterFn := s.dispatch(req)
	hasAfterFn := afterFn != nil
	if hasAfterFn {
		s.logger.Printf("control.handleConn cmd=%s phase=dispatch_done", req.Cmd)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(clientDeadline)); err != nil {
		s.logger.Printf("control: set write deadline: %v", err)
		s.rollbackUndeliveredSpawn(req, resp, err)
		return
	}
	writeErr := s.writeResponse(conn, resp)
	if writeErr != nil {
		s.rollbackUndeliveredSpawn(req, resp, writeErr)
	}
	if hasAfterFn {
		s.logger.Printf("control.handleConn cmd=%s phase=write_done", req.Cmd)
		conn.Close()
		s.logger.Printf("control.handleConn cmd=%s phase=close_done", req.Cmd)
		s.logger.Printf("control.handleConn cmd=%s phase=calling_afterFn", req.Cmd)
		afterFn()
		s.logger.Printf("control.handleConn cmd=%s phase=afterFn_returned", req.Cmd)
	}
}

func (s *Server) rollbackUndeliveredSpawn(req Request, resp Response, writeErr error) {
	if req.Cmd != "spawn" || !resp.OK || resp.ServerID == "" || resp.Token == "" {
		return
	}
	handler, ok := s.handler.(SpawnResponseFailureHandler)
	if !ok {
		return
	}
	s.logger.Printf("control: spawn response undelivered for server %s: %v", resp.ServerID, writeErr)
	handler.HandleSpawnResponseFailure(resp.ServerID, resp.Token)
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

	case "stop_owner":
		oh, ok := s.handler.(OwnerStopHandler)
		if !ok {
			return Response{OK: false, Message: "stop_owner not supported (not a daemon)"}, nil
		}
		msg, err := oh.HandleStopOwner(req)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("stop_owner: %v", err)}, nil
		}
		return Response{OK: true, Message: msg}, nil

	case "graceful-restart":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "graceful-restart not supported (not a daemon)"}, nil
		}
		var snapshotPath string
		var afterFn func()
		var err error
		if oh, ok := dh.(GracefulRestartOptionsHandler); ok {
			snapshotPath, afterFn, err = oh.HandleGracefulRestartWithOptions(GracefulRestartOptions{
				DrainTimeoutMs: req.DrainTimeoutMs,
				SuccessorExe:   req.SuccessorExe,
			})
		} else {
			snapshotPath, afterFn, err = dh.HandleGracefulRestart(req.DrainTimeoutMs)
		}
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

	case "can_suspend":
		var result SuspendCheckResponse
		var err error
		if sh, ok := s.handler.(SuspendCheckForOwnerHandler); ok {
			result, err = sh.HandleCanSuspendForOwner(req.PrevToken, req.ServerID)
		} else if sh, ok := s.handler.(SuspendCheckHandler); ok {
			result, err = sh.HandleCanSuspend(req.PrevToken)
		} else {
			return Response{OK: false, Message: "can_suspend not supported"}, nil
		}
		if err != nil {
			return Response{OK: false, Message: err.Error()}, nil
		}
		data, err := json.Marshal(result)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("marshal can_suspend: %v", err)}, nil
		}
		return Response{OK: true, Data: data}, nil

	case "reconnect-give-up":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "reconnect-give-up not supported (not a daemon)"}, nil
		}
		if err := dh.HandleReconnectGiveUp(req.ReconnectReason); err != nil {
			return Response{OK: false, Message: err.Error()}, nil
		}
		return Response{OK: true, Message: "recorded"}, nil

	case "list_owners":
		dh, ok := s.handler.(DaemonHandler)
		if !ok {
			return Response{OK: false, Message: "list_owners not supported (not a daemon)"}, nil
		}
		result, err := dh.HandleListOwners(req)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("list_owners: %v", err)}, nil
		}
		data, err := json.Marshal(result)
		if err != nil {
			return Response{OK: false, Message: fmt.Sprintf("marshal list_owners: %v", err)}, nil
		}
		return Response{OK: true, Data: data}, nil

	default:
		return Response{OK: false, Message: fmt.Sprintf("unknown command: %s", req.Cmd)}, nil
	}
}

func (s *Server) writeResponse(conn net.Conn, resp Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		s.logger.Printf("control: marshal response: %v", err)
		return err
	}
	data = append(data, '\n')
	n, err := conn.Write(data)
	if n == len(data) {
		return nil
	}
	if err != nil {
		s.logger.Printf("control: write response: %v", err)
		return err
	}
	s.logger.Printf("control: short write response: wrote %d of %d bytes", n, len(data))
	return io.ErrShortWrite
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
	if s.socketPath != "" {
		ipc.Cleanup(s.socketPath)
	}
	s.logger.Printf("control.Close: listener closed, waiting for active handlers...")
	s.wg.Wait()
	s.logger.Printf("control.Close: all handlers done")
}

// SocketPath returns the socket path from the underlying listener.
func (s *Server) SocketPath() string {
	return s.socketPath
}
