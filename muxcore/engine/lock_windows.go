//go:build windows

package engine

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	modkernel32Engine    = syscall.NewLazyDLL("kernel32.dll")
	procLockFileExEngine = modkernel32Engine.NewProc("LockFileEx")
	procUnlockFileEngine = modkernel32Engine.NewProc("UnlockFileEx")
)

func lockDaemonFile(f *os.File) error {
	const flags = 0x00000002 | 0x00000001 // LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
	ol := &syscall.Overlapped{}
	r1, _, err := procLockFileExEngine.Call(
		uintptr(f.Fd()),
		uintptr(flags),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func unlockDaemonFile(f *os.File) error {
	ol := &syscall.Overlapped{}
	r1, _, err := procUnlockFileEngine.Call(
		uintptr(f.Fd()),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}
