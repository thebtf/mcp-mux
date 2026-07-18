//go:build linux

package procgroup

import (
	"errors"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	childSubreaperOnce sync.Once
	childSubreaperErr  error
)

func prepareProcessTreeAuthority() error {
	childSubreaperOnce.Do(func() {
		childSubreaperErr = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	})
	return childSubreaperErr
}

func drainWaitableGroupChildren(pgid int) error {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-pgid, &status, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			return nil
		case err != nil:
			return err
		case pid == 0:
			return nil
		}
	}
}
