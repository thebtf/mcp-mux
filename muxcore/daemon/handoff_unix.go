//go:build unix

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// unixFDConn implements fdConn using an AF_UNIX socket with SCM_RIGHTS
// ancillary messages for out-of-band FD transfer. Wire format for normal
// messages: newline-delimited JSON over the data channel. FD transfer
// messages carry SCM_RIGHTS ancillary data alongside a header on the data
// channel within the same Sendmsg call (atomic delivery).
type unixFDConn struct {
	conn *net.UnixConn
}

// newUnixFDConn wraps an existing *net.UnixConn. Caller owns the lifetime.
func newUnixFDConn(c *net.UnixConn) *unixFDConn {
	return &unixFDConn{conn: c}
}

// WriteJSON writes one newline-delimited JSON message.
func (u *unixFDConn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("unixFDConn: marshal: %w", err)
	}
	b = append(b, '\n')
	for sent := 0; sent < len(b); {
		n, werr := u.conn.Write(b[sent:])
		if werr != nil {
			return fmt.Errorf("unixFDConn: write: %w", werr)
		}
		sent += n
	}
	return nil
}

// ReadJSON is not yet implemented; T010 will provide the full implementation.
func (u *unixFDConn) ReadJSON(v any) error {
	return errors.New("unixFDConn: ReadJSON not yet implemented; T010")
}

// SendFDs transfers a slice of OS file descriptors out-of-band via SCM_RIGHTS.
// The header bytes are sent on the data channel in the same Sendmsg call
// (ancillary + data are delivered atomically). Caller typically sends a
// preceding JSON message via WriteJSON announcing the upcoming FD transfer;
// SendFDs is only the raw OOB hop.
//
// Short-write retry: if the first Sendmsg writes fewer bytes than len(header),
// the remainder is sent via additional Write calls — SCM_RIGHTS is NOT
// re-attached to continuation writes (kernel delivers it exactly once with
// the first datagram).
func (u *unixFDConn) SendFDs(fds []uintptr, header []byte) error {
	if len(fds) == 0 {
		return fmt.Errorf("unixFDConn: SendFDs: no fds provided")
	}
	intFDs := make([]int, len(fds))
	for i, f := range fds {
		intFDs[i] = int(f)
	}
	rights := syscall.UnixRights(intFDs...)

	raw, err := u.conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("unixFDConn: syscall conn: %w", err)
	}
	var sendErr error
	var firstN int
	err = raw.Write(func(fd uintptr) bool {
		// SendmsgN returns the number of data bytes sent (not OOB bytes).
		// The nil sockaddr argument is valid for a connected stream socket.
		firstN, sendErr = syscall.SendmsgN(int(fd), header, rights, nil, 0)
		return true // syscall complete, release the raw fd
	})
	if err != nil {
		return fmt.Errorf("unixFDConn: syscall write: %w", err)
	}
	if sendErr != nil {
		return fmt.Errorf("unixFDConn: sendmsg: %w", sendErr)
	}
	// Short-write continuation: send remaining header bytes.
	// SCM_RIGHTS is NOT re-attached here — it travels once with firstN.
	for sent := firstN; sent < len(header); {
		n, werr := u.conn.Write(header[sent:])
		if werr != nil {
			return fmt.Errorf("unixFDConn: short-write continue: %w", werr)
		}
		sent += n
	}
	return nil
}

// RecvFDs is not yet implemented; T010 will provide the full implementation.
func (u *unixFDConn) RecvFDs() ([]uintptr, []byte, error) {
	return nil, nil, errors.New("unixFDConn: RecvFDs not yet implemented; T010")
}

// Close releases the underlying socket.
func (u *unixFDConn) Close() error {
	return u.conn.Close()
}
