//go:build windows
// +build windows

package owner

import (
	"net"

	"golang.org/x/sys/windows"
)

// readPeerPID extracts the connected peer process ID from a winio named-pipe
// connection. winio's *win32File embeds a publicly-readable file descriptor via
// Fd() uintptr (architecture.md ADR-006); we type-assert the pipe through the
// minimal `interface{ Fd() uintptr }` surface and call
// windows.GetNamedPipeClientProcessId on the resulting handle. Used from
// acceptLoop only on the rejection-logging path; the dispatch-time path uses
// readPeerCreds.
//
// Returns -1 when:
//   - conn does not expose Fd() (unexpected transport)
//   - GetNamedPipeClientProcessId fails (pipe closed, no client connected)
//
// peerCreds normalises the -1 sentinel to 0 in ConnInfo per the
// "0 == unavailable" docstring contract on muxcore.ConnInfo.
func readPeerPID(conn net.Conn) int {
	g, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return -1
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(g.Fd()), &pid); err != nil {
		return -1
	}
	return int(pid)
}

// readPeerCreds returns peer (pid, uid) for a winio named-pipe connection on
// Windows. PID comes from GetNamedPipeClientProcessId; uid is fixed at 0
// because the Windows security model has no Unix-equivalent UID concept
// (see peer_uid_windows.go). Returns (-1, 0) on transport mismatch or
// GetNamedPipeClientProcessId failure — uid is intentionally NOT a sentinel
// on Windows because zero is the documented stable value, not a failure
// signal. peerCreds normalises -1 to 0 per the "0 == unavailable" contract.
func readPeerCreds(conn net.Conn) (pid, uid int) {
	return readPeerPID(conn), 0
}
