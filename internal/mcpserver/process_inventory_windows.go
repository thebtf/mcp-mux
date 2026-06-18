//go:build windows

package mcpserver

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformProcessSnapshot() ([]runtimeProcess, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if err == windows.ERROR_NO_MORE_FILES {
			return []runtimeProcess{}, nil
		}
		return nil, err
	}

	processes := make([]runtimeProcess, 0)
	for {
		name := windows.UTF16ToString(entry.ExeFile[:])
		exePath := windowsProcessImagePath(entry.ProcessID)
		if proc, ok := newRuntimeProcess(int(entry.ProcessID), int(entry.ParentProcessID), name, exePath); ok {
			processes = append(processes, proc)
		}

		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return processes, err
		}
	}
	return processes, nil
}

func windowsProcessImagePath(pid uint32) string {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle)

	size := uint32(windows.MAX_PATH)
	buf := make([]uint16, size)
	for {
		err = windows.QueryFullProcessImageName(handle, 0, &buf[0], &size)
		if err == nil {
			return windows.UTF16ToString(buf[:size])
		}
		if err != windows.ERROR_INSUFFICIENT_BUFFER {
			return ""
		}
		size *= 2
		buf = make([]uint16, size)
	}
}
