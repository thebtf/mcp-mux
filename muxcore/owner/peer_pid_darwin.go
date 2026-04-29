//go:build darwin
// +build darwin

package owner

import (
	"net"

	"golang.org/x/sys/unix"
)

// readPeerPID extracts the connected peer process ID from a Unix domain socket
// on macOS via getsockopt(SOL_LOCAL, LOCAL_PEERPID). LOCAL_PEERPID returns the
// PID of the most recent process to connect on this socket, which on
// stream-mode connections is the peer. Used from acceptLoop only on the
// rejection-logging path; the dispatch-time path uses readPeerCreds.
//
// Returns -1 when:
//   - conn is not a *net.UnixConn (unexpected transport)
//   - SyscallConn() fails (rare; descriptor closed)
//   - getsockopt fails (peer disappeared, kernel limitation)
//
// peerCreds normalises the -1 sentinel to 0 in ConnInfo per the
// "0 == unavailable" docstring contract on muxcore.ConnInfo.
func readPeerPID(conn net.Conn) int {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1
	}
	pid := -1
	_ = raw.Control(func(fd uintptr) {
		if v, gerr := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID); gerr == nil {
			pid = v
		}
	})
	return pid
}

// readPeerCreds returns peer (pid, uid) for a Unix domain socket on macOS.
// Unlike Linux, macOS exposes pid and uid through DIFFERENT getsockopt
// options (LOCAL_PEERPID vs LOCAL_PEERCRED), so a single ucred struct is
// not available. We collapse both reads into one SyscallConn().Control
// callback to share the descriptor lock and the *net.UnixConn type
// assertion across both syscalls — each individual getsockopt is still
// required by the kernel ABI. Returns (-1, -1) on transport mismatch or
// SyscallConn() failure; per-syscall failures preserve the successful
// component (e.g., pid succeeds, uid fails → returns (pid, -1)).
func readPeerCreds(conn net.Conn) (pid, uid int) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, -1
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, -1
	}
	pid, uid = -1, -1
	_ = raw.Control(func(fd uintptr) {
		if v, gerr := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID); gerr == nil {
			pid = v
		}
		if x, gerr := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED); gerr == nil {
			uid = int(x.Uid)
		}
	})
	return pid, uid
}
