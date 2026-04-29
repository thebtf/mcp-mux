//go:build darwin
// +build darwin

package owner

import (
	"net"

	"golang.org/x/sys/unix"
)

// readPeerUID extracts the connected peer user ID from a Unix domain socket
// on macOS via getsockopt(SOL_LOCAL, LOCAL_PEERCRED), which returns the
// kernel's xucred struct (Version, Uid, Ngroups, Groups). Equivalent to
// libc getpeereid() but available through the standard x/sys/unix wrappers
// without a CGo libc binding (Getpeereid is not exposed by x/sys/unix).
//
// Returns -1 when:
//   - conn is not a *net.UnixConn (unexpected transport)
//   - SyscallConn() fails
//   - getsockopt fails (peer disappeared, kernel limitation)
//
// peerCreds normalises -1 to 0 in ConnInfo per the "0 == unavailable"
// contract on muxcore.ConnInfo.
func readPeerUID(conn net.Conn) int {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1
	}
	uid := -1
	_ = raw.Control(func(fd uintptr) {
		if x, gerr := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED); gerr == nil {
			uid = int(x.Uid)
		}
	})
	return uid
}
