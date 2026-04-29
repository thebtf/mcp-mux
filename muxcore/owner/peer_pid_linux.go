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
// using SO_PEERCRED. Linux-specific; called from acceptLoop only on the
// rejection-logging path (failed handshake), where pid alone is sufficient.
// The dispatch-time path uses readPeerCreds (combined PID+UID, single syscall)
// instead. Returns -1 on any error (transport mismatch, syscall failure).
func readPeerPID(conn net.Conn) int {
	if u, ok := readPeerUcred(conn); ok {
		return int(u.Pid)
	}
	return -1
}

// readPeerCreds returns peer (pid, uid) from a Unix domain socket connection
// using a SINGLE SO_PEERCRED getsockopt call. Replaces the v0.24 dispatcher
// double-syscall pattern flagged by Gemini code review (#113); the Linux
// kernel returns the full ucred struct per call so reading pid and uid
// separately wastes a syscall. Returns (-1, -1) on transport mismatch or
// syscall failure.
func readPeerCreds(conn net.Conn) (pid, uid int) {
	if u, ok := readPeerUcred(conn); ok {
		return int(u.Pid), int(u.Uid)
	}
	return -1, -1
}
