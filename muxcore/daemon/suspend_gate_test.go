package daemon

import (
	"errors"
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
