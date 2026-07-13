package daemon

import (
	"errors"
	"fmt"
	"testing"
	"time"
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

	d.mu.Lock()
	d.owners[targetSID] = &OwnerEntry{Owner: target, ServerID: targetSID}
	for i := 0; i < 256; i++ {
		sid := fmt.Sprintf("unrelated-suspend-owner-%03d", i)
		d.owners[sid] = &OwnerEntry{ServerID: sid}
	}
	d.mu.Unlock()

	got, err := d.HandleCanSuspendForOwner("suspend-target-token", targetSID)
	if err != nil || !got.Allowed {
		t.Fatalf("HandleCanSuspendForOwner target = (%+v, %v), want allowed", got, err)
	}
	if _, err := d.HandleCanSuspendForOwner("suspend-target-token", "unrelated-suspend-owner-000"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("HandleCanSuspendForOwner forged owner error = %v, want ErrUnknownToken", err)
	}
}
