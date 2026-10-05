package ipc

import (
	"errors"
	"io"
	"os"
)

// ErrFileLocked identifies contention without interpreting platform error text.
var ErrFileLocked = errors.New("namespace file locked")

type fileLock struct{ file *os.File }

func (l *fileLock) Close() error {
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return errors.Join(unlockNamespaceFile(file), file.Close())
}

// AcquireFileLock holds the existing persistent namespace lock until Close.
// It never deletes/truncates the lock file and never waits while owning a gate.
func AcquireFileLock(path string) (io.Closer, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockNamespaceFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &fileLock{file: file}, nil
}
