//go:build windows
// +build windows

package owner

import "net"

// readPeerUID returns 0 on Windows. The Windows security model has no UID
// concept comparable to Unix; consumer policy that needs cross-platform
// identity should use PID plus a token/SID-based identity check instead.
// A future enhancement could expose the peer's SID via the named-pipe
// security descriptor, but that is out of v0.24 scope.
func readPeerUID(_ net.Conn) int { return 0 }
