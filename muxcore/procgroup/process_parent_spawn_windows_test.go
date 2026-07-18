//go:build windows

package procgroup

import (
	"fmt"
	"os/exec"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var testIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func TestParentSpawnedDaemonStaysOutsideManagedJob(t *testing.T) {
	daemon := exec.Command("cmd", "/c", "ping -n 999 127.0.0.1 >nul")
	daemon.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := daemon.Start(); err != nil {
		t.Fatalf("start parent-owned daemon fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		_, _ = daemon.Process.Wait()
	})

	opts := longRunningCmd()
	opts.StartSuspended = true
	managed, err := Spawn(opts)
	if err != nil {
		t.Fatalf("Spawn managed child: %v", err)
	}
	t.Cleanup(func() { _ = managed.Kill() })

	managed.platform.mu.Lock()
	job := managed.platform.job
	managed.platform.mu.Unlock()
	if job == 0 {
		t.Fatal("managed child has no Job Object authority")
	}
	inJob, err := processInSpecificJob(managed.PID(), job)
	if err != nil {
		t.Fatalf("query managed child Job membership: %v", err)
	}
	if !inJob {
		t.Fatal("managed child PID is outside its supervisor Job")
	}
	inJob, err = processInSpecificJob(daemon.Process.Pid, job)
	if err != nil {
		t.Fatalf("query parent-owned daemon Job membership: %v", err)
	}
	if inJob {
		t.Fatal("parent-owned daemon PID inherited the managed child's supervisor Job")
	}
}

func processInSpecificJob(pid int, job windows.Handle) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(process)

	var result int32
	ok, _, callErr := testIsProcessInJob.Call(
		uintptr(process),
		uintptr(job),
		uintptr(unsafe.Pointer(&result)),
	)
	if ok == 0 {
		return false, fmt.Errorf("IsProcessInJob: %w", callErr)
	}
	return result != 0, nil
}
