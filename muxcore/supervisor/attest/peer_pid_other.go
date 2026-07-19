//go:build !windows && !linux && !darwin

package attest

import (
	"errors"
	"net"
)

var errUnsupportedPeerPID = errors.New("attest: peer PID unsupported on this platform")

func clientPID(net.Conn) (int, error) { return 0, errUnsupportedPeerPID }
func serverPID(net.Conn) (int, error) { return 0, errUnsupportedPeerPID }
