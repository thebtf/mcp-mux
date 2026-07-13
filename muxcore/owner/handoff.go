package owner

import (
	"errors"
	"fmt"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
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
	ServerID    string
	PID         int
	StdinFD     uintptr
	StdoutFD    uintptr
	StderrFD    uintptr
	AuthorityFD uintptr
	Command     string
	Args        []string
	Cwd         string

	abort  func() error
	commit func() error
}

// Abort terminates a prepared detached tree. It is safe to call repeatedly.
func (p HandoffPayload) Abort() error {
	if p.abort == nil {
		return nil
	}
	return p.abort()
}

// Commit releases the predecessor's stdio and tree authority only after the
// successor reports that the owner was constructed and registered.
func (p HandoffPayload) Commit() error {
	if p.commit == nil {
		return nil
	}
	return p.commit()
}

// HasHandoffUpstream reports whether this owner has a subprocess that can be
// detached and transferred to a successor daemon. SessionHandler-only owners
// have no upstream process; they recover through snapshot restore instead.
func (o *Owner) HasHandoffUpstream() bool {
	if o == nil {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.upstream != nil
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
// After a successful ShutdownForHandoff the upstream process continues running
// under a prepared lease. o.done is closed; the caller must later invoke the
// payload's Commit or Abort operation exactly once (both are idempotent).
//
// Error cases:
//   - ErrAlreadyShutDown — Shutdown or ShutdownForHandoff was already called.
//     o.done reflects how that earlier shutdown completed.
//   - ErrNoUpstream — owner has no subprocess to hand off (nil upstream).
//     The already-torn-down owner is completed and o.done is closed.
//   - wrapped upstream.ErrAlreadyDetached / ErrDetachUnsupported — Detach failed.
//     The upstream is aborted or closed, then o.done is closed. A failed handoff
//     never leaves a torn-down owner or process tree in limbo.
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
			close(o.done)
			return
		}

		pid, stdinFD, stdoutFD, stderrFD, authorityFD, err := up.DetachWithAuthority()
		if err != nil {
			cleanupErr := up.AbortDetach()
			if errors.Is(cleanupErr, upstream.ErrDetachNotPrepared) {
				cleanupErr = up.Close()
			} else {
				cleanupErr = errors.Join(cleanupErr, up.Close())
			}
			retErr = errors.Join(fmt.Errorf("owner: detach upstream: %w", err), cleanupErr)
			o.logger.Printf("owner handoff failed; upstream terminated: %v", retErr)
			close(o.done)
			return
		}

		payload = HandoffPayload{
			ServerID:    o.serverID,
			PID:         pid,
			StdinFD:     stdinFD,
			StdoutFD:    stdoutFD,
			StderrFD:    stderrFD,
			AuthorityFD: authorityFD,
			Command:     o.command,
			Args:        o.args,
			Cwd:         o.cwd,
			abort:       up.AbortDetach,
			commit:      up.CommitDetach,
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
