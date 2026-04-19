//go:build unix

package daemon

import (
	"fmt"
	"net"
)

// wrapNetConnAsFDConn converts a net.Conn to the internal fdConn interface.
// On Unix the conn MUST be a *net.UnixConn — other types return an error.
func wrapNetConnAsFDConn(conn net.Conn) (fdConn, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("wrapNetConnAsFDConn: expected *net.UnixConn, got %T", conn)
	}
	return newUnixFDConn(uc), nil
}
