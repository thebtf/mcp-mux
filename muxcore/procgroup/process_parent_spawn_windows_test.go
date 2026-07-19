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

func TestAllowSurviveParentExitPreservesExplicitJobAuthority(t *testing.T) {
	managed, err := Spawn(longRunningCmd())
	if err != nil {
		t.Fatalf("Spawn managed child: %v", err)
	}
	t.Cleanup(func() {
		if managed.Alive() {
			_ = managed.Kill()
		}
	})

	managed.platform.mu.Lock()
	job := managed.platform.job
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	queryErr := windows.QueryInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	)
	if queryErr == nil {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION
		_, queryErr = windows.SetInformationJobObject(
			job,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		)
	}
	managed.platform.mu.Unlock()
	if queryErr != nil {
		t.Fatalf("prepare Job limits: %v", queryErr)
	}

	if err := managed.AllowSurviveParentExit(); err != nil {
		t.Fatalf("AllowSurviveParentExit: %v", err)
	}
	managed.platform.mu.Lock()
	queryErr = windows.QueryInformationJobObject(
		managed.platform.job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	)
	managed.platform.mu.Unlock()
	if queryErr != nil {
		t.Fatalf("query released Job limits: %v", queryErr)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE != 0 {
		t.Fatal("KillOnJobClose remained set after parent-exit release")
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION == 0 {
		t.Fatal("AllowSurviveParentExit replaced unrelated Job limits")
	}
	if err := managed.Kill(); err != nil {
		t.Fatalf("explicit tree Kill after parent-exit release: %v", err)
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
