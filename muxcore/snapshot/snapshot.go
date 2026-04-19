// Package snapshot provides serializable types and IO operations for daemon
// graceful-restart snapshots. The daemon package wires these types to live
// Owner state; this package handles only marshalling and file IO.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
)

const (
	SnapshotFileName = "mcp-muxd-snapshot.json"
	SnapshotVersion  = 1
	SnapshotMaxAge   = 5 * time.Minute
)

// OwnerSnapshot captures the serializable state of an Owner for graceful restart.
// Cached responses are base64-encoded raw JSON-RPC messages.
type OwnerSnapshot struct {
	ServerID                string               `json:"server_id"`
	Command                 string               `json:"command"`
	Args                    []string             `json:"args"`
	Env                     map[string]string    `json:"env,omitempty"`
	Cwd                     string               `json:"cwd"`
	CwdSet                  []string             `json:"cwd_set"`
	Mode                    string               `json:"mode"`
	Classification          classify.SharingMode `json:"classification,omitempty"`
	ClassificationSource    string               `json:"classification_source,omitempty"`
	ClassificationReason    []string             `json:"classification_reason,omitempty"`
	Persistent              bool                 `json:"persistent,omitempty"`
	CachedInit              string               `json:"cached_init,omitempty"`
	CachedTools             string               `json:"cached_tools,omitempty"`
	CachedPrompts           string               `json:"cached_prompts,omitempty"`
	CachedResources         string               `json:"cached_resources,omitempty"`
	CachedResourceTemplates string               `json:"cached_resource_templates,omitempty"`
	// Handoff fields (v0.21.0+). Zero-values = cold-spawn path (FR-8 backwards-compat).
	// Non-zero UpstreamPID + non-empty HandoffSocketPath = reattach path (FR-1/FR-9).
	UpstreamPID       int    `json:"upstream_pid,omitempty"`
	HandoffSocketPath string `json:"handoff_socket_path,omitempty"`
	SpawnPgid         int    `json:"spawn_pgid,omitempty"`
}

// SessionSnapshot captures the serializable state of a Session for graceful restart.
type SessionSnapshot struct {
	MuxSessionID  string            `json:"mux_session_id"`
	Cwd           string            `json:"cwd"`
	Env           map[string]string `json:"env,omitempty"`
	OwnerServerID string            `json:"owner_server_id"`
}

// DaemonSnapshot captures the full daemon state for graceful restart.
type DaemonSnapshot struct {
	Version    int               `json:"version"`
	MuxVersion string            `json:"mux_version"`
	Timestamp  string            `json:"timestamp"`
	Owners     []OwnerSnapshot   `json:"owners"`
	Sessions   []SessionSnapshot `json:"sessions"`
}

// SnapshotPath returns the well-known path for the daemon state snapshot file.
// If baseDir is empty, os.TempDir() is used.
func SnapshotPath(baseDir string) string {
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	return filepath.Join(baseDir, SnapshotFileName)
}

// Serialize marshals data to JSON and writes it atomically to SnapshotPath("").
// Uses a temp file + rename for atomicity. Returns the final path on success.
func Serialize(data *DaemonSnapshot, logger interface{ Printf(string, ...any) }) (string, error) {
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("snapshot marshal: %w", err)
	}

	path := SnapshotPath("")

	// Atomic write: temp file + rename.
	tmpFile, err := os.CreateTemp(filepath.Dir(path), "mcp-muxd-snapshot-*.tmp")
	if err != nil {
		return "", fmt.Errorf("snapshot create temp: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(encoded); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("snapshot write: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("snapshot close: %w", err)
	}

	// On Windows, os.Rename fails if target exists — remove first.
	os.Remove(path)
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("snapshot rename: %w", err)
	}

	logger.Printf("snapshot written: %d owners, %d sessions (%d bytes)",
		len(data.Owners), len(data.Sessions), len(encoded))
	return path, nil
}

// Deserialize reads and validates a snapshot from SnapshotPath("").
// Returns nil, nil if no snapshot exists (cold start). Deletes the file after
// successful load or if the snapshot is stale or corrupt. Logs warnings.
func Deserialize(logger interface{ Printf(string, ...any) }) (*DaemonSnapshot, error) {
	path := SnapshotPath("")

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // cold start — no snapshot
		}
		return nil, fmt.Errorf("snapshot read: %w", err)
	}

	var snap DaemonSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		logger.Printf("warning: corrupt snapshot at %s, ignoring: %v", path, err)
		os.Remove(path)
		return nil, nil
	}

	if snap.Version != SnapshotVersion {
		logger.Printf("warning: snapshot version %d != expected %d, ignoring", snap.Version, SnapshotVersion)
		os.Remove(path)
		return nil, nil
	}

	ts, err := time.Parse(time.RFC3339, snap.Timestamp)
	if err != nil {
		logger.Printf("warning: invalid snapshot timestamp %q, ignoring", snap.Timestamp)
		os.Remove(path)
		return nil, nil
	}
	if time.Since(ts) > SnapshotMaxAge {
		logger.Printf("warning: stale snapshot (%.0fs old), ignoring", time.Since(ts).Seconds())
		os.Remove(path)
		return nil, nil
	}

	// Consume: delete after successful load.
	os.Remove(path)

	logger.Printf("snapshot loaded: %d owners, %d sessions (version %d, age %.1fs)",
		len(snap.Owners), len(snap.Sessions), snap.Version, time.Since(ts).Seconds())
	return &snap, nil
}
