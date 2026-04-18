//go:build linux
// +build linux

package owner

import (
	"net"
	"syscall"
)

// readPeerPID returns the peer process ID from a Unix domain socket connection
// using SO_PEERCRED. This is Linux-specific and safe to call on connections
// from a net.Listen("unix",...) listener (only called from acceptLoop, FR-28).
// Returns -1 on any error.
func readPeerPID(conn net.Conn) int {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1
	}
	var pid int = -1
	_ = raw.Control(func(fd uintptr) {
		ucred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err == nil {
			pid = int(ucred.Pid)
		}
	})
	return pid
}
