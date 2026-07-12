//go:build linux

package upstream

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

func duplicateAttachHandles(stdinFD, stdoutFD, authorityFD uintptr) (uintptr, uintptr, uintptr, error) {
	in, err := unix.Dup(int(stdinFD))
	if err != nil {
		return 0, 0, 0, err
	}
	out, err := unix.Dup(int(stdoutFD))
	if err != nil {
		_ = unix.Close(in)
		return 0, 0, 0, err
	}
	return uintptr(in), uintptr(out), authorityFD, nil
}

func processExitWaiter(pid int) (func(time.Duration) bool, func(), error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, nil, err
	}
	wait := func(timeout time.Duration) bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fds, int(timeout.Milliseconds()))
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err == nil && n == 1
		}
	}
	return wait, func() { _ = unix.Close(fd) }, nil
}
