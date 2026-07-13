//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package upstream

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func watchAttachedLeader(_ *Process, pid int) (<-chan error, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("upstream: kqueue attached leader %d: %w", pid, err)
	}
	change := []unix.Kevent_t{{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	if _, err := unix.Kevent(kq, change, nil, nil); err != nil {
		_ = unix.Close(kq)
		return nil, fmt.Errorf("upstream: watch attached leader %d: %w", pid, err)
	}
	done := make(chan error, 1)
	go func() {
		defer unix.Close(kq)
		events := make([]unix.Kevent_t, 1)
		for {
			_, err := unix.Kevent(kq, nil, events, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			done <- err
			return
		}
	}()
	return done, nil
}
