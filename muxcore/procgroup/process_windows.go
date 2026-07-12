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
	mu        sync.Mutex
	job       windows.Handle
	state     uint8
	finalized chan struct{}
}

const (
	authorityIdle uint8 = iota
	authorityActive
	authorityFinalizing
	authorityFinalized
)

func configurePlatform(cmd *exec.Cmd, p *Process) error {
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
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
	case authorityFinalizing:
		return 0, p.platform.finalized, false
	case authorityFinalized, authorityIdle:
		return 0, nil, false
	default:
		return 0, nil, false
	}
}

func (p *Process) finishJobAuthority(job windows.Handle) {
	if job != 0 {
		_ = windows.CloseHandle(job)
	}
	p.platform.mu.Lock()
	if p.platform.state == authorityFinalizing {
		p.platform.state = authorityFinalized
		close(p.platform.finalized)
	}
	p.platform.mu.Unlock()
}

func waitAuthority(wait <-chan struct{}) {
	if wait != nil {
		<-wait
	}
}

func (p *Process) gracefulKillPlatform(timeout time.Duration) error {
	if p.disableTree {
		return p.killLeader()
	}

	job, wait, owner := p.takeJobAuthority()
	if !owner {
		waitAuthority(wait)
		return nil
	}

	dll := windows.NewLazySystemDLL("kernel32.dll")
	proc := dll.NewProc("GenerateConsoleCtrlEvent")
	const ctrlBreakEvent = 1
	_, _, _ = proc.Call(ctrlBreakEvent, uintptr(p.pid))

	select {
	case <-p.leaderDone:
	case <-time.After(timeout):
	}
	err := windows.TerminateJobObject(job, 1)
	p.finishJobAuthority(job)
	<-p.done
	if err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
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
		waitAuthority(wait)
		return nil
	}
	err := windows.TerminateJobObject(job, 1)
	p.finishJobAuthority(job)
	if p.Alive() {
		<-p.done
	}
	if err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
}

func (p *Process) cleanupPlatform() {
	if p.disableTree {
		return
	}
	job, wait, owner := p.takeJobAuthority()
	if owner {
		p.finishJobAuthority(job)
		return
	}
	waitAuthority(wait)
}

func (p *Process) reapPlatform() {
	if p.disableTree {
		return
	}
	job, wait, owner := p.takeJobAuthority()
	if owner {
		_ = windows.TerminateJobObject(job, 1)
		p.finishJobAuthority(job)
		return
	}
	waitAuthority(wait)
}
