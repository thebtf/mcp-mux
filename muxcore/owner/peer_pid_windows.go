//go:build windows
// +build windows

package owner

import "net"

// readPeerPID returns -1 on Windows. Windows AF_UNIX sockets do not support
// SO_PEERCRED or an equivalent credential-passing mechanism.
func readPeerPID(conn net.Conn) int { return -1 }
