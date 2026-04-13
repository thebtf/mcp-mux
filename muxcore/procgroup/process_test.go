package procgroup

import (
	"runtime"
	"testing"
	"time"
)

// longRunningCmd returns Options for a process that runs ~999 seconds and must
// be explicitly killed. On Windows we use ping (which sleeps between ICMP
// packets) because "sleep" is not a native command.
func longRunningCmd() Options {
	if runtime.GOOS == "windows" {
		return Options{Command: "cmd", Args: []string{"/c", "ping -n 999 127.0.0.1 >nul"}}
	}
	return Options{Command: "sleep", Args: []string{"999"}}
}

// treeCmd returns Options for a process that itself spawns children.
// On Windows the Job Object kills all members automatically so a single-process
// kill is sufficient to verify tree-kill semantics; we therefore reuse
// longRunningCmd. On Unix we spawn two sleep sub-processes and wait for them.
func treeCmd() Options {
	if runtime.GOOS == "windows" {
		return longRunningCmd()
	}
	return Options{Command: "sh", Args: []string{"-c", "sleep 999 & sleep 999 & wait"}}
}

// TestSpawn_StartsProcess verifies that Spawn returns a live process with PID > 0.
func TestSpawn_StartsProcess(t *testing.T) {
	t.Parallel()

	p, err := Spawn(longRunningCmd())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	if p.PID() <= 0 {
		t.Errorf("expected PID > 0, got %d", p.PID())
	}
	if !p.Alive() {
		t.Error("expected Alive() == true immediately after spawn")
	}
}

// TestGracefulKill_KillsTree spawns a process (with children on Unix), calls
// GracefulKill, and verifies the process tree is dead within a short timeout.
func TestGracefulKill_KillsTree(t *testing.T) {
	t.Parallel()

	p, err := Spawn(treeCmd())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	const gracePeriod = 3 * time.Second
	if err := p.GracefulKill(gracePeriod); err != nil {
		t.Fatalf("GracefulKill: %v", err)
	}

	select {
	case <-p.Done():
		// process exited — good
	case <-time.After(gracePeriod + 2*time.Second):
		t.Fatal("process did not exit after GracefulKill + extra grace")
	}

	if p.Alive() {
		t.Error("expected Alive() == false after GracefulKill")
	}
}

// TestKill_Immediate verifies that Kill() terminates the process quickly.
func TestKill_Immediate(t *testing.T) {
	t.Parallel()

	p, err := Spawn(longRunningCmd())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	start := time.Now()
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit within 5 s after Kill")
	}

	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("Kill took too long: %v", elapsed)
	}
}

// TestDone_ClosesAfterKill verifies that the Done() channel is closed once the
// process is killed.
func TestDone_ClosesAfterKill(t *testing.T) {
	t.Parallel()

	p, err := Spawn(longRunningCmd())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	doneCh := p.Done()

	// Channel must not be closed yet.
	select {
	case <-doneCh:
		t.Fatal("Done() closed before Kill was called")
	default:
	}

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case <-doneCh:
		// Expected: channel closed after kill.
	case <-time.After(5 * time.Second):
		t.Fatal("Done() channel did not close within 5 s after Kill")
	}
}

// TestAlive_FalseAfterKill verifies that Alive() returns false after Kill().
func TestAlive_FalseAfterKill(t *testing.T) {
	t.Parallel()

	p, err := Spawn(longRunningCmd())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Wait for the reaper goroutine to record the exit.
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit within 5 s after Kill")
	}

	if p.Alive() {
		t.Error("expected Alive() == false after Kill")
	}
}

// TestSpawn_InvalidCommand verifies that Spawn returns an error for a
// non-existent command and does not panic or block.
func TestSpawn_InvalidCommand(t *testing.T) {
	t.Parallel()

	_, err := Spawn(Options{Command: "this-command-does-not-exist-at-all-9f3a2b1c"})
	if err == nil {
		t.Error("expected error spawning non-existent command, got nil")
	}
}
