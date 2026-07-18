//go:build linux

package procgroup

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestSpawnDoesNotEnableProcessWideSubreaper(t *testing.T) {
	if os.Getenv("MCP_MUX_PROCGROUP_SUBREAPER_HELPER") == "1" {
		before := childSubreaperSetting(t)
		if before != 0 {
			t.Fatalf("fresh helper subreaper setting = %d, want 0", before)
		}
		process, err := Spawn(longRunningCmd())
		if err != nil {
			t.Fatal(err)
		}
		defer process.Kill()
		after := childSubreaperSetting(t)
		if after != before {
			t.Fatalf("Spawn changed process-wide subreaper setting from %d to %d", before, after)
		}
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestSpawnDoesNotEnableProcessWideSubreaper$")
	command.Env = append(os.Environ(), "MCP_MUX_PROCGROUP_SUBREAPER_HELPER=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated subreaper contract failed: %v\n%s", err, output)
	}
}

func childSubreaperSetting(t *testing.T) int32 {
	t.Helper()
	var setting int32
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&setting)), 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	return setting
}

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
