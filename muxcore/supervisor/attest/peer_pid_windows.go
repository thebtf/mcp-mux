//go:build windows

package attest

import (
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

func clientPID(connection net.Conn) (int, error) {
	handle, err := pipeHandle(connection)
	if err != nil {
		return 0, err
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(handle, &pid); err != nil {
		return 0, fmt.Errorf("attest: named pipe client PID: %w", err)
	}
	if pid == 0 {
		return 0, fmt.Errorf("attest: client PID unavailable")
	}
	return int(pid), nil
}

func serverPID(connection net.Conn) (int, error) {
	handle, err := pipeHandle(connection)
	if err != nil {
		return 0, err
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(handle, &pid); err != nil {
		return 0, fmt.Errorf("attest: named pipe server PID: %w", err)
	}
	if pid == 0 {
		return 0, fmt.Errorf("attest: server PID unavailable")
	}
	return int(pid), nil
}

func pipeHandle(connection net.Conn) (windows.Handle, error) {
	handleProvider, ok := connection.(interface{ Fd() uintptr })
	if !ok {
		return 0, fmt.Errorf("attest: named pipe handle unavailable")
	}
	return windows.Handle(handleProvider.Fd()), nil
}
