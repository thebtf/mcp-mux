//go:build linux
// +build linux

package owner

import (
	"net"
	"syscall"
)

// readPeerUcred returns the SO_PEERCRED ucred struct for a Unix domain socket
// connection. Shared between readPeerPID (this file) and readPeerUID
// (peer_uid_linux.go) — one syscall returns Pid+Uid+Gid together.
//
// Returns (nil, false) when conn is not a *net.UnixConn or the SO_PEERCRED
// getsockopt fails.
func readPeerUcred(conn net.Conn) (*syscall.Ucred, bool) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, false
	}
	var ucred *syscall.Ucred
	_ = raw.Control(func(fd uintptr) {
		u, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err == nil {
			ucred = u
		}
	})
	return ucred, ucred != nil
}

// readPeerPID returns the peer process ID from a Unix domain socket connection
// using SO_PEERCRED. Linux-specific; only called from acceptLoop. Returns -1
// on any error (transport mismatch, syscall failure).
func readPeerPID(conn net.Conn) int {
	if u, ok := readPeerUcred(conn); ok {
		return int(u.Pid)
	}
	return -1
}
