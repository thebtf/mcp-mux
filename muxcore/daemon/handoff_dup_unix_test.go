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

func dupRawHandoffHandle(t *testing.T, handle uintptr) uintptr {
	t.Helper()
	if handle == 0 {
		return 0
	}
	fd, err := syscall.Dup(int(handle))
	if err != nil {
		t.Fatalf("dup raw handoff fd: %v", err)
	}
	return uintptr(fd)
}

func closeRawHandoffHandle(handle uintptr) {
	if handle != 0 {
		_ = syscall.Close(int(handle))
	}
}

func daemonTestProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
