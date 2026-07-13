//go:build linux

package upstream

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var pidfdOpen = unix.PidfdOpen

const procIdentityPollInterval = 50 * time.Millisecond

type procIdentity struct {
	startTime uint64
	state     byte
}

func watchAttachedLeader(_ *Process, pid int) (<-chan error, error) {
	fd, err := pidfdOpen(pid, 0)
	if err != nil {
		if !pidfdFallbackAllowed(err) {
			return nil, fmt.Errorf("upstream: pidfd_open attached leader %d: %w", pid, err)
		}
		return watchAttachedLeaderProc(pid)
	}
	done := make(chan error, 1)
	go func() {
		defer unix.Close(fd)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			_, err := unix.Poll(fds, -1)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			done <- err
			return
		}
	}()
	return done, nil
}

func pidfdFallbackAllowed(err error) bool {
	return errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.ENODEV) ||
		errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES)
}

// watchAttachedLeaderProc is the compatibility path for kernels or seccomp
// policies without pidfd_open. It records /proc start_time before polling, so
// PID reuse is treated as exit of the original leader rather than continued
// ownership of an unrelated process.
func watchAttachedLeaderProc(pid int) (<-chan error, error) {
	original, err := readProcIdentity(pid)
	if err != nil {
		return nil, fmt.Errorf("upstream: read attached leader %d identity: %w", pid, err)
	}
	if original.state == 'Z' {
		done := make(chan error, 1)
		done <- nil
		return done, nil
	}

	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(procIdentityPollInterval)
		defer ticker.Stop()
		for range ticker.C {
			current, err := readProcIdentity(pid)
			if errors.Is(err, os.ErrNotExist) {
				done <- nil
				return
			}
			if err != nil {
				done <- fmt.Errorf("upstream: poll attached leader %d identity: %w", pid, err)
				return
			}
			if current.startTime != original.startTime || current.state == 'Z' {
				done <- nil
				return
			}
		}
	}()
	return done, nil
}

func readProcIdentity(pid int) (procIdentity, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procIdentity{}, err
	}
	return parseProcIdentity(data)
}

func parseProcIdentity(data []byte) (procIdentity, error) {
	// comm is parenthesized and may itself contain spaces or ')'. The final ')'
	// is the only stable delimiter before state (field 3).
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 || closeParen+1 >= len(data) {
		return procIdentity{}, fmt.Errorf("malformed /proc stat")
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	const startTimeIndexAfterComm = 19 // field 22 minus state field 3
	if len(fields) <= startTimeIndexAfterComm || len(fields[0]) != 1 {
		return procIdentity{}, fmt.Errorf("malformed /proc stat fields")
	}
	startTime, err := strconv.ParseUint(fields[startTimeIndexAfterComm], 10, 64)
	if err != nil {
		return procIdentity{}, fmt.Errorf("parse /proc start_time: %w", err)
	}
	return procIdentity{startTime: startTime, state: fields[0][0]}, nil
}
