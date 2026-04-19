//go:build windows

package daemon

import (
	"net"
)

// wrapNetConnAsFDConn converts a net.Conn to the internal fdConn interface.
// On Windows the conn must be a winio named-pipe connection
// (winio.DialPipeContext / ListenPipe.Accept). The wrapper accepts any net.Conn —
// the caller is responsible for supplying a valid named-pipe connection.
// SetTargetPID is set internally by performHandoff after reading HelloMsg.SourcePID.
func wrapNetConnAsFDConn(conn net.Conn) (fdConn, error) {
	return newWindowsFDConn(conn), nil
}
