//go:build linux

package upstream

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func watchAttachedLeader(_ *Process, pid int) (<-chan error, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("upstream: pidfd_open attached leader %d: %w", pid, err)
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
