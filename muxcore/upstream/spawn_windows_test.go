//go:build windows

package upstream

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestUpstreamJob_SurvivesDaemonJobClose verifies that closing the daemon's
// handle to the per-upstream Job Object does NOT kill the upstream process.
//
// Design (C1): The upstream process holds an implicit reference to its job
// via membership. JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE fires only when ALL
// handles close — not just the daemon's. So closing the daemon's handle
// while the process is alive leaves the process running.
//
// If this test fails (child dies on CloseHandle), KILL_ON_JOB_CLOSE fired
// prematurely — the nested job assignment or implicit-handle semantics are
// not behaving as expected on this OS version.
func TestUpstreamJob_SurvivesDaemonJobClose(t *testing.T) {
	// Spawn a long-running child via the upstream package. `ping` is used
	// instead of `timeout` because `timeout` refuses to run with redirected
	// stdin (which upstream.Start always uses) and exits immediately with
	// "ERROR: stdin redirect not supported". ping runs happily with stdin
	// redirected and sleeps ~30s with the given count.
	p, err := Start("ping", []string{"-n", "30", "127.0.0.1"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	pid := p.PID()
	if pid <= 0 {
		_ = p.proc.Kill()
		t.Fatalf("expected PID > 0, got %d", pid)
	}

	// Verify child is alive before the test exercises job semantics.
	select {
	case <-p.Done:
		t.Fatal("child exited before test started")
	default:
	}

	if p.jobHandle == 0 {
		// Job creation may have been skipped (nested jobs unsupported on this
		// OS version, or insufficient privilege). Skip rather than fail —
		// the feature degrades gracefully per AC8.
		_ = p.proc.Kill()
		t.Skip("jobHandle == 0: per-upstream job was not created (nested jobs unsupported or privilege error)")
	}

	// Simulate daemon releasing its job handle (as happens during graceful
	// handoff in T020). Save and zero the field so cleanup does not double-close.
	jobHandle := windows.Handle(p.jobHandle)
	p.jobHandle = 0

	if err := windows.CloseHandle(jobHandle); err != nil {
		_ = p.proc.Kill()
		t.Fatalf("CloseHandle(jobHandle): %v", err)
	}

	// Wait briefly — if KILL_ON_JOB_CLOSE fired prematurely, the child dies
	// within milliseconds. 500ms is sufficient to detect that failure mode.
	time.Sleep(500 * time.Millisecond)

	// Assert child is still alive via OpenProcess probe.
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		_ = p.proc.Kill()
		t.Fatalf("child died after CloseHandle: OpenProcess(pid=%d): %v — "+
			"KILL_ON_JOB_CLOSE fired prematurely; check nested job support", pid, err)
	}
	windows.CloseHandle(h)

	// Also verify via the upstream.Process Done channel.
	select {
	case <-p.Done:
		t.Fatal("child process exited after job handle close — KILL_ON_JOB_CLOSE fired prematurely")
	default:
		// Good: child is still running.
	}

	// Cleanup: kill the child explicitly (jobHandle already zeroed above).
	if err := p.proc.Kill(); err != nil {
		t.Logf("cleanup Kill: %v", err)
	}
	select {
	case <-p.Done:
	case <-time.After(5 * time.Second):
		t.Log("warning: child did not exit within 5s of Kill during cleanup")
	}
}
