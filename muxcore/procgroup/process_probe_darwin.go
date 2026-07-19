//go:build darwin

package procgroup

import (
	"errors"
	"syscall"
)

// Darwin's killpg path returns EPERM when the process group still exists but
// contains no signalable members, including a group whose remaining members
// are zombies. Keep polling for ESRCH; the shared deadline remains fail-closed.
func processGroupProbePending(err error) bool {
	return errors.Is(err, syscall.EPERM)
}
