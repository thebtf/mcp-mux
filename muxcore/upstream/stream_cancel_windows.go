//go:build windows

package upstream

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/windows"
)

var cancelSynchronousIOProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

func prepareStreamReadCancel(_ *os.File) (cancel, clear, cleanup func() error, err error) {
	runtime.LockOSThread()
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		runtime.UnlockOSThread()
		return nil, nil, func() error { return nil }, fmt.Errorf("open stream reader thread: %w", err)
	}
	cancel = func() error {
		ok, _, callErr := cancelSynchronousIOProc.Call(uintptr(thread))
		if ok != 0 || errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return nil
		}
		if callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_GEN_FAILURE
		}
		return fmt.Errorf("CancelSynchronousIo: %w", callErr)
	}
	clear = func() error { return nil }
	cleanup = func() error {
		err := windows.CloseHandle(thread)
		runtime.UnlockOSThread()
		return err
	}
	return cancel, clear, cleanup, nil
}
