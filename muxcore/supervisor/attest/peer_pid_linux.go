//go:build linux

package attest

import (
	"fmt"
	"net"
	"syscall"
)

func peerPID(connection net.Conn) (int, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("attest: expected Unix connection")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, err
	}
	pid := 0
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			socketErr = err
			return
		}
		pid = int(credentials.Pid)
	}); err != nil {
		return 0, err
	}
	if socketErr != nil {
		return 0, socketErr
	}
	if pid <= 0 {
		return 0, fmt.Errorf("attest: peer PID unavailable")
	}
	return pid, nil
}

func clientPID(connection net.Conn) (int, error) { return peerPID(connection) }
func serverPID(connection net.Conn) (int, error) { return peerPID(connection) }
