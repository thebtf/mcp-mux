//go:build unix && !linux && !darwin
// +build unix,!linux,!darwin

package owner

import "net"

func readPeerPID(conn net.Conn) int { return -1 }
