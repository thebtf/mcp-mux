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
// stream-mode connections is the peer.
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
