//go:build unix && !linux && !darwin
// +build unix,!linux,!darwin

package owner

import "net"

// readPeerPID stub for non-Linux/non-Darwin Unix kernels where neither
// SO_PEERCRED nor LOCAL_PEERPID is wired. peerCreds normalises -1 to 0.
func readPeerPID(conn net.Conn) int { return -1 }

// readPeerCreds stub for non-Linux/non-Darwin Unix kernels. Both fields
// fall through to the "unavailable" sentinel; peerCreds normalises both
// to 0 per the ConnInfo contract.
func readPeerCreds(conn net.Conn) (pid, uid int) { return -1, -1 }
