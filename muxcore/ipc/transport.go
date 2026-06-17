//go:build !windows

// Package ipc provides cross-platform IPC transport.
//
// Unix-like platforms use Unix domain sockets. Windows uses named pipes in
// transport_windows.go to avoid stale AF_UNIX reparse-point locks.
package ipc

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/sockperm"
)

const dialTimeout = 500 * time.Millisecond

var (
	staleSocketRemoveAttempts = 40
	staleSocketRemoveDelay    = 25 * time.Millisecond
	removeSocketFile          = os.Remove
	renameSocketFile          = os.Rename
	sleepBeforeRemoveRetry    = time.Sleep
)

// Listen creates an IPC listener at the given path.
// Any stale socket file is removed before listening.
// Returns an error if another process is actively serving on path.
func Listen(path string) (net.Listener, error) {
	if IsAvailable(path) {
		return nil, fmt.Errorf("ipc: listener already active at %s (another process is serving)", path)
	}

	if err := removeStaleSocket(path); err != nil {
		return nil, fmt.Errorf("ipc: remove stale socket %s: %w", path, err)
	}

	ln, err := sockperm.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen %s: %w", path, err)
	}

	return ln, nil
}

func removeStaleSocket(path string) error {
	attempts := staleSocketRemoveAttempts
	if attempts < 1 {
		attempts = 1
	}

	delay := staleSocketRemoveDelay
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 && IsAvailable(path) {
			return fmt.Errorf("listener became active while removing stale socket")
		}

		err := removeSocketFile(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		lastErr = err

		if attempt+1 < attempts && delay > 0 {
			sleepBeforeRemoveRetry(delay)
		}
	}
	if retireErr := retireStaleSocket(path); retireErr == nil {
		return nil
	} else if os.IsNotExist(retireErr) {
		return nil
	} else {
		lastErr = fmt.Errorf("%w; retire stale socket: %v", lastErr, retireErr)
	}
	return lastErr
}

func retireStaleSocket(path string) error {
	retiredPath := fmt.Sprintf("%s.stale.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := renameSocketFile(path, retiredPath); err != nil {
		return err
	}
	_ = removeSocketFile(retiredPath)
	return nil
}

// Dial connects to an IPC endpoint at the given path.
// Returns an error if the connection cannot be established within 500ms.
func Dial(path string) (net.Conn, error) {
	return DialTimeout(path, dialTimeout)
}

func DialTimeout(path string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
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

// IsEndpointOccupiedError reports errors that mean an endpoint exists but did
// not complete a usable client connection. Unix callers rely on socket-file
// cleanup instead, so no dial error is treated as occupied here.
func IsEndpointOccupiedError(error) bool {
	return false
}

// Cleanup removes the socket file. Called by owner on shutdown.
func Cleanup(path string) {
	if IsAvailable(path) {
		return
	}
	_ = removeStaleSocket(path)
}
