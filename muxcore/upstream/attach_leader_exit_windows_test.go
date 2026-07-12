//go:build windows

package upstream

import (
	"time"

	"golang.org/x/sys/windows"
)

func duplicateAttachHandle(handle uintptr) (uintptr, error) {
	var duplicate windows.Handle
	err := windows.DuplicateHandle(
		windows.CurrentProcess(), windows.Handle(handle), windows.CurrentProcess(),
		&duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS,
	)
	return uintptr(duplicate), err
}

func duplicateAttachHandles(stdinFD, stdoutFD, authorityFD uintptr) (uintptr, uintptr, uintptr, error) {
	in, err := duplicateAttachHandle(stdinFD)
	if err != nil {
		return 0, 0, 0, err
	}
	out, err := duplicateAttachHandle(stdoutFD)
	if err != nil {
		_ = windows.CloseHandle(windows.Handle(in))
		return 0, 0, 0, err
	}
	authority, err := duplicateAttachHandle(authorityFD)
	if err != nil {
		_ = windows.CloseHandle(windows.Handle(in))
		_ = windows.CloseHandle(windows.Handle(out))
		return 0, 0, 0, err
	}
	return in, out, authority, nil
}

func processExitWaiter(pid int) (func(time.Duration) bool, func(), error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return nil, nil, err
	}
	wait := func(timeout time.Duration) bool {
		status, err := windows.WaitForSingleObject(handle, uint32(timeout.Milliseconds()))
		return err == nil && status == windows.WAIT_OBJECT_0
	}
	return wait, func() { _ = windows.CloseHandle(handle) }, nil
}
