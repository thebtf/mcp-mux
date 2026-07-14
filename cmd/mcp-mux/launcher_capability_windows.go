//go:build windows

package main

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

func directParentExecutable() (string, error) {
	pid := uint32(windows.GetCurrentProcessId())
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return "", err
	}
	for {
		if entry.ProcessID == pid {
			h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ParentProcessID)
			if err != nil {
				return "", err
			}
			defer windows.CloseHandle(h)
			size := uint32(windows.MAX_PATH)
			buf := make([]uint16, size)
			for {
				if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil {
					return windows.UTF16ToString(buf[:size]), nil
				} else if err != windows.ERROR_INSUFFICIENT_BUFFER {
					return "", err
				}
				size *= 2
				buf = make([]uint16, size)
			}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	return "", fmt.Errorf("current process %d not found", pid)
}

func launcherAttestationServerPID(conn net.Conn) (int, error) {
	handleConn, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return 0, fmt.Errorf("launcher attestation connection exposes no Windows handle")
	}
	var peerPID uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(handleConn.Fd()), &peerPID); err != nil {
		return 0, err
	}
	return int(peerPID), nil
}
