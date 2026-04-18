//go:build windows

// Package sockperm applies 0600 UNIX socket permissions via umask serialization.
//
// On Windows 10 1803+ AF_UNIX sockets inherit the creating process's default DACL,
// granting access only to the owner SID and LocalSystem. Named pipes created via
// net.Listen("unix", path) on Windows follow the same model. No Umask-equivalent
// API is needed; this file is intentionally a no-op wrapper.
package sockperm

import "net"

func listenWithMode(network, addr string, mode uint32) (net.Listener, error) {
	return net.Listen(network, addr)
}
