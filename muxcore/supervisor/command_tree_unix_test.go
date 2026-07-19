//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package supervisor

import (
	"errors"
	"syscall"
)

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
