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

// AcquireFileLock holds the persistent namespace lock until Close, creating it
// when absent. It never deletes/truncates it or waits while owning a gate.
func AcquireFileLock(path string) (io.Closer, error) {
	file, err := openNamespaceFile(path)
	if err != nil {
		return nil, err
	}
	return AcquireFileLockFromFile(file)
}

// AcquireFileLockFromFile consumes file, including on failure, and uses the
// same namespace byte-range lock as AcquireFileLock. It never reopens the path.
func AcquireFileLockFromFile(file *os.File) (io.Closer, error) {
	if err := lockNamespaceFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &fileLock{file: file}, nil
}
