//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

func directParentExecutable() (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", os.Getppid()))
}

func launcherAttestationServerPID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("launcher attestation is not a Unix connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var peerPID int
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			peerErr = err
			return
		}
		peerPID = int(cred.Pid)
	}); err != nil {
		return 0, err
	}
	if peerErr != nil {
		return 0, peerErr
	}
	return peerPID, nil
}
