package upstream

import (
	"runtime"
	"testing"
	"time"
)

// TestSoftClose_UpstreamHonorsStdinClose verifies that an upstream which exits
// cleanly on stdin close (the happy path) results in exitCode 0 and no SIGKILL.
// echo_pipe.go reads stdin and exits with code 0 when stdin closes — perfect
// for demonstrating the polite-shutdown path (US3-AC1).
func TestSoftClose_UpstreamHonorsStdinClose(t *testing.T) {
	// echo_pipe.go exits cleanly on stdin EOF (scan loop returns false on EOF).
	p, err := Start("go", []string{"run", "../../testdata/echo_pipe.go"}, nil, "", nil)
	if err != nil {
		t.Skipf("cannot start echo_pipe.go: %v", err)
	}

	exitCode, err := p.SoftClose(10 * time.Second)
	if err != nil {
		t.Fatalf("SoftClose() error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("SoftClose() exitCode = %d, want 0 (upstream should exit cleanly on stdin close)", exitCode)
	}

	// Process must be done within a short grace period.
	select {
	case <-p.Done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("process.Done not closed after SoftClose")
	}
}

// TestSoftClose_UpstreamIgnoresStdinClose_FallsBackToKill verifies that when an
// upstream ignores stdin close and keeps running, SoftClose falls back to
// GracefulKill after the timeout. The process must be dead after SoftClose returns.
func TestSoftClose_UpstreamIgnoresStdinClose_FallsBackToKill(t *testing.T) {
	// sleep/timeout ignores stdin — it will never exit on stdin close alone.
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "timeout", "/t", "30", "/nobreak"}
	} else {
		cmd = "sleep"
		args = []string{"30"}
	}

	p, err := Start(cmd, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// 200ms timeout — process won't exit voluntarily; GracefulKill fallback fires.
	_, err = p.SoftClose(200 * time.Millisecond)
	if err != nil {
		t.Fatalf("SoftClose() fallback kill error: %v (GracefulKill should succeed)", err)
	}

	// Process must be terminated after the forced kill path.
	select {
	case <-p.Done:
		// good — process was killed
	case <-time.After(10 * time.Second):
		t.Fatal("process not done after SoftClose forced kill path")
	}
}
