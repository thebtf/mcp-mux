//go:build windows

package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platformState struct {
	mu          sync.Mutex
	job         windows.Handle
	state       uint8
	finalized   chan struct{}
	finalizeErr error
}

const (
	authorityIdle uint8 = iota
	authorityActive
	authorityFinalizing
	authorityFinalized
)

func configurePlatform(cmd *exec.Cmd, p *Process) error {
	flags := uint32(windows.CREATE_NO_WINDOW)
	if p.startSuspended {
		flags |= windows.CREATE_SUSPENDED
	}
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: flags}
	if p.disableTree {
		return nil
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
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
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	p.platform.job = job
	p.platform.state = authorityActive
	p.platform.finalized = make(chan struct{})
	return nil
}

func (p *Process) postStart() error {
	p.platform.mu.Lock()
	job := p.platform.job
	p.platform.mu.Unlock()
	if job == 0 {
		return nil
	}

	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(p.pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess pid=%d: %w", p.pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if p.startSuspended {
		return resumeProcessThreads(p.pid)
	}
	return nil
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
			if previous > 0 {
				resumed++
			}
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
		return fmt.Errorf("no suspended threads found for pid %d", pid)
	}
	return nil
}

func (p *Process) takeJobAuthority() (windows.Handle, <-chan struct{}, bool) {
	p.platform.mu.Lock()
	defer p.platform.mu.Unlock()
	switch p.platform.state {
	case authorityActive:
		job := p.platform.job
		p.platform.job = 0
		p.platform.state = authorityFinalizing
		return job, p.platform.finalized, true
	case authorityFinalizing, authorityFinalized:
		return 0, p.platform.finalized, false
	case authorityIdle:
		return 0, nil, false
	default:
		return 0, nil, false
	}
}

func (p *Process) finishJobAuthority(job windows.Handle, finalizeErr error) error {
	if job != 0 {
		if closeErr := windows.CloseHandle(job); closeErr != nil {
			finalizeErr = errors.Join(finalizeErr, fmt.Errorf("close process Job: %w", closeErr))
		}
	}
	p.platform.mu.Lock()
	if p.platform.state == authorityFinalizing {
		p.platform.finalizeErr = finalizeErr
		p.platform.state = authorityFinalized
		close(p.platform.finalized)
	}
	result := p.platform.finalizeErr
	p.platform.mu.Unlock()
	return result
}

func (p *Process) waitJobAuthority(wait <-chan struct{}) error {
	if wait == nil {
		return nil
	}
	<-wait
	p.platform.mu.Lock()
	defer p.platform.mu.Unlock()
	return p.platform.finalizeErr
}

func terminateJobAndWait(job windows.Handle) error {
	if job == 0 {
		return nil
	}
	terminateErr := windows.TerminateJobObject(job, 1)
	waitMillis := uint32(processTreeWaitTimeout / time.Millisecond)
	if waitMillis == 0 {
		waitMillis = 1
	}
	status, waitErr := windows.WaitForSingleObject(job, waitMillis)
	if waitErr != nil {
		return errors.Join(terminateErr, fmt.Errorf("WaitForSingleObject(job): %w", waitErr))
	}
	if status == uint32(windows.WAIT_TIMEOUT) {
		return errors.Join(terminateErr, fmt.Errorf("process Job remained alive after %s", processTreeWaitTimeout))
	}
	if status != windows.WAIT_OBJECT_0 {
		return errors.Join(terminateErr, fmt.Errorf("unexpected Job wait status %d", status))
	}
	return terminateErr
}

func (p *Process) gracefulKillPlatform(timeout time.Duration) error {
	if p.disableTree {
		return p.killLeader()
	}

	job, wait, owner := p.takeJobAuthority()
	if !owner {
		authorityErr := p.waitJobAuthority(wait)
		<-p.done
		return authorityErr
	}

	dll := windows.NewLazySystemDLL("kernel32.dll")
	proc := dll.NewProc("GenerateConsoleCtrlEvent")
	const ctrlBreakEvent = 1
	_, _, _ = proc.Call(ctrlBreakEvent, uintptr(p.pid))

	select {
	case <-p.leaderDone:
	case <-time.After(timeout):
	}
	authorityErr := terminateJobAndWait(job)
	if authorityErr != nil {
		authorityErr = fmt.Errorf("terminate process Job: %w", authorityErr)
	}
	authorityErr = p.finishJobAuthority(job, authorityErr)
	<-p.done
	return authorityErr
}

func (p *Process) killLeader() error {
	if !p.Alive() || p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	if err == nil {
		<-p.done
	}
	return err
}

func (p *Process) killPlatform() error {
	if p.disableTree {
		return p.killLeader()
	}

	job, wait, owner := p.takeJobAuthority()
	if !owner {
		authorityErr := p.waitJobAuthority(wait)
		<-p.done
		return authorityErr
	}
	authorityErr := terminateJobAndWait(job)
	if authorityErr != nil {
		authorityErr = fmt.Errorf("terminate process Job: %w", authorityErr)
	}
	authorityErr = p.finishJobAuthority(job, authorityErr)
	if p.Alive() {
		<-p.done
	}
	return authorityErr
}

func (p *Process) cleanupPlatform() error {
	if p.disableTree {
		return nil
	}
	job, wait, owner := p.takeJobAuthority()
	if owner {
		authorityErr := terminateJobAndWait(job)
		if authorityErr != nil {
			authorityErr = fmt.Errorf("terminate process Job: %w", authorityErr)
		}
		return p.finishJobAuthority(job, authorityErr)
	}
	return p.waitJobAuthority(wait)
}

func (p *Process) reapPlatform() error {
	if p.disableTree {
		return nil
	}
	job, wait, owner := p.takeJobAuthority()
	if owner {
		authorityErr := terminateJobAndWait(job)
		if authorityErr != nil {
			authorityErr = fmt.Errorf("terminate process Job: %w", authorityErr)
		}
		return p.finishJobAuthority(job, authorityErr)
	}
	return p.waitJobAuthority(wait)
}
