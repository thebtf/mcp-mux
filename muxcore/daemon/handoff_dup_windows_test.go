//go:build windows

package daemon

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func dupFDForHandoff(t *testing.T, f *os.File) uintptr {
	t.Helper()
	var dup windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		windows.Handle(f.Fd()),
		windows.CurrentProcess(),
		&dup,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		t.Fatalf("duplicate handoff handle: %v", err)
	}
	return uintptr(dup)
}

func dupRawHandoffHandle(t *testing.T, handle uintptr) uintptr {
	t.Helper()
	if handle == 0 {
		return 0
	}
	var dup windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		windows.Handle(handle),
		windows.CurrentProcess(),
		&dup,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		t.Fatalf("duplicate raw handoff handle: %v", err)
	}
	return uintptr(dup)
}

func closeRawHandoffHandle(handle uintptr) {
	if handle != 0 {
		_ = windows.CloseHandle(windows.Handle(handle))
	}
}

func daemonTestProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && status == uint32(windows.WAIT_TIMEOUT)
}
