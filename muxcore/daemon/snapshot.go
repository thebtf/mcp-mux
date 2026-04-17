package daemon

import (
	"fmt"
	"log"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
	"github.com/thejerf/suture/v4"
)

// DaemonSnapshot is an alias for mcpsnapshot.DaemonSnapshot.
// Re-exported here so daemon-internal code can reference it without
// the package qualifier while tests continue to use the type directly.
type DaemonSnapshot = mcpsnapshot.DaemonSnapshot

// SnapshotPath returns the well-known path for the daemon state snapshot file.
func SnapshotPath() string {
	return mcpsnapshot.SnapshotPath("")
}

// SerializeSnapshot walks all owners, exports their state, and delegates
// to mcpsnapshot.Serialize for the atomic write.
func (d *Daemon) SerializeSnapshot() (string, error) {
	d.mu.RLock()
	owners := make([]mcpsnapshot.OwnerSnapshot, 0, len(d.owners))
	sessions := make([]mcpsnapshot.SessionSnapshot, 0)

	for sid, entry := range d.owners {
		if entry.Owner == nil {
			continue // skip placeholders
		}
		snap := entry.Owner.ExportSnapshot()
		snap.ServerID = sid
		snap.Mode = entry.Mode
		if snap.Env == nil {
			snap.Env = entry.Env
		}
		snap.Persistent = entry.Persistent
		owners = append(owners, snap)

		// Collect session metadata from this owner.
		for _, ss := range entry.Owner.ExportSessions() {
			ss.OwnerServerID = sid
			sessions = append(sessions, ss)
		}
	}
	d.mu.RUnlock()

	data := &DaemonSnapshot{
		Version:    mcpsnapshot.SnapshotVersion,
		MuxVersion: owner.Version,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Owners:     owners,
		Sessions:   sessions,
	}

	return mcpsnapshot.Serialize(data, d.logger)
}

// DeserializeSnapshot reads and validates a snapshot from the well-known path.
// Returns nil, nil if no snapshot exists (cold start). Deletes the file after
// successful load or if stale. Logs warnings for corrupt/stale snapshots.
func DeserializeSnapshot(logger interface{ Printf(string, ...any) }) (*DaemonSnapshot, error) {
	return mcpsnapshot.Deserialize(logger)
}

// loadSnapshot checks for a snapshot file and restores owners from it.
// Called on daemon startup. If no snapshot exists, returns 0 (cold start).
// Returns the number of owners restored.
func (d *Daemon) loadSnapshot() int {
	snap, err := DeserializeSnapshot(d.logger)
	if err != nil {
		d.logger.Printf("snapshot load error: %v", err)
		return 0
	}
	if snap == nil {
		return 0 // cold start
	}

	restored := 0
	for _, ownerSnap := range snap.Owners {
		sid := ownerSnap.ServerID
		ipcPath := serverid.IPCPath(sid, "")
		controlPath := serverid.ControlPath(sid, "")

		if ownerSnap.Classification == classify.ModeIsolated &&
			len(ownerSnap.CwdSet) > 1 {
			shortSID := sid
			if len(shortSID) > 8 {
				shortSID = shortSID[:8]
			}
			d.logger.Printf("snapshot: healing poisoned isolated owner %s: cwdSet %v → [%s]",
				shortSID, ownerSnap.CwdSet, ownerSnap.Cwd)
			ownerSnap.CwdSet = []string{ownerSnap.Cwd}
		}

		// Capture loop variables for closure.
		cmd, args := ownerSnap.Command, ownerSnap.Args
		o, err := owner.NewOwnerFromSnapshot(owner.OwnerConfig{
			Command:        cmd,
			Args:           args,
			Env:            ownerSnap.Env,
			Cwd:            ownerSnap.Cwd,
			IPCPath:        ipcPath,
			ControlPath:    controlPath,
			ServerID:       sid,
			TokenHandshake: true,
			OnZeroSessions: func(serverID string) {
				d.onZeroSessions(serverID)
			},
			OnUpstreamExit: func(serverID string) {
				d.onUpstreamExit(serverID)
			},
			OnPersistentDetected: func(serverID string) {
				d.SetPersistent(serverID, true)
			},
			OnCacheReady: func(serverID string) {
				d.mu.RLock()
				entry, ok := d.owners[serverID]
				d.mu.RUnlock()
				if !ok || entry.Owner == nil {
					return
				}
				s := entry.Owner.ExportSnapshot()
				d.updateTemplate(cmd, args, s)
			},
			Logger: log.New(d.logger.Writer(), fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags|log.Lmicroseconds),
		}, ownerSnap)
		if err != nil {
			d.logger.Printf("snapshot: failed to restore owner %s (%s): %v", sid[:8], ownerSnap.Command, err)
			continue
		}

		// Register with supervisor BEFORE inserting into owners map so that
		// any concurrent failure is handled by suture.
		var serviceToken suture.ServiceToken
		if d.supervisor != nil {
			serviceToken = d.supervisor.Add(o)
		}

		d.mu.Lock()
		d.owners[sid] = &OwnerEntry{
			Owner:        o,
			ServerID:     sid,
			Command:      ownerSnap.Command,
			Args:         ownerSnap.Args,
			Cwd:          ownerSnap.Cwd,
			Mode:         ownerSnap.Mode,
			Env:          ownerSnap.Env,
			Persistent:   ownerSnap.Persistent,
			LastSession:  time.Now(),
			IdleTimeout:  d.ownerIdleTimeout,
			serviceToken: serviceToken,
		}
		d.mu.Unlock()

		// Seed template cache from snapshot so new isolated spawns can use it immediately.
		if ownerSnap.CachedInit != "" && ownerSnap.CachedTools != "" {
			d.updateTemplate(ownerSnap.Command, ownerSnap.Args, ownerSnap)
		}

		// Spawn upstream in background — refreshes caches when ready.
		o.SpawnUpstreamBackground()

		d.logger.Printf("snapshot: restored owner %s for %s %v", sid[:8], ownerSnap.Command, ownerSnap.Args)
		restored++
	}

	d.logger.Printf("snapshot: restored %d/%d owners", restored, len(snap.Owners))

	// FR-3 — post-restore listener health gate.
	//
	// Snapshot restore synchronously calls ipc.Listen, so a bind failure would
	// already have aborted the entry above. But the listener can die AFTER
	// successful bind for reasons that do not flow through closeListener()
	// and therefore leave IsAccepting() lying. Observed in production on
	// 2026-04-17 after a graceful-restart: 6/9 restored owners passed
	// IsAccepting but refused ipc.Dial from a fresh shim. Until the exact
	// trigger is pinned down, validate every restored owner defensively and
	// tear down the zombies so the next shim request cold-spawns a fresh one.
	d.runRestoreHealthGate()

	return restored
}

