package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

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
