//go:build !windows

package daemon

import (
	"os"
	"syscall"
	"testing"
)

func dupFDForHandoff(t *testing.T, f *os.File) uintptr {
	t.Helper()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup handoff fd: %v", err)
	}
	return uintptr(fd)
}