// restoreHealthGateWindow is the time we allow newly-restored owners to fully
// bind their IPC listeners before the FR-3 sweep runs. Calibrated above the
// ipc.Dial 500ms timeout plus a small margin for scheduler jitter on slow CI
// runners. Declared as var so tests can override it.
var restoreHealthGateWindow = 750 * time.Millisecond

// runRestoreHealthGate walks every owner currently in d.owners and verifies
// its listener is reachable via an outbound dial probe. Entries that fail the
// probe are torn down and removed from the registry.
//
// Runs in a goroutine so the probe sweep does not block the startup path;
// the goroutine logs its summary and exits. Each probe uses ipc.Dial's
// 500ms timeout. The sweep takes an RLock snapshot of the owners map, then
// re-acquires the write lock under CAS (entry still matches what we probed)
// for each zombie found, so concurrent spawn/shutdown cannot produce torn
// state.
func (d *Daemon) runRestoreHealthGate() {
	// Read the tunable once here (main goroutine) and pass into the worker
	// via closure capture. Reading it inside the worker would race t.Cleanup
	// callers in tests that restore the var after the test function returns
	// but before the goroutine wakes.
	window := restoreHealthGateWindow
	go func() {
		// Respect daemon shutdown: if Shutdown closes d.done during our
		// sleep window, exit immediately instead of sweeping a daemon that
		// is already tearing itself down.
		select {
		case <-time.After(window):
		case <-d.done:
			return
		}

		d.mu.RLock()
		entries := make([]*OwnerEntry, 0, len(d.owners))
		for _, e := range d.owners {
			if e.Owner != nil {
				entries = append(entries, e)
			}
		}
		d.mu.RUnlock()

		zombies := 0
		for _, entry := range entries {
			// Re-check shutdown between probes so a large owner set cannot
			// extend our presence on a dying daemon.
			select {
			case <-d.done:
				return
			default:
			}

			// IMPORTANT: a zombie is an owner whose listener died WITHOUT a
			// closeListener() call — i.e. IsAccepting reports true (sync
			// channel still open) but IsReachable reports false (dial fails).
			// Owners that legitimately closed their listener (e.g. isolated
			// servers after the first session connects) report IsAccepting
			// false and IsReachable false; they are NOT zombies and we MUST
			// NOT tear them down here — the health gate is a defensive
			// check against the unreachable-despite-IsAccepting class only.
			if !entry.Owner.IsAccepting() {
				continue
			}
			// Probe outside d.mu — ipc.Dial can take up to 500ms.
			if entry.Owner.IsReachable() {
				continue
			}

			d.mu.Lock()
			sid := entry.ServerID
			current, ok := d.owners[sid]
			if !ok || current != entry {
				d.mu.Unlock()
				continue
			}
			d.zombieDetectedRestore++
			shortSID := sid
			if len(shortSID) > 8 {
				shortSID = shortSID[:8]
			}
			d.logger.Printf(
				"zombie-listener detected: path=restore server=%s ipc=%q cmd=%q action=tear-down-and-respawn-on-demand",
				shortSID, entry.Owner.IPCPath(), entry.Command,
			)
			delete(d.owners, sid)
			d.mu.Unlock()
			// Shutdown OUTSIDE the lock — it closes sockets, tears down
			// upstream, and may fire callbacks back into the daemon.
			entry.Owner.Shutdown()
			zombies++
		}
		if zombies > 0 {
			d.logger.Printf("post-restore health gate: tore down %d zombie owner(s)", zombies)
		}
	}()
}
