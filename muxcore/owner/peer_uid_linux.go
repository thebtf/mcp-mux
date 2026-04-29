//go:build linux
// +build linux

package owner

import "net"

// readPeerUID returns the peer user ID from a Unix domain socket connection
// using SO_PEERCRED. Linux-specific; relies on the shared readPeerUcred helper
// in peer_pid_linux.go (one ucred read returns Pid+Uid+Gid together — no
// second syscall). Returns -1 on any error (transport mismatch, syscall
// failure). peerCreds normalises -1 to 0 in ConnInfo per the
// "0 == unavailable" contract.
func readPeerUID(conn net.Conn) int {
	if u, ok := readPeerUcred(conn); ok {
		return int(u.Uid)
	}
	return -1
}
