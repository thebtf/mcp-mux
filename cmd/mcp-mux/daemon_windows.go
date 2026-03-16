package main

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

var (
	modkernel32     = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx  = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

// lockFile acquires an exclusive non-blocking lock via LockFileEx.
func lockFile(f *os.File) error {
	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
	const flags = 0x00000002 | 0x00000001
	ol := &syscall.Overlapped{}
	r1, _, err := procLockFileEx.Call(
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

// unlockFile releases the file lock.
func unlockFile(f *os.File) error {
	ol := &syscall.Overlapped{}
	r1, _, err := procUnlockFileEx.Call(
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
