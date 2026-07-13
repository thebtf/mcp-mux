package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func TestHandleCanSuspendSafetyGates(t *testing.T) {
	d := testDaemon(t)
	sid := "owner-suspend-gate"
	o := testReconnectOwner(t, sid)
	seedReconnectHistory(t, o, "suspend-token", "/project/suspend", nil)

	d.mu.Lock()
	d.owners[sid] = &OwnerEntry{Owner: o, ServerID: sid}
	d.mu.Unlock()

	got, err := d.HandleCanSuspend("suspend-token")
	if err != nil || !got.Allowed {
		t.Fatalf("HandleCanSuspend idle = (%+v, %v), want allowed", got, err)
	}

	d.mu.Lock()
	d.owners[sid].Persistent = true
	d.mu.Unlock()
	got, err = d.HandleCanSuspend("suspend-token")
	if err != nil || got.Allowed || got.Reason != "persistent" {
		t.Fatalf("HandleCanSuspend persistent = (%+v, %v)", got, err)
	}

	d.mu.Lock()
	d.owners[sid].Persistent = false
	d.mu.Unlock()
	o.RegisterBusy("background", time.Now(), time.Minute, "test", -1)
	got, err = d.HandleCanSuspend("suspend-token")
	if err != nil || got.Allowed || got.Reason != "busy" {
		t.Fatalf("HandleCanSuspend busy = (%+v, %v)", got, err)
	}
}

func TestHandleCanSuspendUnknownToken(t *testing.T) {
	d := testDaemon(t)
	if _, err := d.HandleCanSuspend("missing"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("HandleCanSuspend error = %v, want ErrUnknownToken", err)
	}
}

func TestHandleCanSuspendForOwnerUsesExactOwnerWithoutFanout(t *testing.T) {
	d := testDaemon(t)
	const targetSID = "owner-suspend-target"
	target := testReconnectOwner(t, targetSID)
	seedReconnectHistory(t, target, "suspend-target-token", "/project/suspend", nil)
	unrelatedSID := "owner-suspend-unrelated"
	unrelated := testReconnectOwner(t, unrelatedSID)
	seedReconnectHistory(t, unrelated, "unrelated-token", "/project/unrelated", nil)

	d.mu.Lock()
	d.owners[targetSID] = &OwnerEntry{Owner: target, ServerID: targetSID}
	d.owners[unrelatedSID] = &OwnerEntry{Owner: unrelated, ServerID: unrelatedSID}
	d.mu.Unlock()
	var lookups []string
	d.lookupReconnectHistory = func(o *owner.Owner, token string) (string, string, map[string]string, bool) {
		lookups = append(lookups, o.ServerID())
		return o.SessionMgr().LookupHistory(token)
	}

	got, err := d.HandleCanSuspendForOwner("suspend-target-token", targetSID)
	if err != nil || !got.Allowed {
		t.Fatalf("HandleCanSuspendForOwner target = (%+v, %v), want allowed", got, err)
	}
	if len(lookups) != 1 || lookups[0] != targetSID {
		t.Fatalf("exact-owner lookups = %v, want only %q", lookups, targetSID)
	}

	lookups = nil
	if _, err := d.HandleCanSuspendForOwner("suspend-target-token", unrelatedSID); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("HandleCanSuspendForOwner forged owner error = %v, want ErrUnknownToken", err)
	}
	if len(lookups) != 1 || lookups[0] != unrelatedSID {
		t.Fatalf("forged-owner lookups = %v, want only %q", lookups, unrelatedSID)
	}

	// A recreated owner with the same ServerID must not inherit the removed
	// owner's token history and authorize its stale shim.
	recreated := testReconnectOwner(t, targetSID)
	d.mu.Lock()
	d.owners[targetSID] = &OwnerEntry{Owner: recreated, ServerID: targetSID}
	d.mu.Unlock()
	lookups = nil
	if _, err := d.HandleCanSuspendForOwner("suspend-target-token", targetSID); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("recreated owner accepted stale token: %v", err)
	}
	if len(lookups) != 1 || lookups[0] != targetSID {
		t.Fatalf("recreated-owner lookups = %v, want only %q", lookups, targetSID)
	}
}
