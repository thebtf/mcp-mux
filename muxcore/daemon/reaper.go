package daemon

import (
	"log"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

// Reaper periodically sweeps daemon owners for cleanup.
//
// v0.11.0 "Activity-Aware Reaping": the old grace-period model (kill on zero
// sessions after 30s) was replaced with an activity-aware model because the
// grace-period semantic killed stateful async work that didn't emit
// pending_requests (aimux background jobs, long-running inference).
//
// An owner is eligible for removal only when ALL of the following hold:
//   1. sessions == 0
//   2. not persistent (x-mux.persistent: true pins the owner forever)
//   3. pending JSON-RPC requests == 0
//   4. active progress tokens == 0 (no long-running tool/call streaming)
//   5. no active busy declarations (x-mux busy protocol)
//   6. lastActivity older than the owner's idleTimeout
//
// The default idleTimeout is 10 minutes (was 30s grace in v0.10.x).
// Overridable via MCP_MUX_OWNER_IDLE env or per-owner via x-mux.idleTimeout.
//
// Dead upstreams on persistent owners are still re-spawned, and the daemon
// still auto-exits after its own idle period when it has zero owners.
type Reaper struct {
	daemon   *Daemon
	interval time.Duration
	logger   *log.Logger
	done     chan struct{}
	stopped  chan struct{}
}

// NewReaper creates and starts a reaper goroutine.
func NewReaper(d *Daemon, interval time.Duration) *Reaper {
	if interval == 0 {
		interval = 10 * time.Second
	}
	r := &Reaper{
		daemon:   d,
		interval: interval,
		logger:   d.logger,
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go r.loop()
	return r
}

// Stop signals the reaper to stop and waits for it to finish.
func (r *Reaper) Stop() {
	select {
	case <-r.done:
		return // already stopped
	default:
		close(r.done)
	}
	<-r.stopped
}

func (r *Reaper) loop() {
	defer close(r.stopped)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	lastActivity := time.Now()

	for {
		select {
		case <-r.done:
			return
		case <-r.daemon.Done():
			return
		case <-ticker.C:
			swept := r.sweep()
			if swept > 0 || r.daemon.OwnerCount() > 0 || r.totalSessions() > 0 {
				lastActivity = time.Now()
			}

			// Idle auto-exit: no owners and no sessions for idleTimeout
			if r.daemon.OwnerCount() == 0 && r.totalSessions() == 0 {
				if time.Since(lastActivity) > r.daemon.idleTimeout {
					r.logger.Printf("reaper: daemon idle for %s, auto-exiting", r.daemon.idleTimeout)
					go r.daemon.Shutdown()
					return
				}
			}
		}
	}
}

// sweep runs one GC cycle and returns the number of owners affected.
func (r *Reaper) sweep() int {
	now := time.Now()
	affected := 0

	r.daemon.mu.RLock()
	entries := make([]*OwnerEntry, 0, len(r.daemon.owners))
	sids := make([]string, 0, len(r.daemon.owners))
	for sid, e := range r.daemon.owners {
		entries = append(entries, e)
		sids = append(sids, sid)
	}
	r.daemon.mu.RUnlock()

	// Sweep expired pending and bound reconnect-history tokens across all owners.
	// Prevents unbounded growth when shims never connect and when consumed
	// reconnect history ages past its 30-minute refresh window.
	totalPendingSwept := 0
	totalBoundSwept := 0
	for _, entry := range entries {
		if entry.Owner == nil {
			continue
		}
		totalPendingSwept += entry.Owner.SessionMgr().SweepExpiredPending()
		totalBoundSwept += entry.Owner.SessionMgr().SweepExpiredBound()
	}
	if totalPendingSwept > 0 {
		r.logger.Printf("reaper: swept %d expired pending tokens", totalPendingSwept)
	}
	if totalBoundSwept > 0 {
		r.logger.Printf("reaper: swept %d expired bound tokens", totalBoundSwept)
	}

	for i, entry := range entries {
		sid := sids[i]

		// Skip placeholders — owner is still being created by Spawn.
		if entry.Owner == nil {
			continue
		}

		// Check if upstream is dead
		select {
		case <-entry.Owner.Done():
			// Owner is shut down (upstream exited and was cleaned up)
			if entry.Persistent {
				r.logger.Printf("reaper: persistent owner %s dead, re-spawning", sid[:8])
				r.respawn(entry)
				affected++
			}
			continue
		default:
		}

		sample := evictionSample{
			Sessions:             entry.Owner.SessionCount(),
			Persistent:           entry.Persistent,
			PendingRequests:      entry.Owner.PendingRequests(),
			ActiveProgressTokens: entry.Owner.ActiveProgressTokens(),
			HasBusyWork:          entry.Owner.HasActiveBusyWork(),
			UpstreamDead:         entry.Owner.UpstreamDead(),
			IdleTimeout:          entry.IdleTimeout,
			OwnerIdleOverride:    entry.Owner.IdleTimeout(),
			LastSession:          entry.LastSession,
			LastActivity:         entry.Owner.LastActivity(),
		}
		decision := shouldEvict(sample, now, r.daemon.ownerIdleTimeout)
		if decision.evict {
			if decision.reason == "zombie" {
				// Upstream is already dead — hard remove (no point in stdin close).
				r.logger.Printf("reaper: owner %s upstream dead with 0 sessions, removing", sid[:8])
				_ = r.daemon.Remove(sid)
			} else {
				// Idle eviction: give upstream a chance to exit cleanly via stdin close.
				// SoftRemove closes stdin and waits up to 30s before SIGTERM/SIGKILL (US3).
				r.logger.Printf(
					"reaper: owner %s idle for %.0fs (timeout %.0fs), soft-removing",
					sid[:8], decision.elapsed.Seconds(), decision.idleTimeout.Seconds(),
				)
				_ = r.daemon.SoftRemove(sid)
			}
			affected++
		}
	}

	return affected
}

// evictionSample captures all state the reaper needs to decide whether an
// owner is eligible for removal. Extracted as a value type so the kill-gate
// logic (shouldEvict) is a pure function and can be unit-tested without
// constructing a real Owner.
type evictionSample struct {
	Sessions             int
	Persistent           bool
	PendingRequests      int64
	ActiveProgressTokens int
	HasBusyWork          bool
	UpstreamDead         bool
	// IdleTimeout is the owner-entry idle timeout (set at Spawn time
	// from daemon defaults).
	IdleTimeout time.Duration
	// OwnerIdleOverride is the x-mux.idleTimeout capability (0 if unset).
	OwnerIdleOverride time.Duration
	LastSession       time.Time
	LastActivity      time.Time
}

type evictionDecision struct {
	evict       bool
	elapsed     time.Duration
	idleTimeout time.Duration
	reason      string // "zombie" or "idle"
}

// shouldEvict applies the activity-aware kill gate:
//
//	sessions == 0 AND
//	!persistent AND
//	pendingRequests == 0 AND
//	activeProgressTokens == 0 AND
//	!hasBusyWork AND
//	now - max(lastSession, lastActivity) > idleTimeout
//
// The effective idleTimeout is, in priority order:
//  1. OwnerIdleOverride from x-mux.idleTimeout capability
//  2. Sample.IdleTimeout (daemon default copied at spawn)
//  3. daemonDefault argument (fallback for placeholder entries)
func shouldEvict(s evictionSample, now time.Time, daemonDefault time.Duration) evictionDecision {
	// Zombie: upstream dead + zero sessions = evict immediately regardless of other conditions.
	if s.UpstreamDead && s.Sessions == 0 {
		return evictionDecision{evict: true, reason: "zombie"}
	}
	if s.Sessions != 0 {
		return evictionDecision{}
	}
	if s.Persistent {
		return evictionDecision{}
	}
	if s.PendingRequests > 0 {
		return evictionDecision{}
	}
	if s.ActiveProgressTokens > 0 {
		return evictionDecision{}
	}
	if s.HasBusyWork {
		return evictionDecision{}
	}

	idleTimeout := s.IdleTimeout
	if s.OwnerIdleOverride > 0 {
		idleTimeout = s.OwnerIdleOverride
	}
	if idleTimeout <= 0 {
		idleTimeout = daemonDefault
	}
	if idleTimeout <= 0 {
		// No timeout configured anywhere — do not evict.
		return evictionDecision{}
	}

	idleSince := s.LastSession
	if s.LastActivity.After(idleSince) {
		idleSince = s.LastActivity
	}
	elapsed := now.Sub(idleSince)
	return evictionDecision{
		evict:       elapsed > idleTimeout,
		elapsed:     elapsed,
		idleTimeout: idleTimeout,
		reason:      "idle",
	}
}

// respawn attempts to re-create a persistent owner after upstream death.
func (r *Reaper) respawn(entry *OwnerEntry) {
	sid := entry.ServerID
	// Remove the dead entry first
	_ = r.daemon.Remove(sid)

	// Re-spawn with same parameters
	_, newSID, _, err := r.daemon.Spawn(control.Request{
		Command: entry.Command,
		Args:    entry.Args,
		Cwd:     entry.Cwd,
		Mode:    entry.Mode,
		Env:     entry.Env,
	})
	if err != nil {
		r.logger.Printf("reaper: failed to re-spawn %s: %v", sid[:8], err)
		return
	}

	// Preserve persistent flag
	r.daemon.SetPersistent(newSID, true)
	r.logger.Printf("reaper: re-spawned %s as %s", sid[:8], newSID[:8])
}

// totalSessions returns the total number of sessions across all owners.
func (r *Reaper) totalSessions() int {
	r.daemon.mu.RLock()
	defer r.daemon.mu.RUnlock()
	total := 0
	for _, e := range r.daemon.owners {
		if e.Owner != nil {
			total += e.Owner.SessionCount()
		}
	}
	return total
}
