//go:build linux

package procgroup

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWaitProcessGroupGoneDoesNotReapUnrelatedGroup(t *testing.T) {
	unrelated := exec.Command("sh", "-c", "exit 0")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	unrelatedReaped := false
	t.Cleanup(func() {
		if !unrelatedReaped {
			_ = unrelated.Process.Kill()
			_ = unrelated.Wait()
		}
	})
	waitExitedWithoutReaping(t, unrelated.Process.Pid)

	target := exec.Command("sleep", "999")
	target.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := target.Start(); err != nil {
		t.Fatal(err)
	}
	targetReaped := false
	t.Cleanup(func() {
		if !targetReaped {
			_ = syscall.Kill(-target.Process.Pid, syscall.SIGKILL)
			_ = target.Wait()
		}
	})
	if err := syscall.Kill(-target.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = target.Wait()
	targetReaped = true
	leaderDone := make(chan struct{})
	close(leaderDone)
	if err := waitProcessGroupGone(target.Process.Pid, leaderDone); err != nil {
		t.Fatal(err)
	}

	if err := unrelated.Wait(); err != nil {
		t.Fatalf("exact-PGID reaper stole unrelated child: %v", err)
	}
	unrelatedReaped = true
}

func waitExitedWithoutReaping(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); err != nil {
			t.Fatal(err)
		}
		if info.Signo != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d did not become waitable", pid)
}
