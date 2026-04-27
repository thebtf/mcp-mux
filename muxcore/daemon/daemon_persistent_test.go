package daemon

import (
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func TestSessionHandlerOwnerInheritsPersistent(t *testing.T) {
	ctlPath := shortSocketPath(t, "persist.ctl.sock")
	d, err := New(Config{
		ControlPath:    ctlPath,
		GracePeriod:    1 * time.Second,
		IdleTimeout:    5 * time.Second,
		SkipSnapshot:   true,
		Logger:         testLogger(t),
		Persistent:     true,
		SessionHandler: noopSessionHandler{},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	_, sid, _, err := d.Spawn(control.Request{
		Cmd:  "spawn",
		Args: []string{t.Name()},
		Mode: "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	d.mu.RLock()
	entry := d.owners[sid]
	d.mu.RUnlock()

	if entry == nil {
		t.Fatal("owner entry not found after Spawn")
	}
	if !entry.Persistent {
		t.Errorf("OwnerEntry.Persistent = false, want true (d.cfg.Persistent=true should propagate)")
	}
}
