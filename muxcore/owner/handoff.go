package owner

import (
	"errors"
	"fmt"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

// ErrAlreadyShutDown is returned by ShutdownForHandoff if Shutdown (or a prior
// ShutdownForHandoff) was already called on this Owner.
var ErrAlreadyShutDown = errors.New("owner: already shut down")

// ErrNoUpstream is returned by ShutdownForHandoff if the owner has no upstream
// process to detach (nil upstream — in-process handler or no spawn yet).
var ErrNoUpstream = errors.New("owner: no upstream to hand off")

// HandoffPayload carries the detached upstream process information needed for
// the new owner to adopt the process without restarting it.
type HandoffPayload struct {
	ServerID string
	PID      int
	StdinFD  uintptr
	StdoutFD uintptr
	Command  string
	Args     []string
	Cwd      string
}

// teardownExceptUpstream closes the control server, IPC listener, and all
// active sessions. It is the shared first-half of both Shutdown and
// ShutdownForHandoff. Safe to call with an already-empty sessions map.
// closeListenerOnce ensures the listener is closed at most once.
func (o *Owner) teardownExceptUpstream() {
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

	if o.rejectionLogger != nil {
		o.rejectionLogger.Close()
	}
}

// ShutdownForHandoff performs the same cleanup as Shutdown (closes listener,
// sessions, control server) but instead of closing the upstream process it
// detaches from it — returning the PID and file descriptors so the caller can
// transfer ownership to a new daemon without restarting the upstream.
//
// After a successful ShutdownForHandoff the upstream process continues running.
// o.done is closed and o.Done() returns immediately. A subsequent call to
// Shutdown() is a safe no-op because shutdownOnce has already fired.
//
// Error cases:
//   - ErrAlreadyShutDown — Shutdown or ShutdownForHandoff was already called.
//     o.done may or may not be closed depending on how shutdown originally ended.
//   - ErrNoUpstream — owner has no subprocess to hand off (nil upstream).
//     o.done is NOT closed; the owner remains in a limbo state.
//   - wrapped upstream.ErrAlreadyDetached / ErrDetachUnsupported — Detach failed.
//     o.done is NOT closed; the owner remains in a limbo state.
func (o *Owner) ShutdownForHandoff() (HandoffPayload, error) {
	var payload HandoffPayload
	var retErr error
	attempted := false

	o.shutdownOnce.Do(func() {
		attempted = true

		o.teardownExceptUpstream()

		o.mu.Lock()
		up := o.upstream
		o.mu.Unlock()

		if up == nil {
			retErr = ErrNoUpstream
			// Do NOT close(o.done) — owner failed to hand off; caller must
			// decide whether to hard-shutdown or retry.
			return
		}

		pid, stdinFD, stdoutFD, err := up.Detach()
		if err != nil {
			retErr = fmt.Errorf("owner: detach upstream: %w", err)
			// Do NOT close(o.done) — detach failed; upstream is still owned.
			return
		}

		payload = HandoffPayload{
			ServerID: o.serverID,
			PID:      pid,
			StdinFD:  stdinFD,
			StdoutFD: stdoutFD,
			Command:  o.command,
			Args:     o.args,
			Cwd:      o.cwd,
		}

		o.logger.Printf("owner handed off (pid=%d)", pid)
		close(o.done)
	})

	if !attempted {
		// shutdownOnce already fired — either Shutdown or ShutdownForHandoff ran.
		return HandoffPayload{}, ErrAlreadyShutDown
	}
	return payload, retErr
}
