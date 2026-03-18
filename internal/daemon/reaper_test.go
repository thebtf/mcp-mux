package daemon

import (
	"log"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/internal/control"
)

func testDaemonWithReaper(t *testing.T, grace, idle time.Duration) (*Daemon, *Reaper) {
	t.Helper()
	ctlPath := shortSocketPath(t, "daemon.ctl.sock")
	d, err := New(Config{
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

	_, sid, err := d.Spawn(control.Request{
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

	_, sid, err := d.Spawn(control.Request{
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
