package mux

import (
	"testing"
	"time"
)

func TestBusyProtocol_RegisterAndClear(t *testing.T) {
	o := newMinimalOwner()

	if o.HasActiveBusyWork() {
		t.Fatal("fresh owner should have no busy work")
	}

	o.RegisterBusy("job-1", time.Now(), 5*time.Minute, "test task", -1)
	if !o.HasActiveBusyWork() {
		t.Fatal("expected busy after register")
	}

	o.ClearBusy("job-1")
	if o.HasActiveBusyWork() {
		t.Fatal("expected idle after clear")
	}
}

func TestBusyProtocol_HardExpiresAt(t *testing.T) {
	o := newMinimalOwner()

	// Tiny estimated duration → hard cap = 2ms. Register a declaration
	// that started 10ms ago; it should already be expired.
	past := time.Now().Add(-10 * time.Millisecond)
	o.RegisterBusy("stale", past, 1*time.Millisecond, "", -1)

	// HasActiveBusyWork must GC the expired entry and return false.
	if o.HasActiveBusyWork() {
		t.Fatal("expected stale busy entry to be gc'd")
	}
	o.busyMu.Lock()
	remaining := len(o.busyDeclarations)
	o.busyMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected gc'd map, got %d entries", remaining)
	}
}

func TestBusyProtocol_DefaultDuration(t *testing.T) {
	o := newMinimalOwner()
	// Zero estimated duration → 10 minutes default.
	o.RegisterBusy("job-a", time.Time{}, 0, "", -1)

	o.busyMu.Lock()
	d, ok := o.busyDeclarations["job-a"]
	o.busyMu.Unlock()
	if !ok {
		t.Fatal("declaration not registered")
	}
	if d.HardExpiresAt.IsZero() {
		t.Fatal("HardExpiresAt should be set")
	}
	// Hard cap = default duration * 2 = 20 minutes.
	expected := 20 * time.Minute
	actual := d.HardExpiresAt.Sub(d.StartedAt)
	if actual < expected-time.Second || actual > expected+time.Second {
		t.Errorf("hard cap want ~%s, got %s", expected, actual)
	}
}

func TestBusyProtocol_ParseBusyNotification(t *testing.T) {
	o := newMinimalOwner()

	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/busy","params":{"id":"job-42","estimatedDurationMs":60000,"task":"long inference"}}`)
	o.handleBusyNotification(raw)

	if !o.HasActiveBusyWork() {
		t.Fatal("expected busy after handleBusyNotification")
	}

	o.busyMu.Lock()
	d := o.busyDeclarations["job-42"]
	o.busyMu.Unlock()
	if d.Task != "long inference" {
		t.Errorf("task want %q, got %q", "long inference", d.Task)
	}
	// estimatedDurationMs=60000 → hard cap 120_000 ms from StartedAt
	span := d.HardExpiresAt.Sub(d.StartedAt)
	if span < 119*time.Second || span > 121*time.Second {
		t.Errorf("hard cap want ~120s, got %s", span)
	}
}

func TestBusyProtocol_ParseIdleNotification(t *testing.T) {
	o := newMinimalOwner()
	o.RegisterBusy("cleanup-me", time.Now(), time.Hour, "", -1)

	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/idle","params":{"id":"cleanup-me"}}`)
	o.handleIdleNotification(raw)

	if o.HasActiveBusyWork() {
		t.Fatal("expected idle after handleIdleNotification")
	}
}

func TestBusyProtocol_MissingIDIgnored(t *testing.T) {
	o := newMinimalOwner()
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/busy","params":{}}`)
	o.handleBusyNotification(raw)
	if o.HasActiveBusyWork() {
		t.Fatal("expected empty-id notification to be ignored")
	}
}

func TestBusyProtocol_DurationCappedAt24h(t *testing.T) {
	o := newMinimalOwner()

	// RegisterBusy with a duration far above 24 h — must be clamped.
	o.RegisterBusy("long-job", time.Now(), 48*time.Hour, "should cap", -1)

	o.busyMu.Lock()
	d, ok := o.busyDeclarations["long-job"]
	o.busyMu.Unlock()
	if !ok {
		t.Fatal("declaration not found")
	}
	actual := d.HardExpiresAt.Sub(d.StartedAt)
	// Clamped to 24 h → hard cap = 24 h * 2 = 48 h
	want := 48 * time.Hour
	if actual < want-time.Second || actual > want+time.Second {
		t.Errorf("hard cap want ~%s, got %s", want, actual)
	}
}

func TestBusyProtocol_NotificationDurationCappedAt24h(t *testing.T) {
	o := newMinimalOwner()

	// estimatedDurationMs = 200_000_000 (> 86_400_000 ms = 24 h) must be clamped.
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/busy","params":{"id":"huge","estimatedDurationMs":200000000}}`)
	o.handleBusyNotification(raw)

	o.busyMu.Lock()
	d, ok := o.busyDeclarations["huge"]
	o.busyMu.Unlock()
	if !ok {
		t.Fatal("declaration not found")
	}
	actual := d.HardExpiresAt.Sub(d.StartedAt)
	// Clamped to 24 h → hard cap = 48 h
	want := 48 * time.Hour
	if actual < want-time.Second || actual > want+time.Second {
		t.Errorf("notification hard cap want ~%s, got %s", want, actual)
	}
}
