// Package sockperm provides a hardened net.Listen wrapper that creates Unix
// domain sockets with 0600 permissions on Unix platforms.
//
// On Unix, uses umask serialization via a package-level mutex to prevent
// races when multiple goroutines call Listen concurrently (syscall.Umask
// is process-global and not thread-safe).
//
// On Windows, delegates directly to net.Listen (see sockperm_windows.go).
package sockperm

import "net"

// Listen creates a network listener like net.Listen, but for Unix domain sockets
// on Unix platforms, applies 0600 permissions via umask(0177) serialization.
// Cross-references: FR-29 (S5-001 HIGH), clarification C5.
func Listen(network, addr string) (net.Listener, error) {
	return listenWithMode(network, addr, 0600)
}
