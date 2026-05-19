package daemon

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func testDaemonWithReaper(t *testing.T, grace, idle time.Duration) (*Daemon, *Reaper) {
	t.Helper()
	ctlPath := shortSocketPath(t, "daemon.ctl.sock")
	d, err := New(Config{
		Name:        "test-daemon",
		ControlPath: ctlPath,
		GracePeriod: grace,
		IdleTimeout: idle,
		Logger:      log.New(os.Stderr, "[reaper-test] ", log.LstdFlags),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	r := NewReaper(d, 200*time.Millisecond) // fast sweep for tests
	t.Cleanup(func() {
		r.Stop()
		d.Shutdown()
	})
	return d, r
}

func TestReaperGracePeriodExpiry(t *testing.T) {
	d, _ := testDaemonWithReaper(t, 500*time.Millisecond, 1*time.Minute)

	_, sid, _, err := d.Spawn(control.Request{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	// Simulate zero sessions by setting LastSession to the past
	d.mu.Lock()
	if entry, ok := d.owners[sid]; ok {
		entry.LastSession = time.Now().Add(-2 * time.Second)
	}
	d.mu.Unlock()

	// Wait for reaper to sweep
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("reaper did not remove owner within timeout (count=%d)", d.OwnerCount())
		default:
			if d.OwnerCount() == 0 {
				return // success
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func TestReaperPersistentSurvivesGrace(t *testing.T) {
	d, _ := testDaemonWithReaper(t, 500*time.Millisecond, 1*time.Minute)

	_, sid, _, err := d.Spawn(control.Request{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	d.SetPersistent(sid, true)

	// Set LastSession to the past
	d.mu.Lock()
	if entry, ok := d.owners[sid]; ok {
		entry.LastSession = time.Now().Add(-2 * time.Second)
	}
	d.mu.Unlock()

	// Wait a few sweep cycles — persistent owner should survive
	time.Sleep(1 * time.Second)

	if d.OwnerCount() != 1 {
		t.Errorf("persistent owner was removed, OwnerCount() = %d", d.OwnerCount())
	}
}

func TestReaperRespectsConfigPersistent(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "[reaper-test] ", 0)
	ctlPath := shortSocketPath(t, "persistent.ctl.sock")
	d, err := New(Config{
		Name:             "test-daemon",
		ControlPath:      ctlPath,
		IdleTimeout:      5 * time.Second,
		OwnerIdleTimeout: 100 * time.Millisecond,
		SkipSnapshot:     true,
		Logger:           logger,
		Persistent:       true,
		SessionHandler:   noopSessionHandler{},
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

	d.mu.Lock()
	entry := d.owners[sid]
	if entry == nil {
		d.mu.Unlock()
		t.Fatal("owner entry not found after Spawn")
	}
	entry.LastSession = time.Now().Add(-1 * time.Second)
	d.mu.Unlock()

	time.Sleep(150 * time.Millisecond)

	r := &Reaper{daemon: d, logger: logger}
	if affected := r.sweep(); affected != 0 {
		t.Fatalf("sweep() affected %d owners, want 0 for persistent owner", affected)
	}
	if d.OwnerCount() != 1 {
		t.Fatalf("OwnerCount() = %d after sweep, want 1", d.OwnerCount())
	}
	if got := d.Entry(sid); got == nil || !got.Persistent {
		t.Fatal("persistent owner lost after sweep")
	}
	if logOutput := logs.String(); strings.Contains(logOutput, "soft-removing") || strings.Contains(logOutput, "upstream dead with 0 sessions, removing") {
		t.Fatalf("reaper log indicates eviction for persistent owner: %s", logOutput)
	}
}

func TestReaperIdleAutoExit(t *testing.T) {
	d, _ := testDaemonWithReaper(t, 100*time.Millisecond, 500*time.Millisecond)

	// No owners spawned — daemon should auto-exit after idle timeout
	select {
	case <-d.Done():
		// success — daemon auto-exited
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not auto-exit after idle timeout")
	}
}
