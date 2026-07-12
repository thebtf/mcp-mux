//go:build windows

package upstream

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func watchAttachedLeader(p *Process, pid int) (<-chan error, error) {
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(pid),
	)
	if err != nil {
		return nil, fmt.Errorf("upstream: open attached leader %d: %w", pid, err)
	}
	p.authorityMu.Lock()
	job := windows.Handle(p.jobHandle)
	p.authorityMu.Unlock()
	inJob, err := processInJob(handle, job)
	if err != nil || !inJob {
		_ = windows.CloseHandle(handle)
		if err != nil {
			return nil, fmt.Errorf("upstream: validate attached leader %d: %w", pid, err)
		}
		return nil, fmt.Errorf("upstream: attached leader %d is outside transferred Job", pid)
	}

	done := make(chan error, 1)
	go func() {
		defer windows.CloseHandle(handle)
		status, err := windows.WaitForSingleObject(handle, windows.INFINITE)
		if err == nil && status != windows.WAIT_OBJECT_0 {
			err = fmt.Errorf("WaitForSingleObject status=%d", status)
		}
		done <- err
	}()
	return done, nil
}
