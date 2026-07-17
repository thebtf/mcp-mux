//go:build windows

package upstream

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var isProcessInJobProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func applyUnixSpawnAttrs(sysAttr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if sysAttr == nil {
		sysAttr = &syscall.SysProcAttr{}
	}
	// The upstream cannot execute until its single transferable Job authority
	// is established. afterSpawnWindows resumes the primary thread.
	sysAttr.CreationFlags |= windows.CREATE_NO_WINDOW | windows.CREATE_SUSPENDED
	return sysAttr
}

func cmdApplyUnixSpawnAttrs(cmd *exec.Cmd) {}

func createUpstreamJob(processHandle windows.Handle) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return job, nil
}

func resumeProcessThreads(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("Thread32First: %w", err)
	}

	resumed := 0
	for {
		if entry.OwnerProcessID == uint32(pid) {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("OpenThread %d: %w", entry.ThreadID, err)
			}
			previous, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return fmt.Errorf("ResumeThread %d: %w", entry.ThreadID, resumeErr)
			}
			if previous == 0 {
				// CREATE_SUSPENDED applies to the primary thread. Security,
				// monitoring, or debugger software may inject an already-running
				// thread before this snapshot; it is not our suspension to release.
				continue
			}
			resumed++
		}

		err = windows.Thread32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return fmt.Errorf("Thread32Next: %w", err)
		}
	}
	if resumed == 0 {
		return fmt.Errorf("no threads found for suspended pid %d", pid)
	}
	return nil
}

var resumeSpawnedProcessThreads = resumeProcessThreads

func afterSpawnWindows(p *Process, pid int) error {
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess pid=%d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	job, err := createUpstreamJob(handle)
	if err != nil {
		return err
	}
	p.authorityMu.Lock()
	p.jobHandle = uintptr(job)
	p.authorityMu.Unlock()

	if err := resumeSpawnedProcessThreads(pid); err != nil {
		// Keep the installed Job reachable. Start's shared failed-start path
		// must terminate, wait, and close the exact authority before retry.
		return err
	}
	return nil
}

func (p *Process) takeJobAuthority() windows.Handle {
	p.authorityMu.Lock()
	job := windows.Handle(p.jobHandle)
	p.jobHandle = 0
	p.authorityMu.Unlock()
	return job
}

func terminateProcessTree(p *Process) error {
	p.authorityMu.Lock()
	defer p.authorityMu.Unlock()
	job := windows.Handle(p.jobHandle)
	if job == 0 {
		if p.proc == nil {
			return nil
		}
		return p.proc.Kill()
	}
	if err := windows.TerminateJobObject(job, 1); err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	waitMillis := uint32(processTreeRetirementTimeout / time.Millisecond)
	status, err := windows.WaitForSingleObject(job, waitMillis)
	if err != nil {
		return fmt.Errorf("WaitForSingleObject(job): %w", err)
	}
	if status != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("job retirement not proven after %s (wait status=%d)", processTreeRetirementTimeout, status)
	}
	if err := windows.CloseHandle(job); err != nil {
		return fmt.Errorf("CloseHandle(job): %w", err)
	}
	p.jobHandle = 0
	return nil
}

func releaseTreeAuthority(p *Process) error {
	job := p.takeJobAuthority()
	if job == 0 {
		return nil
	}
	if err := windows.CloseHandle(job); err != nil {
		return fmt.Errorf("CloseHandle(job): %w", err)
	}
	return nil
}

func handoffAuthority(p *Process) (uintptr, error) {
	p.authorityMu.Lock()
	defer p.authorityMu.Unlock()
	if p.jobHandle == 0 {
		return 0, errors.New("upstream: Windows Job authority unavailable")
	}
	return p.jobHandle, nil
}

func processInJob(process, job windows.Handle) (bool, error) {
	var inJob int32
	ok, _, callErr := isProcessInJobProc.Call(
		uintptr(process),
		uintptr(job),
		uintptr(unsafe.Pointer(&inJob)),
	)
	if ok == 0 {
		if callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_HANDLE
		}
		return false, callErr
	}
	return inJob != 0, nil
}

func prepareLegacyDetach(p *Process, pid int) error {
	p.authorityMu.Lock()
	job := windows.Handle(p.jobHandle)
	p.authorityMu.Unlock()
	if job == 0 {
		return errors.New("upstream: Windows Job authority unavailable")
	}

	target, err := windows.OpenProcess(
		windows.PROCESS_DUP_HANDLE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess pid=%d for legacy detach: %w", pid, err)
	}
	defer windows.CloseHandle(target)

	inJob, err := processInJob(target, job)
	if err != nil {
		return fmt.Errorf("validate legacy detach Job: %w", err)
	}
	if !inJob {
		return fmt.Errorf("upstream: pid %d is not contained by detach Job", pid)
	}

	var remoteLease windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		job,
		target,
		&remoteLease,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return fmt.Errorf("duplicate legacy self-lease: %w", err)
	}
	return nil
}

func attachHandoffAuthority(p *Process, pid int, authority uintptr, requireAuthority bool) error {
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess pid=%d for attach: %w", pid, err)
	}
	defer windows.CloseHandle(process)

	var job windows.Handle
	if authority == 0 {
		if requireAuthority {
			return errors.New("upstream: Windows handoff missing Job authority")
		}
		job, err = createUpstreamJob(process)
		if err != nil {
			return fmt.Errorf("reacquire legacy Job authority: %w", err)
		}
	} else {
		job = windows.Handle(authority)
		inJob, err := processInJob(process, job)
		if err != nil {
			return fmt.Errorf("validate transferred Job authority: %w", err)
		}
		if !inJob {
			return fmt.Errorf("upstream: transferred Job does not contain pid %d", pid)
		}
	}

	p.authorityMu.Lock()
	p.jobHandle = uintptr(job)
	p.authorityMu.Unlock()
	return nil
}

func closeHandoffAuthority(authority uintptr) {
	if authority != 0 {
		_ = windows.CloseHandle(windows.Handle(authority))
	}
}
