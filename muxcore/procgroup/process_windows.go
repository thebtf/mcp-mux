//go:build windows

package procgroup

import (
	"fmt"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformState holds the Windows Job Object handle that wraps the process tree.
// The Job Object is created with KillOnJobClose, so the entire tree is terminated
// when the handle is closed (either explicitly via Kill/GracefulKill or when the
// mcp-mux process itself exits).
type platformState struct {
	job windows.Handle
}

// configurePlatform creates a Job Object with KILL_ON_JOB_CLOSE and assigns the
// child process to it.
//
// We deliberately do NOT use CREATE_NEW_PROCESS_GROUP because that flag breaks
// dotnet build and other tools that rely on inheriting the parent console group.
func configurePlatform(cmd *exec.Cmd, p *Process) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}

	// Set the kill-on-close limit so the job (and all its processes) die when
	// the last handle to the job is closed.
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

	// SysProcAttr.CreationFlags is set so the process starts without a new
	// console window. We do NOT set CREATE_NEW_PROCESS_GROUP here.
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}

	return nil
}

// postStart assigns the already-started process to the Job Object.
// Must be called after cmd.Start() has set cmd.Process (and therefore p.pid).
func (p *Process) postStart() error {
	if p.platform.job == 0 {
		return nil
	}

	handle, err := windows.OpenProcess(
		windows.PROCESS_ALL_ACCESS,
		false,
		uint32(p.pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess pid=%d: %w", p.pid, err)
	}
	defer windows.CloseHandle(handle)

	if err := windows.AssignProcessToJobObject(p.platform.job, handle); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// gracefulKillPlatform tries CTRL_BREAK_EVENT first (works for console processes),
// waits up to timeout, then falls back to TerminateJobObject.
func (p *Process) gracefulKillPlatform(timeout time.Duration) error {
	// GenerateConsoleCtrlEvent may fail for non-console processes — that is fine,
	// we treat it as best-effort and always fall through to TerminateJobObject on
	// timeout.
	dll := windows.NewLazySystemDLL("kernel32.dll")
	proc := dll.NewProc("GenerateConsoleCtrlEvent")
	const ctrlBreakEvent = 1
	_, _, _ = proc.Call(ctrlBreakEvent, uintptr(p.pid))

	select {
	case <-p.done:
		p.closeJob()
		return nil
	case <-time.After(timeout):
		return p.killPlatform()
	}
}

// killPlatform terminates the Job Object (all processes in the tree) immediately.
func (p *Process) killPlatform() error {
	if p.platform.job == 0 {
		// Fallback: terminate the leader process directly.
		if p.cmd.Process != nil {
			return p.cmd.Process.Kill()
		}
		return nil
	}

	if err := windows.TerminateJobObject(p.platform.job, 1); err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	<-p.done
	p.closeJob()
	return nil
}

// closeJob closes the Job Object handle, releasing OS resources.
func (p *Process) closeJob() {
	if p.platform.job != 0 {
		_ = windows.CloseHandle(p.platform.job)
		p.platform.job = 0
	}
}
