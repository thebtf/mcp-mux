//go:build darwin
// +build darwin

package owner

import "net"

// readPeerPID returns -1 on macOS. macOS uses LOCAL_PEERCRED with a different
// credential structure than Linux's SO_PEERCRED/Ucred; the syscall package
// does not expose a portable equivalent. The log message will show pid=-1.
func readPeerPID(conn net.Conn) int { return -1 }
