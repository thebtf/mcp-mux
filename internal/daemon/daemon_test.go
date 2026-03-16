package daemon

import (
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bitswan-space/mcp-mux/internal/control"
)

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(os.Stderr, "[daemon-test] ", log.LstdFlags)
}

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	ctlPath := filepath.Join(t.TempDir(), "daemon.ctl.sock")
	d, err := New(Config{
		ControlPath: ctlPath,
		GracePeriod: 1 * time.Second,
		IdleTimeout: 5 * time.Second,
		Logger:      testLogger(t),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	return d
}

func TestDaemonSpawnAndStatus(t *testing.T) {
	d := testDaemon(t)

	ipcPath, sid, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Cwd:     "",
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}
	if ipcPath == "" {
		t.Error("Spawn() returned empty ipcPath")
	}
	if sid == "" {
		t.Error("Spawn() returned empty serverID")
	}

	if d.OwnerCount() != 1 {
		t.Errorf("OwnerCount() = %d, want 1", d.OwnerCount())
	}

	// Status should include the server
	status := d.HandleStatus()
	if status["owner_count"] != 1 {
		t.Errorf("status owner_count = %v, want 1", status["owner_count"])
	}
	if !status["daemon"].(bool) {
		t.Error("status daemon should be true")
	}
}

func TestDaemonSpawnReusesExisting(t *testing.T) {
	d := testDaemon(t)

	req := control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	}

	ipc1, sid1, err := d.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn() 1 error: %v", err)
	}

	ipc2, sid2, err := d.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn() 2 error: %v", err)
	}

	if ipc1 != ipc2 {
		t.Errorf("second spawn returned different ipcPath: %s vs %s", ipc1, ipc2)
	}
	if sid1 != sid2 {
		t.Errorf("second spawn returned different serverID: %s vs %s", sid1, sid2)
	}
	if d.OwnerCount() != 1 {
		t.Errorf("OwnerCount() = %d, want 1 (should reuse)", d.OwnerCount())
	}
}

func TestDaemonRemove(t *testing.T) {
	d := testDaemon(t)

	_, sid, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	if err := d.Remove(sid); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}

	if d.OwnerCount() != 0 {
		t.Errorf("OwnerCount() = %d after Remove, want 0", d.OwnerCount())
	}

	// Remove again should fail
	if err := d.Remove(sid); err == nil {
		t.Error("Remove() should fail for non-existent server")
	}
}

func TestDaemonShutdownCleansAll(t *testing.T) {
	d := testDaemon(t)

	for i := 0; i < 3; i++ {
		_, _, err := d.Spawn(control.Request{
			Cmd:     "spawn",
			Command: "go",
			Args:    []string{"run", "../../testdata/mock_server.go"},
			Mode:    "isolated",
		})
		if err != nil {
			t.Fatalf("Spawn() %d error: %v", i, err)
		}
	}

	if d.OwnerCount() != 3 {
		t.Fatalf("OwnerCount() = %d, want 3", d.OwnerCount())
	}

	d.Shutdown()

	select {
	case <-d.Done():
		// ok
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown() did not complete in time")
	}

	if d.OwnerCount() != 0 {
		t.Errorf("OwnerCount() = %d after Shutdown, want 0", d.OwnerCount())
	}
}

func TestDaemonSetPersistent(t *testing.T) {
	d := testDaemon(t)

	_, sid, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	d.SetPersistent(sid, true)

	entry := d.Entry(sid)
	if entry == nil {
		t.Fatal("Entry() returned nil")
	}
	if !entry.Persistent {
		t.Error("Persistent should be true after SetPersistent(true)")
	}
}
