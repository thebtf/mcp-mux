package daemon

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
)

func TestGenerationHandoffParityStatusRejectsFreshSpawnEvidence(t *testing.T) {
	os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })

	origWindow := restoreHealthGateWindow
	restoreHealthGateWindow = time.Hour
	t.Cleanup(func() { restoreHealthGateWindow = origWindow })

	sid := "aabbccdd-generation-parity"
	cwd := t.TempDir()
	snap := DaemonSnapshot{
		Version:          mcpsnapshot.SnapshotVersion,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		DaemonGeneration: "daemon-gen-predecessor",
		PredecessorPID:   4242,
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:        sid,
				Command:         "echo",
				Args:            []string{"generation"},
				Cwd:             cwd,
				CwdSet:          []string{cwd},
				Mode:            "global",
				Classification:  classify.ModeShared,
				OwnerGeneration: "owner-gen-predecessor",
				Persistent:      true,
				CachedInit:      "e30=",
				CachedTools:     "e30=",
			},
		},
		Sessions: []mcpsnapshot.SessionSnapshot{},
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal snapshot: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}

	tokenFile, err := os.CreateTemp("", "mcp-mux-generation-handoff-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenFile.Close()
	if err := os.WriteFile(tokenFile.Name(), []byte("generation-parity-token"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(tokenFile.Name()) })

	origHook := dialHandoffHook
	dialHandoffHook = func(_ string, _ time.Duration) (fdConn, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { dialHandoffHook = origHook })

	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenFile.Name())
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "/mock/unreachable-generation-handoff")

	ctlPath := shortSocketPath(t, "generation-handoff.ctl.sock")
	d, err := New(Config{
		Name:           "test-daemon",
		ControlPath:    ctlPath,
		GracePeriod:    time.Second,
		IdleTimeout:    5 * time.Second,
		SkipSnapshot:   true,
		SessionHandler: noopSessionHandler{},
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	if restored := d.loadSnapshot(); restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
	}

	status := d.HandleStatus()
	handoff, ok := status["handoff"].(map[string]any)
	if !ok {
		t.Fatalf("handoff type = %T, want map[string]any", status["handoff"])
	}
	if got := handoff["predecessor_pid"]; got != 4242 {
		t.Fatalf("handoff.predecessor_pid = %#v, want 4242", got)
	}
	if got := handoff["predecessor_daemon_generation"]; got != "daemon-gen-predecessor" {
		t.Fatalf("handoff.predecessor_daemon_generation = %#v, want daemon-gen-predecessor", got)
	}
	if got, ok := handoff["successor_daemon_generation"].(string); !ok || got == "" || got == "daemon-gen-predecessor" {
		t.Fatalf("handoff.successor_daemon_generation = %#v, want distinct non-empty successor generation", handoff["successor_daemon_generation"])
	}
	if got := uint64Status(t, handoff, "restored_owner_count"); got != 1 {
		t.Fatalf("handoff.restored_owner_count = %d, want 1", got)
	}
	if got := uint64Status(t, handoff, "old_owner_socket_retired_count"); got != 1 {
		t.Fatalf("handoff.old_owner_socket_retired_count = %d, want 1", got)
	}

	servers, ok := status["servers"].([]map[string]any)
	if !ok {
		t.Fatalf("servers type = %T, want []map[string]any", status["servers"])
	}
	if len(servers) != 1 {
		t.Fatalf("len(servers) = %d, want 1", len(servers))
	}
	server := servers[0]
	if got := server["restore_source"]; got == "fresh" {
		t.Fatalf("restore_source = %q; restored owner must not masquerade as fresh spawn", got)
	}
	if got := server["restore_source"]; got != "snapshot_fallback" {
		t.Fatalf("restore_source = %#v, want snapshot_fallback", got)
	}
	if got := server["restored_from_owner_generation"]; got != "owner-gen-predecessor" {
		t.Fatalf("restored_from_owner_generation = %#v, want owner-gen-predecessor", got)
	}
	if got, ok := server["owner_generation"].(string); !ok || got == "" || got == "owner-gen-predecessor" {
		t.Fatalf("owner_generation = %#v, want distinct non-empty restored owner generation", server["owner_generation"])
	}
	if got := server["persistent"]; got != true {
		t.Fatalf("persistent = %#v, want true", got)
	}
}
