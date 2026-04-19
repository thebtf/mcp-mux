//go:build unix

package daemon

import (
	"bufio"
	"encoding/json"
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
	br   *bufio.Reader
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

// ReadJSON reads one newline-delimited JSON message from the connection.
// Lazy-initialises a bufio.Reader on first call; the reader is cached for
// subsequent reads on the same connection.
func (u *unixFDConn) ReadJSON(v any) error {
	if u.br == nil {
		u.br = bufio.NewReader(u.conn)
	}
	line, err := u.br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("unixFDConn: read line: %w", err)
	}
	line = line[:len(line)-1] // strip trailing newline
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("unixFDConn: unmarshal %q: %w", line, err)
	}
	return nil
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

// RecvFDs receives file descriptors transferred via SCM_RIGHTS ancillary data
// and returns the received FDs, the raw header bytes from the data channel, and
// any error. Sized for up to 4 FDs (matching SendFDs expectations).
//
// Truncation: any failure in ParseSocketControlMessage or ParseUnixRights is
// returned as an error — partial FD transfer is never silently accepted because
// it would leak FDs on the sender side.
func (u *unixFDConn) RecvFDs() ([]uintptr, []byte, error) {
	// OOB buffer sized for up to 4 FDs. syscall.CmsgSpace accounts for the
	// cmsg header and alignment padding; 4 * 4 covers 4 int-sized FDs.
	const maxFDs = 4
	oobBufSize := syscall.CmsgSpace(maxFDs * 4)
	hdrBuf := make([]byte, 4096)
	oobBuf := make([]byte, oobBufSize)

	raw, err := u.conn.SyscallConn()
	if err != nil {
		return nil, nil, fmt.Errorf("unixFDConn: syscall conn: %w", err)
	}

	var readN, oobN int
	var readErr error
	err = raw.Read(func(fd uintptr) bool {
		n, oobn, _, _, e := syscall.Recvmsg(int(fd), hdrBuf, oobBuf, 0)
		readN = n
		oobN = oobn
		readErr = e
		return true
	})
	if err != nil {
		return nil, nil, fmt.Errorf("unixFDConn: raw read: %w", err)
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("unixFDConn: recvmsg: %w", readErr)
	}

	msgs, err := syscall.ParseSocketControlMessage(oobBuf[:oobN])
	if err != nil {
		return nil, nil, fmt.Errorf("unixFDConn: parse oob: %w", err)
	}

	var fds []uintptr
	for _, m := range msgs {
		if m.Header.Level != syscall.SOL_SOCKET || m.Header.Type != syscall.SCM_RIGHTS {
			continue // unknown cmsg level/type — skip safely
		}
		rights, perr := syscall.ParseUnixRights(&m)
		if perr != nil {
			return nil, nil, fmt.Errorf("unixFDConn: parse rights: %w", perr)
		}
		for _, f := range rights {
			fds = append(fds, uintptr(f))
		}
	}

	return fds, hdrBuf[:readN], nil
}

// Close releases the underlying socket.
func (u *unixFDConn) Close() error {
	return u.conn.Close()
}
