package daemon

import (
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func TestHandleStatus_StableOperatorContract(t *testing.T) {
	d := testDaemon(t)

	initial := d.HandleStatus()
	if got, ok := initial["daemon_generation"].(string); !ok || got == "" {
		t.Fatalf("daemon_generation = %#v, want non-empty string", initial["daemon_generation"])
	}
	if got := uint64Status(t, initial, "reaped_owner_count"); got != 0 {
		t.Fatalf("reaped_owner_count = %d, want 0 before owner removal", got)
	}
	assertOwnerRemovalStatus(t, initial, 0, "operator_hard", 0)
	assertHandoffRestoredCount(t, initial, 0)

	_, sid, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	withOwner := d.HandleStatus()
	servers, ok := withOwner["servers"].([]map[string]any)
	if !ok {
		t.Fatalf("servers type = %T, want []map[string]any", withOwner["servers"])
	}
	if len(servers) != 1 {
		t.Fatalf("len(servers) = %d, want 1", len(servers))
	}
	if got, ok := servers[0]["owner_generation"].(string); !ok || got == "" {
		t.Fatalf("owner_generation = %#v, want non-empty string", servers[0]["owner_generation"])
	}
	if got := servers[0]["restore_source"]; got != "fresh" {
		t.Fatalf("restore_source = %#v, want fresh", got)
	}

	if err := d.Remove(sid); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}

	afterRemove := d.HandleStatus()
	assertOwnerRemovalStatus(t, afterRemove, 1, "operator_hard", 1)
}

func assertHandoffRestoredCount(t *testing.T, status map[string]any, want uint64) {
	t.Helper()
	handoff, ok := status["handoff"].(map[string]any)
	if !ok {
		t.Fatalf("handoff type = %T, want map[string]any", status["handoff"])
	}
	if got := uint64Status(t, handoff, "restored_owner_count"); got != want {
		t.Fatalf("handoff.restored_owner_count = %d, want %d", got, want)
	}
}

func assertOwnerRemovalStatus(t *testing.T, status map[string]any, wantTotal uint64, reason string, wantReason uint64) {
	t.Helper()
	ownerRemoval, ok := status["owner_removal"].(map[string]any)
	if !ok {
		t.Fatalf("owner_removal type = %T, want map[string]any", status["owner_removal"])
	}
	if got := uint64Status(t, ownerRemoval, "total"); got != wantTotal {
		t.Fatalf("owner_removal.total = %d, want %d", got, wantTotal)
	}
	byReason, ok := ownerRemoval["by_reason"].(map[string]uint64)
	if !ok {
		t.Fatalf("owner_removal.by_reason type = %T, want map[string]uint64", ownerRemoval["by_reason"])
	}
	if got := byReason[reason]; got != wantReason {
		t.Fatalf("owner_removal.by_reason[%q] = %d, want %d", reason, got, wantReason)
	}
	_ = uint64Status(t, ownerRemoval, "pending_tokens_removed")
	_ = uint64Status(t, ownerRemoval, "bound_history_removed")
}

func uint64Status(t *testing.T, status map[string]any, key string) uint64 {
	t.Helper()
	switch got := status[key].(type) {
	case uint64:
		return got
	case int:
		if got < 0 {
			t.Fatalf("%s = %d, want non-negative", key, got)
		}
		return uint64(got)
	default:
		t.Fatalf("%s type = %T, want uint64-compatible", key, status[key])
		return 0
	}
}
