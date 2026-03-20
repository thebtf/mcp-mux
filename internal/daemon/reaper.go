package daemon

import (
	"log"
	"time"

	"github.com/thebtf/mcp-mux/internal/control"
)

// Reaper periodically sweeps daemon owners for cleanup:
// - Non-persistent owners with zero sessions past grace period → shutdown
// - Dead upstreams on persistent owners → re-spawn
// - Daemon idle (zero owners, zero sessions) past idle timeout → auto-exit
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

	for i, entry := range entries {
		sid := sids[i]
		sessions := entry.Owner.SessionCount()

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

		// Non-persistent + zero sessions + grace elapsed → remove
		if sessions == 0 && !entry.Persistent {
			elapsed := now.Sub(entry.LastSession)
			if elapsed > entry.GracePeriod {
				r.logger.Printf("reaper: owner %s grace expired (%.0fs), removing", sid[:8], elapsed.Seconds())
				_ = r.daemon.Remove(sid)
				affected++
			}
		}
	}

	return affected
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
		total += e.Owner.SessionCount()
	}
	return total
}
