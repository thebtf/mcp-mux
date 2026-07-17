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

// ErrHandoffAlreadyPrepared is returned while an earlier prepared transfer
// still awaits Commit or Abort settlement.
var ErrHandoffAlreadyPrepared = errors.New("owner: handoff already prepared")

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
	o.teardownOnce.Do(func() {
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
	})
}

func (o *Owner) beginHandoffTransition() *upstream.Process {
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.RLock()
	proc := o.upstream
	o.mu.RUnlock()
	if proc != nil {
		o.retiringProcess = proc
		o.materializationState = MaterializationFinalizing
		o.upstreamDead.Store(true)
	}
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	return proc
}

func (o *Owner) recordFailedHandoffTransition(proc *upstream.Process, err error, proven bool) {
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	if proven {
		o.mu.Lock()
		if o.upstream == proc {
			o.upstream = nil
		}
		o.mu.Unlock()
		if o.retiringProcess == proc {
			o.retiringProcess = nil
		}
		o.materializationBlockedErr = nil
		o.materializationState = MaterializationCacheOnly
	} else {
		o.materializationState = MaterializationFinalizeBlocked
		o.materializationBlockedErr = errors.Join(errFinalizationUnproven, err)
		o.retiringProcess = proc
	}
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
}

func (o *Owner) commitPreparedHandoff(proc *upstream.Process, pid int) error {
	o.removalMu.Lock()
	defer o.removalMu.Unlock()
	if !o.handoffPrepared {
		select {
		case <-o.done:
			return nil
		default:
			return ErrHandoffAlreadyPrepared
		}
	}
	err := proc.CommitDetach()
	if !proc.RetirementProven() {
		o.recordFailedHandoffTransition(proc, err, false)
		return errors.Join(errFinalizationUnproven, err)
	}
	if err != nil {
		// The successor already owns duplicated stdio and tree authority once
		// detach reaches committed. A local authority-handle close warning must
		// not trigger cold fallback beside the accepted successor generation.
		o.logger.Printf("owner handoff commit finalized with local release warning: %v", err)
	}
	o.handoffPrepared = false
	o.recordFailedHandoffTransition(proc, nil, true)
	o.completeShutdown(fmt.Sprintf("owner handed off (pid=%d)", pid))
	return nil
}

func (o *Owner) abortPreparedHandoff(proc *upstream.Process, pid int) error {
	o.removalMu.Lock()
	defer o.removalMu.Unlock()
	if !o.handoffPrepared {
		select {
		case <-o.done:
			return nil
		default:
			return ErrHandoffAlreadyPrepared
		}
	}
	err := proc.AbortDetach()
	proven := proc.RetirementProven()
	o.recordFailedHandoffTransition(proc, err, proven)
	if proven {
		o.handoffPrepared = false
	}
	if proven {
		o.completeShutdown(fmt.Sprintf("owner handoff aborted and finalized (pid=%d)", pid))
	}
	return err
}

// ShutdownForHandoff performs the same admission and session teardown as
// Shutdown, but prepares the upstream process for transactional transfer.
// The returned payload retains the owner's removal lease logically: o.done
// remains open until Commit proves transferred authority or Abort proves full
// process-tree retirement. FinalizeForRemoval may retry a failed abort later.
//
// Error cases:
//   - ErrAlreadyShutDown — shutdown or a prior settled handoff completed.
//   - ErrHandoffAlreadyPrepared — a prepared transfer still awaits settlement.
//   - ErrNoUpstream — no subprocess exists; teardown completes immediately.
//   - wrapped detach errors — cleanup is attempted and completion occurs only
//     when process-tree retirement is proven.
func (o *Owner) ShutdownForHandoff() (HandoffPayload, error) {
	o.removalMu.Lock()
	defer o.removalMu.Unlock()
	select {
	case <-o.done:
		return HandoffPayload{}, ErrAlreadyShutDown
	default:
	}
	if o.handoffPrepared {
		return HandoffPayload{}, ErrHandoffAlreadyPrepared
	}

	if err := o.quiesceMaterializationForHandoff(); err != nil {
		up := o.beginHandoffTransition()
		o.teardownExceptUpstream()
		closeErr := error(nil)
		proven := up == nil
		if up != nil {
			closeErr = up.Close()
			proven = up.RetirementProven()
		}
		retErr := errors.Join(fmt.Errorf("owner: quiesce materialization for handoff: %w", err), closeErr)
		o.recordFailedHandoffTransition(up, retErr, proven)
		if proven {
			o.completeShutdown("owner handoff quiesce failed; upstream finalized")
		}
		return HandoffPayload{}, retErr
	}

	up := o.beginHandoffTransition()
	o.teardownExceptUpstream()
	if up == nil {
		o.completeShutdown("owner handoff completed without upstream")
		return HandoffPayload{}, ErrNoUpstream
	}

	pid, stdinFD, stdoutFD, stderrFD, authorityFD, err := up.DetachWithAuthority()
	if err != nil {
		cleanupErr := up.AbortDetach()
		if errors.Is(cleanupErr, upstream.ErrDetachNotPrepared) {
			cleanupErr = up.Close()
		} else {
			cleanupErr = errors.Join(cleanupErr, up.Close())
		}
		retErr := errors.Join(fmt.Errorf("owner: detach upstream: %w", err), cleanupErr)
		proven := up.RetirementProven()
		o.recordFailedHandoffTransition(up, retErr, proven)
		if proven {
			o.completeShutdown("owner handoff failed; upstream finalized")
		}
		o.logger.Printf("owner handoff failed: %v", retErr)
		return HandoffPayload{}, retErr
	}

	o.handoffPrepared = true

	payload := HandoffPayload{
		ServerID:    o.serverID,
		PID:         pid,
		StdinFD:     stdinFD,
		StdoutFD:    stdoutFD,
		StderrFD:    stderrFD,
		AuthorityFD: authorityFD,
		Command:     o.command,
		Args:        o.args,
		Cwd:         o.cwd,
		abort: func() error {
			return o.abortPreparedHandoff(up, pid)
		},
		commit: func() error {
			return o.commitPreparedHandoff(up, pid)
		},
	}
	return payload, nil
}
