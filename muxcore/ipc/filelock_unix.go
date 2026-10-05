//go:build !windows

package ipc

import (
	"errors"
	"os"
	"syscall"
)

func lockNamespaceFile(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrFileLocked
	}
	return err
}
func unlockNamespaceFile(file *os.File) error { return syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }
