//go:build windows

package upstream

import (
	"fmt"
	"log"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// applyUnixSpawnAttrs is a no-op on Windows. On Unix (T008), this sets
// process group attributes for signal isolation. Defined here so the
// call site in process.go can compile on all platforms via build tags.
// T015 supersedes T008's spawn_windows.go stub — this file includes both
// the T008 no-op and the new per-upstream Job Object helpers.
func applyUnixSpawnAttrs(cmd *exec.Cmd) {}

// createUpstreamJob creates an anonymous Windows Job Object for a single
// upstream process and assigns the process to it.
//
// Design (C1 corrected): Each upstream gets its OWN Job Object distinct
// from the daemon-wide procgroup job. Windows 8+ nested jobs allow a
// process to be in multiple jobs simultaneously.
//
// NO JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: Windows kills every process in a
// job as soon as the LAST explicit handle to that job closes (membership
// does NOT refcount the handle). If the daemon is the only handle-holder
// and sets KILL_ON_JOB_CLOSE, closing the daemon's handle kills the child —
// defeating FR-1. For planned handoff (T017), the daemon DuplicateHandle's
// the job to the successor before exiting, keeping the handle count > 0.
// For unplanned daemon death (SIGKILL), the absence of KILL_ON_JOB_CLOSE
// is what lets the child survive. Intentional kill paths go through
// TerminateJobObject explicitly, not via handle close.
//
// JOB_OBJECT_LIMIT_BREAKAWAY_OK: children spawned by the upstream can
// escape this job (prevents cascading kills of language server subprocesses).
func createUpstreamJob(processHandle windows.Handle) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK

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

// closeUpstreamJob closes the daemon's handle to the upstream's Job Object.
// This does NOT kill the upstream (KILL_ON_JOB_CLOSE is intentionally absent;
// see createUpstreamJob). The upstream process remains alive with the job
// still associated, but the daemon relinquishes its handle.
//
// Intentional kill path: call terminateUpstreamJob (or equivalent wrapper
// over windows.TerminateJobObject) BEFORE closing — this kills every
// process in the job synchronously regardless of handle state.
//
// For planned handoff (T017 + T020), the daemon duplicates this handle
// to the successor via DuplicateHandle BEFORE calling closeUpstreamJob.
func closeUpstreamJob(job windows.Handle) {
	if job != 0 {
		_ = windows.CloseHandle(job)
	}
}

// afterSpawnWindows is called after procgroup.Spawn succeeds on Windows.
// It opens a process handle for the spawned PID, creates a per-upstream
// Job Object, assigns the process to it, and stores the job handle in p.
// On failure, logs a warning but does not abort — upstream still runs
// without the custom job (graceful degradation per AC8).
func afterSpawnWindows(p *Process, pid int) {
	// F75-11: least privilege — AssignProcessToJobObject requires only
	// PROCESS_SET_QUOTA and PROCESS_TERMINATE (MSDN), NOT PROCESS_ALL_ACCESS.
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false, uint32(pid))
	if err != nil {
		log.Printf("[upstream] WARNING: OpenProcess pid=%d for job assignment: %v", pid, err)
		return
	}
	defer windows.CloseHandle(handle)

	job, err := createUpstreamJob(handle)
	if err != nil {
		log.Printf("[upstream] WARNING: createUpstreamJob pid=%d: %v", pid, err)
		return
	}

	p.jobHandle = uintptr(job)
}
