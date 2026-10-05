package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

var modernOwnerInfoProhibitedKeys = []string{
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

// TestHandleListOwners registers 3 in-process owners with distinct cwds, calls
// HandleListOwners, and asserts: count=3, truncated=false, sorted by server_id,
// all expected IDs present.
func TestHandleListOwners(t *testing.T) {
	d := testDaemon(t)

	// IDs chosen to have a deterministic alphabetical sort order: aaa... < bbb... < ccc...
	sids := []string{"aaa0aaa000000001", "bbb0bbb000000002", "ccc0ccc000000003"}
	cwds := []string{"/proj/alpha", "/proj/beta", "/proj/gamma"}

	for i, sid := range sids {
		ipcPath := shortSocketPath(t, "lo-"+sid[:4]+".sock")
		o, err := owner.NewOwner(owner.OwnerConfig{
			IPCPath:        ipcPath,
			ServerID:       sid,
			SessionHandler: noopSessionHandler{},
			Logger:         testLogger(t),
		})
		if err != nil {
			t.Fatalf("NewOwner %s: %v", sid, err)
		}
		capturedO := o
		t.Cleanup(func() { capturedO.Shutdown() })

		d.mu.Lock()
		d.owners[sid] = &OwnerEntry{
			Owner:    o,
			ServerID: sid,
			Command:  "test-cmd",
			Args:     []string{"--arg"},
			Cwd:      cwds[i],
		}
		d.mu.Unlock()
	}

	resp, err := d.HandleListOwners(control.Request{Cmd: "list_owners"})
	if err != nil {
		t.Fatalf("HandleListOwners error: %v", err)
	}

	if len(resp.Owners) != 3 {
		t.Errorf("want 3 owners, got %d", len(resp.Owners))
	}
	if resp.Truncated {
		t.Error("want truncated=false for 3 owners, got true")
	}

	// Assert sorted ascending by server_id.
	gotSIDs := make([]string, len(resp.Owners))
	for i, o := range resp.Owners {
		gotSIDs[i] = o.ServerID
	}
	if !sort.StringsAreSorted(gotSIDs) {
		t.Errorf("owners not sorted by server_id ascending: %v", gotSIDs)
	}

	// All expected SIDs must be present, and no owner may have an empty ServerID.
	sidSet := make(map[string]bool)
	for _, o := range resp.Owners {
		if o.ServerID == "" {
			t.Error("owner has empty ServerID")
		}
		if o.EngineName != "test-daemon" {
			t.Errorf("owner %s engine_name = %q, want test-daemon", o.ServerID, o.EngineName)
		}
		sidSet[o.ServerID] = true
	}
	for _, sid := range sids {
		if !sidSet[sid] {
			t.Errorf("missing server_id %s in response", sid)
		}
	}
}

// TestHandleListOwners_Truncated verifies that more than 200 owners causes
// len(resp.Owners)==200 and truncated=true.
func TestHandleListOwners_Truncated(t *testing.T) {
	d := testDaemon(t)

	// Create one real owner to share across all entries (Owner field must be non-nil
	// for entries to be included in list_owners output).
	ipcPath := shortSocketPath(t, "trunc.sock")
	sharedOwner, err := owner.NewOwner(owner.OwnerConfig{
		IPCPath:        ipcPath,
		ServerID:       "shared-trunc-owner",
		SessionHandler: noopSessionHandler{},
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	t.Cleanup(func() { sharedOwner.Shutdown() })

	d.mu.Lock()
	for i := 0; i < 201; i++ {
		sid := fmt.Sprintf("sid%06d000000000", i)
		d.owners[sid] = &OwnerEntry{
			Owner:    sharedOwner,
			ServerID: sid,
			Command:  "test-cmd",
		}
	}
	d.mu.Unlock()

	resp, err := d.HandleListOwners(control.Request{Cmd: "list_owners"})
	if err != nil {
		t.Fatalf("HandleListOwners error: %v", err)
	}
	if len(resp.Owners) != 200 {
		t.Errorf("want 200 owners (capped), got %d", len(resp.Owners))
	}
	if !resp.Truncated {
		t.Error("want truncated=true for 201 owners, got false")
	}
}

func TestStatusIntHandlesJSONNumber(t *testing.T) {
	if got := statusInt(json.Number("1234")); got != 1234 {
		t.Fatalf("statusInt(json.Number) = %d, want 1234", got)
	}
	if got := statusInt(json.Number("not-a-number")); got != 0 {
		t.Fatalf("statusInt(invalid json.Number) = %d, want 0", got)
	}
}

func TestHandleListOwners_ModernPolicyFactsMirrorDaemonStatus(t *testing.T) {
	d := testDaemon(t)
	const (
		modernID            = "modern-list-policy-contract"
		legacyID            = "legacy-list-policy-contract"
		modernPlaceholderID = "modern-list-placeholder"
		legacyPlaceholderID = "legacy-list-placeholder"
	)
	modern := newStatusContractOwner(t, modernID, era.EraModern20260728)
	legacy := newStatusContractOwner(t, legacyID, era.EraLegacy)

	d.mu.Lock()
	d.owners[modernID] = &OwnerEntry{
		Owner:                       modern,
		ServerID:                    modernID,
		Command:                     "entry-command-must-not-infer-policy",
		ProtocolEra:                 era.EraModern20260728,
		Env:                         map[string]string{"API_TOKEN": "modern-credential-sentinel"},
		Persistent:                  true,
		OwnerGeneration:             "private-modern-generation",
		RestoredFromOwnerGeneration: "private-predecessor-generation",
		RestoreSource:               "snapshot_fallback",
	}
	d.owners[legacyID] = &OwnerEntry{
		Owner:           legacy,
		ServerID:        legacyID,
		ProtocolEra:     era.EraLegacy,
		Env:             map[string]string{"API_TOKEN": "legacy-credential-sentinel"},
		OwnerGeneration: "legacy-generation",
		RestoreSource:   "fresh",
	}
	d.owners[modernPlaceholderID] = &OwnerEntry{
		ServerID:    modernPlaceholderID,
		ProtocolEra: era.EraModern20260728,
		Command:     "must-not-materialize-placeholder",
	}
	d.owners[legacyPlaceholderID] = &OwnerEntry{
		ServerID:    legacyPlaceholderID,
		ProtocolEra: era.EraLegacy,
		Command:     "must-not-fabricate-legacy-owner",
	}
	d.mu.Unlock()

	status := d.HandleStatus()
	modernStatus := statusServerByID(t, status, modernID)
	assertModernPolicyFacts(t, "daemon.HandleStatus modern server", modernStatus)
	assertStatusKeysAbsent(t, "daemon.HandleStatus modern server", modernStatus, modernServerProhibitedKeys)
	assertStatusOmitsServerIDs(t, status, modernPlaceholderID, legacyPlaceholderID)

	resp, err := d.HandleListOwners(control.Request{Cmd: "list_owners"})
	if err != nil {
		t.Fatalf("HandleListOwners() error: %v", err)
	}
	if len(resp.Owners) != 2 {
		t.Fatalf("HandleListOwners owners = %d, want 2 active owners", len(resp.Owners))
	}
	modernInfo := ownerInfoByServerID(t, resp.Owners, modernID)
	legacyInfo := ownerInfoByServerID(t, resp.Owners, legacyID)
	assertOwnersOmitServerIDs(t, resp.Owners, modernPlaceholderID, legacyPlaceholderID)

	modernWire := ownerInfoJSONMap(t, modernInfo)
	assertModernPolicyFacts(t, "HandleListOwners modern owner", modernWire)
	assertStatusKeysAbsent(t, "HandleListOwners modern owner", modernWire, modernOwnerInfoProhibitedKeys)
	assertOwnerInfoMatchesModernStatus(t, modernInfo, modernStatus)

	legacyWire := ownerInfoJSONMap(t, legacyInfo)
	assertStatusKeysAbsent(t, "HandleListOwners legacy owner", legacyWire, modernOwnerPolicyKeys)
	for label, fields := range map[string]map[string]any{"modern": modernWire, "legacy": legacyWire} {
		assertStatusKeysAbsent(t, "HandleListOwners "+label+" owner", fields, []string{"env", "token", "credentials", "authorization"})
	}
	wire, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal list owners response: %v", err)
	}
	for _, private := range []string{"modern-credential-sentinel", "legacy-credential-sentinel", "private-modern-generation", "private-predecessor-generation"} {
		if strings.Contains(string(wire), private) {
			t.Errorf("list owners response leaked private fixture value %q", private)
		}
	}
}

func TestOwnerInfo_ZeroJSONOmission(t *testing.T) {
	zeroWire, err := json.Marshal(control.OwnerInfo{})
	if err != nil {
		t.Fatalf("marshal zero OwnerInfo: %v", err)
	}
	var zeroFields map[string]json.RawMessage
	if err := json.Unmarshal(zeroWire, &zeroFields); err != nil {
		t.Fatalf("unmarshal zero OwnerInfo: %v", err)
	}
	for _, key := range modernOwnerPolicyKeys {
		if _, found := zeroFields[key]; found {
			t.Errorf("zero OwnerInfo JSON contains %q: %s", key, zeroWire)
		}
	}
	if _, found := zeroFields["maintenance"]; found {
		t.Errorf("zero OwnerInfo JSON contains maintenance: %s", zeroWire)
	}
}

func assertStatusOmitsServerIDs(t *testing.T, status map[string]any, serverIDs ...string) {
	t.Helper()
	servers, ok := status["servers"].([]map[string]any)
	if !ok {
		t.Fatalf("status servers type = %T, want []map[string]any", status["servers"])
	}
	for _, sid := range serverIDs {
		for _, server := range servers {
			if server["server_id"] == sid {
				t.Errorf("daemon.HandleStatus fabricated placeholder server %q: %#v", sid, server)
			}
		}
	}
}

func ownerInfoByServerID(t *testing.T, owners []control.OwnerInfo, sid string) control.OwnerInfo {
	t.Helper()
	for _, info := range owners {
		if info.ServerID == sid {
			return info
		}
	}
	t.Fatalf("HandleListOwners missing server_id %q: %#v", sid, owners)
	return control.OwnerInfo{}
}

func assertOwnersOmitServerIDs(t *testing.T, owners []control.OwnerInfo, serverIDs ...string) {
	t.Helper()
	for _, sid := range serverIDs {
		for _, info := range owners {
			if info.ServerID == sid {
				t.Errorf("HandleListOwners fabricated placeholder owner %q: %#v", sid, info)
			}
		}
	}
}

func ownerInfoJSONMap(t *testing.T, info control.OwnerInfo) map[string]any {
	t.Helper()
	wire, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal OwnerInfo: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatalf("unmarshal OwnerInfo: %v", err)
	}
	return fields
}

func assertOwnerInfoMatchesModernStatus(t *testing.T, info control.OwnerInfo, status map[string]any) {
	t.Helper()
	if got, want := info.Sessions, statusInt(status["session_count"]); got != want {
		t.Errorf("HandleListOwners sessions = %d, daemon status session_count = %d", got, want)
	}
	if got, want := info.Pending, statusInt(status["pending_requests"]); got != want {
		t.Errorf("HandleListOwners pending = %d, daemon status pending_requests = %d", got, want)
	}
	for _, field := range []struct {
		name string
		got  bool
		key  string
	}{
		{name: "cached_init", got: info.CachedInit, key: "cached_init"},
		{name: "cached_tools", got: info.CachedTools, key: "cached_tools"},
		{name: "cached_prompts", got: info.CachedPrompts, key: "cached_prompts"},
		{name: "cached_resources", got: info.CachedResources, key: "cached_resources"},
	} {
		want, ok := status[field.key].(bool)
		if !ok || field.got != want {
			t.Errorf("HandleListOwners %s = %v, daemon status %s = %#v", field.name, field.got, field.key, status[field.key])
		}
	}
	if got := info.Persistent; got != true {
		t.Errorf("HandleListOwners persistent = %v, want true", got)
	}
}
