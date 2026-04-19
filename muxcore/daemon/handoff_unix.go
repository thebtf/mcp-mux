//go:build unix

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/sockperm"
)

// unixFDConn implements fdConn using an AF_UNIX socket with SCM_RIGHTS
// ancillary messages for out-of-band FD transfer.
//
// Read path uses raw Recvmsg (via SyscallConn) for ALL reads — both JSON
// messages AND FD transfers. Mixing bufio.Reader with raw Recvmsg causes
// the bufio layer to read ahead into bytes that belong to subsequent
// SCM_RIGHTS datagrams, corrupting the stream. Buffer leftovers (bytes
// received past a newline on a JSON read) are cached in rbuf for the
// next call.
//
// Write path uses raw Sendmsg for FD transfer only; plain Write for JSON
// (the runtime poller serialises plain Writes, which is fine since they
// never carry ancillary data).
type unixFDConn struct {
	conn *net.UnixConn
	// rbuf holds bytes that arrived past the end of the last JSON line —
	// either a partial next message, or the 1-byte sentinel attached to
	// an SCM_RIGHTS datagram. Consumed first by ReadJSON / RecvFDs.
	rbuf []byte
	// pendingFDs stashes file descriptors that arrived via SCM_RIGHTS
	// during a ReadJSON call (not a RecvFDs call). RecvFDs drains this
	// slot before issuing a fresh recvmsg. Every recvmsg call MUST pass
	// an oobBuf to avoid MSG_CTRUNC silently dropping (leaking) the FDs
	// in the kernel — so whenever SCM_RIGHTS arrives mid-JSON-read, we
	// capture it here instead of dropping it.
	pendingFDs []uintptr
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

// recvmsgOnce runs a single syscall.Recvmsg inside raw.Read, handling
// EAGAIN correctly (returning false from the callback tells the poller
// to wait for readability, then invoke the callback again). Returns
// (dataN, oobN, err) where err is the final syscall error, if any, after
// all EAGAIN retries completed.
func (u *unixFDConn) recvmsgOnce(dataBuf, oobBuf []byte) (int, int, error) {
	raw, err := u.conn.SyscallConn()
	if err != nil {
		return 0, 0, fmt.Errorf("unixFDConn: syscall conn: %w", err)
	}
	var dataN, oobN int
	var rerr error
	err = raw.Read(func(fd uintptr) bool {
		n, oobn, _, _, e := syscall.Recvmsg(int(fd), dataBuf, oobBuf, 0)
		if errors.Is(e, syscall.EAGAIN) || errors.Is(e, syscall.EWOULDBLOCK) {
			return false // not ready — poller will re-invoke when readable
		}
		dataN, oobN, rerr = n, oobn, e
		return true
	})
	if err != nil {
		return 0, 0, fmt.Errorf("unixFDConn: raw read: %w", err)
	}
	return dataN, oobN, rerr
}

// ReadJSON reads one newline-delimited JSON message via raw Recvmsg.
// Buffers any bytes past the newline into u.rbuf for subsequent calls.
// Every Recvmsg carries an oobBuf so that SCM_RIGHTS arriving mid-JSON-
// read is captured into u.pendingFDs rather than silently dropped
// (Linux sets MSG_CTRUNC on nil oobBuf; FDs are lost).
func (u *unixFDConn) ReadJSON(v any) error {
	const (
		dataChunk = 4096
		maxFDs    = 4
	)
	oobSize := syscall.CmsgSpace(maxFDs * 4)
	for {
		if i := indexNewline(u.rbuf); i >= 0 {
			line := u.rbuf[:i]
			u.rbuf = append(u.rbuf[:0], u.rbuf[i+1:]...) // shift remainder to front
			if err := json.Unmarshal(line, v); err != nil {
				return fmt.Errorf("unixFDConn: unmarshal %q: %w", line, err)
			}
			return nil
		}
		dataBuf := make([]byte, dataChunk)
		oobBuf := make([]byte, oobSize)
		dataN, oobN, err := u.recvmsgOnce(dataBuf, oobBuf)
		if err != nil {
			return fmt.Errorf("unixFDConn: read: %w", err)
		}
		if dataN == 0 && oobN == 0 {
			return fmt.Errorf("unixFDConn: read: unexpected EOF (rbuf=%d)", len(u.rbuf))
		}
		if oobN > 0 {
			// SCM_RIGHTS arrived mid-JSON-read. Stash FDs so RecvFDs gets them later.
			if err := u.stashFDsFromOOB(oobBuf[:oobN]); err != nil {
				return fmt.Errorf("unixFDConn: stash oob: %w", err)
			}
		}
		if dataN > 0 {
			u.rbuf = append(u.rbuf, dataBuf[:dataN]...)
		}
	}
}

// stashFDsFromOOB parses SCM_RIGHTS ancillary data and appends extracted
// FDs to u.pendingFDs. Caller must ensure oob is the exact slice returned
// by a Recvmsg (trimmed to oobN).
func (u *unixFDConn) stashFDsFromOOB(oob []byte) error {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return fmt.Errorf("parse oob: %w", err)
	}
	for _, m := range msgs {
		if m.Header.Level != syscall.SOL_SOCKET || m.Header.Type != syscall.SCM_RIGHTS {
			continue
		}
		rights, perr := syscall.ParseUnixRights(&m)
		if perr != nil {
			return fmt.Errorf("parse rights: %w", perr)
		}
		for _, f := range rights {
			u.pendingFDs = append(u.pendingFDs, uintptr(f))
		}
	}
	return nil
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
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
// and returns the received FDs, the data-channel header bytes, and any error.
// Sized for up to 4 FDs (matching SendFDs expectations).
//
// Because ReadJSON may have read ahead past its newline into bytes that belong
// to the SCM_RIGHTS datagram's data portion, those buffered bytes (u.rbuf) are
// consumed first — THEN a raw Recvmsg reads the ancillary-bearing datagram
// (whose data portion may be a zero-or-one-byte sentinel per POSIX).
//
// Truncation: any failure in ParseSocketControlMessage or ParseUnixRights is
// returned as an error — partial FD transfer is never silently accepted, since
// it would leak FDs on the sender side.
func (u *unixFDConn) RecvFDs() ([]uintptr, []byte, error) {
	// Drain pendingFDs first — SCM_RIGHTS that arrived during a prior
	// ReadJSON call was stashed here rather than silently dropped.
	if len(u.pendingFDs) > 0 {
		fds := append([]uintptr(nil), u.pendingFDs...)
		u.pendingFDs = u.pendingFDs[:0]
		// Rbuf may contain the 1-byte sentinel (or other data-channel
		// bytes) that travelled alongside that OOB — return whatever's
		// in rbuf as the header and clear it.
		header := append([]byte(nil), u.rbuf...)
		u.rbuf = u.rbuf[:0]
		return fds, header, nil
	}

	// OOB buffer sized for up to 4 FDs.
	const maxFDs = 4
	oobBufSize := syscall.CmsgSpace(maxFDs * 4)
	hdrBuf := make([]byte, 4096)
	oobBuf := make([]byte, oobBufSize)

	dataN, oobN, rerr := u.recvmsgOnce(hdrBuf, oobBuf)
	if rerr != nil {
		return nil, nil, fmt.Errorf("unixFDConn: recvmsg: %w", rerr)
	}

	// Prepend any leftover data bytes buffered by a prior ReadJSON.
	var combined []byte
	if len(u.rbuf) > 0 {
		combined = append(combined, u.rbuf...)
		u.rbuf = u.rbuf[:0]
	}
	combined = append(combined, hdrBuf[:dataN]...)

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

	return fds, combined, nil
}

// Close releases the underlying socket.
func (u *unixFDConn) Close() error {
	return u.conn.Close()
}

// listenHandoffUnix binds a Unix domain socket and accepts one connection
// within the handoff window timeout. Caller owns the returned conn's lifetime.
// Uses sockperm.Listen for 0600 permissions (FR-29 / S5-001 reuse).
func listenHandoffUnix(socketPath string, acceptTimeout time.Duration) (fdConn, error) {
	ln, err := sockperm.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("handoff listen: %w", err)
	}
	defer ln.Close()

	if acceptTimeout > 0 {
		if uln, ok := ln.(*net.UnixListener); ok {
			_ = uln.SetDeadline(time.Now().Add(acceptTimeout))
		}
	}
	conn, err := ln.Accept()
	if err != nil {
		return nil, fmt.Errorf("handoff accept: %w", err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("handoff accept: not *net.UnixConn")
	}
	return newUnixFDConn(uc), nil
}

// dialHandoffUnix connects to a Unix domain socket previously bound by
// listenHandoffUnix. Caller owns the returned conn's lifetime.
func dialHandoffUnix(socketPath string, timeout time.Duration) (fdConn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("handoff dial: %w", err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("handoff dial: not *net.UnixConn")
	}
	return newUnixFDConn(uc), nil
}
