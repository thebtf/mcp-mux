package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/thebtf/mcp-mux/internal/mux"
)

const (
	snapshotFileName   = "mcp-muxd-snapshot.json"
	snapshotVersion    = 1
	snapshotMaxAge     = 5 * time.Minute
)

// DaemonSnapshot captures the full daemon state for graceful restart.
type DaemonSnapshot struct {
	Version    int                  `json:"version"`
	MuxVersion string               `json:"mux_version"`
	Timestamp  string               `json:"timestamp"`
	Owners     []mux.OwnerSnapshot  `json:"owners"`
	Sessions   []mux.SessionSnapshot `json:"sessions"`
}

// SnapshotPath returns the well-known path for the daemon state snapshot file.
func SnapshotPath() string {
	return filepath.Join(os.TempDir(), snapshotFileName)
}

// SerializeSnapshot walks all owners, exports their state, and writes an atomic
// JSON snapshot to the well-known path. Uses temp file + rename for atomicity.
func (d *Daemon) SerializeSnapshot() (string, error) {
	d.mu.RLock()
	owners := make([]mux.OwnerSnapshot, 0, len(d.owners))
	sessions := make([]mux.SessionSnapshot, 0)

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

		// Collect session metadata from this owner
		for _, ss := range entry.Owner.ExportSessions() {
			ss.OwnerServerID = sid
			sessions = append(sessions, ss)
		}
	}
	d.mu.RUnlock()

	snapshot := DaemonSnapshot{
		Version:    snapshotVersion,
		MuxVersion: mux.Version,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Owners:     owners,
		Sessions:   sessions,
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return "", fmt.Errorf("snapshot marshal: %w", err)
	}

	path := SnapshotPath()

	// Atomic write: temp file + rename
	tmpFile, err := os.CreateTemp(filepath.Dir(path), "mcp-muxd-snapshot-*.tmp")
	if err != nil {
		return "", fmt.Errorf("snapshot create temp: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("snapshot write: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("snapshot close: %w", err)
	}

	// On Windows, os.Rename fails if target exists — remove first
	os.Remove(path)
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("snapshot rename: %w", err)
	}

	d.logger.Printf("snapshot written: %d owners, %d sessions (%d bytes)", len(owners), len(sessions), len(data))
	return path, nil
}

// DeserializeSnapshot reads and validates a snapshot from the well-known path.
// Returns nil, nil if no snapshot exists (cold start). Deletes the file after
// successful load or if stale. Logs warnings for corrupt/stale snapshots.
func DeserializeSnapshot(logger interface{ Printf(string, ...any) }) (*DaemonSnapshot, error) {
	path := SnapshotPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // cold start — no snapshot
		}
		return nil, fmt.Errorf("snapshot read: %w", err)
	}

	var snapshot DaemonSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		logger.Printf("warning: corrupt snapshot at %s, ignoring: %v", path, err)
		os.Remove(path)
		return nil, nil
	}

	if snapshot.Version != snapshotVersion {
		logger.Printf("warning: snapshot version %d != expected %d, ignoring", snapshot.Version, snapshotVersion)
		os.Remove(path)
		return nil, nil
	}

	// Check staleness
	ts, err := time.Parse(time.RFC3339, snapshot.Timestamp)
	if err != nil {
		logger.Printf("warning: invalid snapshot timestamp %q, ignoring", snapshot.Timestamp)
		os.Remove(path)
		return nil, nil
	}
	if time.Since(ts) > snapshotMaxAge {
		logger.Printf("warning: stale snapshot (%.0fs old), ignoring", time.Since(ts).Seconds())
		os.Remove(path)
		return nil, nil
	}

	// Consume: delete after successful load
	os.Remove(path)

	logger.Printf("snapshot loaded: %d owners, %d sessions (version %d, age %.1fs)",
		len(snapshot.Owners), len(snapshot.Sessions), snapshot.Version, time.Since(ts).Seconds())
	return &snapshot, nil
}
