//go:build unix
// +build unix

package sockperm

import (
	"net"
	"sync"
	"syscall"
)

// umaskMu serializes syscall.Umask calls. syscall.Umask is process-global and
// not thread-safe:
//
//	G1 sets umask(0177), G2 sets umask(0177), G1 restores original,
//	G2 creates socket with wrong umask.
//
// The mutex ensures only one goroutine manipulates the umask at a time.
var umaskMu sync.Mutex

func listenWithMode(network, addr string, mode uint32) (net.Listener, error) {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	// Compute umask as the complement of desired mode bits within the 9-bit mask.
	// Example: mode=0600 → ^0600 & 0777 = 0177 (blocks group/other access).
	old := syscall.Umask(int(^mode & 0777))
	defer syscall.Umask(old)
	return net.Listen(network, addr)
}
