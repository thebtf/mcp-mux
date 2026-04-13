// Package ipc provides cross-platform IPC transport using Unix domain sockets.
//
// On all platforms (including Windows 10 1803+), Go supports Unix domain sockets
// via net.Listen("unix", path). This avoids the need for platform-specific
// named pipe libraries in the PoC.
package ipc

import (
	"fmt"
	"net"
	"os"
	"time"
)

const dialTimeout = 500 * time.Millisecond

// Listen creates an IPC listener at the given path.
// Any stale socket file is removed before listening.
func Listen(path string) (net.Listener, error) {
	// Remove stale socket file if it exists
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("ipc: remove stale socket %s: %w", path, err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen %s: %w", path, err)
	}

	return ln, nil
}

// Dial connects to an IPC endpoint at the given path.
// Returns an error if the connection cannot be established within 500ms.
func Dial(path string) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("ipc: dial %s: %w", path, err)
	}
	return conn, nil
}

// IsAvailable checks if an IPC endpoint is connectable (owner is alive).
func IsAvailable(path string) bool {
	conn, err := Dial(path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Cleanup removes the socket file. Called by owner on shutdown.
func Cleanup(path string) {
	_ = os.Remove(path)
}
