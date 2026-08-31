package daemon

import (
	"reflect"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func TestHandleStatus_StableOperatorContract(t *testing.T) {
	d := testDaemon(t)

	initial := d.HandleStatus()
	if got := initial["engine_name"]; got != "test-daemon" {
		t.Fatalf("engine_name = %#v, want test-daemon", got)
	}
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

var modernOwnerPolicyFacts = map[string]string{
	"protocol_era":     "2026-07-28",
	"sharing_policy":   "forced-isolated",
	"cache_policy":     "off",
	"lifecycle_policy": "r1-quarantine",
}

var modernOwnerPolicyKeys = []string{
	"protocol_era",
	"sharing_policy",
	"cache_policy",
	"lifecycle_policy",
}

var modernServerProhibitedKeys = []string{
	"sessions",
	"inflight",
	"oldest_request_age_ms",
	"finalization_error",
	"owner_generation",
	"restored_from_owner_generation",
	"restore_source",
	"mux_engines",
	"topology",
	"registry",
	"registry_descriptor",
	"taxonomy",
	"counter",
	"counters",
	"logging",
	"logs",
}

var preservedModernOwnerStatusKeys = []string{
	"command",
	"args",
	"cwd",
	"cwd_set",
	"mux_version",
	"upstream_pid",
	"session_count",
	"pending_requests",
	"cached_init",
	"cached_tools",
	"cached_prompts",
	"cached_resources",
	"materialization_state",
	"materialization_trigger",
	"materialization_policy",
	"materialization_generation",
	"pending_demand_count",
	"persistent_pending",
	"restart_pin_count",
	"cache_ready",
	"upstream_live",
}

func TestHandleStatus_ModernOwnerPolicyFactsAndRedaction(t *testing.T) {
	d := testDaemon(t)
	const sid = "modern-status-policy-contract"
	o := newStatusContractOwner(t, sid, era.EraModern20260728)

	d.mu.Lock()
	d.owners[sid] = &OwnerEntry{
		Owner:                       o,
		ServerID:                    sid,
		Command:                     "entry-command-must-not-replace-owner-status",
		Args:                        []string{"--entry-arg"},
		Cwd:                         "/entry-cwd",
		ProtocolEra:                 era.EraModern20260728,
		Persistent:                  true,
		OwnerGeneration:             "private-modern-generation",
		RestoredFromOwnerGeneration: "private-predecessor-generation",
		RestoreSource:               "snapshot_fallback",
	}
	d.mu.Unlock()

	ownerStatus := o.Status()
	server := statusServerByID(t, d.HandleStatus(), sid)

	assertModernPolicyFacts(t, "owner.Status", ownerStatus)
	assertModernPolicyFacts(t, "daemon.HandleStatus server", server)
	assertStatusKeysMatch(t, "daemon.HandleStatus server", server, ownerStatus, preservedModernOwnerStatusKeys)
	if got := server["persistent"]; got != true {
		t.Errorf("daemon.HandleStatus server persistent = %#v, want true", got)
	}
	assertStatusKeysAbsent(t, "daemon.HandleStatus modern server", server, modernServerProhibitedKeys)
}

func TestHandleStatus_LegacyOwnerOmitsModernPolicyFacts(t *testing.T) {
	d := testDaemon(t)
	const sid = "legacy-status-policy-contract"
	o := newStatusContractOwner(t, sid, era.EraLegacy)

	d.mu.Lock()
	d.owners[sid] = &OwnerEntry{
		Owner:           o,
		ServerID:        sid,
		ProtocolEra:     era.EraLegacy,
		OwnerGeneration: "legacy-generation",
		RestoreSource:   "fresh",
	}
	d.mu.Unlock()

	server := statusServerByID(t, d.HandleStatus(), sid)
	assertStatusKeysAbsent(t, "daemon.HandleStatus legacy server", server, modernOwnerPolicyKeys)
	if got := server["owner_generation"]; got != "legacy-generation" {
		t.Errorf("legacy owner_generation = %#v, want legacy-generation", got)
	}
	if got := server["restore_source"]; got != "fresh" {
		t.Errorf("legacy restore_source = %#v, want fresh", got)
	}
	if _, found := server["sessions"]; !found {
		t.Error("legacy server omitted existing sessions detail")
	}
}

func newStatusContractOwner(t *testing.T, sid string, protocolEra era.ProtocolEra) *owner.Owner {
	t.Helper()
	o, err := owner.NewOwner(owner.OwnerConfig{
		Command:        "policy-contract-server",
		Args:           []string{"--policy-contract"},
		Cwd:            "/owner-cwd",
		IPCPath:        shortSocketPath(t, "policy-contract.sock"),
		ServerID:       sid,
		ProtocolEra:    protocolEra,
		SessionHandler: noopSessionHandler{},
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	t.Cleanup(func() { o.Shutdown() })
	return o
}

func statusServerByID(t *testing.T, status map[string]any, sid string) map[string]any {
	t.Helper()
	servers, ok := status["servers"].([]map[string]any)
	if !ok {
		t.Fatalf("status servers type = %T, want []map[string]any", status["servers"])
	}
	for _, server := range servers {
		if server["server_id"] == sid {
			return server
		}
	}
	t.Fatalf("status missing server_id %q: %#v", sid, servers)
	return nil
}

func assertModernPolicyFacts(t *testing.T, label string, got map[string]any) {
	t.Helper()
	for key, want := range modernOwnerPolicyFacts {
		if actual, found := got[key]; !found || actual != want {
			t.Errorf("%s %s = %#v (present=%v), want %q", label, key, actual, found, want)
		}
	}
}

func assertStatusKeysMatch(t *testing.T, label string, got, want map[string]any, keys []string) {
	t.Helper()
	for _, key := range keys {
		actual, gotFound := got[key]
		expected, wantFound := want[key]
		if !gotFound || !wantFound || !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s %s = %#v (present=%v), owner.Status = %#v (present=%v)", label, key, actual, gotFound, expected, wantFound)
		}
	}
}

func assertStatusKeysAbsent(t *testing.T, label string, status map[string]any, keys []string) {
	t.Helper()
	for _, key := range keys {
		if value, found := status[key]; found {
			t.Errorf("%s leaked prohibited %s = %#v", label, key, value)
		}
	}
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
