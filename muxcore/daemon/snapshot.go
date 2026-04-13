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
	return restored
}
