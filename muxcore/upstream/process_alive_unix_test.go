//go:build !windows

package upstream

import (
	"errors"
	"os"
	"syscall"
)

func upstreamTestProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func killUpstreamTestProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
