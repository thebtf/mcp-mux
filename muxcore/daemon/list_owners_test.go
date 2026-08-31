package daemon

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
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
		Persistent:                  true,
		OwnerGeneration:             "private-modern-generation",
		RestoredFromOwnerGeneration: "private-predecessor-generation",
		RestoreSource:               "snapshot_fallback",
	}
	d.owners[legacyID] = &OwnerEntry{
		Owner:           legacy,
		ServerID:        legacyID,
		ProtocolEra:     era.EraLegacy,
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
}

func TestOwnerInfo_ModernPolicySchemaAndZeroJSONOmission(t *testing.T) {
	typ := reflect.TypeOf(control.OwnerInfo{})
	wantTags := map[string]string{
		"ServerID":             "server_id",
		"EngineName":           "engine_name,omitempty",
		"Command":              "command",
		"Args":                 "args",
		"Cwd":                  "cwd",
		"CwdSet":               "cwd_set",
		"Sessions":             "sessions",
		"Pending":              "pending",
		"UpstreamPID":          "upstream_pid,omitempty",
		"Classification":       "classification",
		"ClassificationSource": "classification_source,omitempty",
		"ClassificationReason": "classification_reason,omitempty",
		"MuxVersion":           "mux_version",
		"Persistent":           "persistent",
		"CachedInit":           "cached_init,omitempty",
		"CachedTools":          "cached_tools,omitempty",
		"CachedPrompts":        "cached_prompts,omitempty",
		"CachedResources":      "cached_resources,omitempty",
		"ProtocolEra":          "protocol_era,omitempty",
		"SharingPolicy":        "sharing_policy,omitempty",
		"CachePolicy":          "cache_policy,omitempty",
		"LifecyclePolicy":      "lifecycle_policy,omitempty",
	}
	if typ.NumField() != len(wantTags) {
		t.Errorf("OwnerInfo field count = %d, want exactly %d", typ.NumField(), len(wantTags))
	}
	for name, wantTag := range wantTags {
		field, found := typ.FieldByName(name)
		if !found {
			t.Errorf("OwnerInfo missing required field %s", name)
			continue
		}
		if name == "ProtocolEra" || name == "SharingPolicy" || name == "CachePolicy" || name == "LifecyclePolicy" {
			if field.Type.Kind() != reflect.String {
				t.Errorf("OwnerInfo.%s type = %s, want string", name, field.Type)
			}
		}
		if got := field.Tag.Get("json"); got != wantTag {
			t.Errorf("OwnerInfo.%s json tag = %q, want %q", name, got, wantTag)
		}
	}
	for i := range typ.NumField() {
		if _, allowed := wantTags[typ.Field(i).Name]; !allowed {
			t.Errorf("OwnerInfo unexpectedly exposes prohibited/R3 field %s", typ.Field(i).Name)
		}
	}

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

	var roundTrip control.OwnerInfo
	if err := json.Unmarshal(zeroWire, &roundTrip); err != nil {
		t.Fatalf("unmarshal zero OwnerInfo roundtrip: %v", err)
	}
	roundTripWire, err := json.Marshal(roundTrip)
	if err != nil {
		t.Fatalf("marshal zero OwnerInfo roundtrip: %v", err)
	}
	for _, key := range modernOwnerPolicyKeys {
		if stringFieldPresent(t, typ, roundTrip, key) {
			t.Errorf("zero OwnerInfo roundtrip populated %q", key)
		}
		if containsJSONKey(roundTripWire, key) {
			t.Errorf("zero OwnerInfo roundtrip JSON contains %q: %s", key, roundTripWire)
		}
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

func stringFieldPresent(t *testing.T, typ reflect.Type, value control.OwnerInfo, jsonKey string) bool {
	t.Helper()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Tag.Get("json") != jsonKey+",omitempty" {
			continue
		}
		return reflect.ValueOf(value).Field(i).String() != ""
	}
	return false
}

func containsJSONKey(wire []byte, key string) bool {
	var fields map[string]json.RawMessage
	return json.Unmarshal(wire, &fields) == nil && fields[key] != nil
}
