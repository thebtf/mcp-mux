//go:build unix

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/sockperm"
)

const maxFDs = 4

// recvmsgReader implements io.Reader by calling syscall.Recvmsg on each Read.
// Any SCM_RIGHTS OOB data arriving during a Read is captured into u.pendingFDs
// so callers can drain it via RecvFDs. MSG_CTRUNC is treated as a hard error.
type recvmsgReader struct {
	u *unixFDConn
}

func (r *recvmsgReader) Read(p []byte) (int, error) {
	raw, err := r.u.conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("unixFDConn: syscall conn: %w", err)
	}
	oobBuf := make([]byte, syscall.CmsgSpace(maxFDs*4))
	var dataN, oobN int
	var flags int
	var rerr error
	err = raw.Read(func(fd uintptr) bool {
		n, oobn, f, _, e := syscall.Recvmsg(int(fd), p, oobBuf, 0)
		if errors.Is(e, syscall.EAGAIN) || errors.Is(e, syscall.EWOULDBLOCK) {
			return false // tell poller to wait for readiness
		}
		dataN, oobN, flags, rerr = n, oobn, f, e
		return true
	})
	if err != nil {
		return 0, fmt.Errorf("unixFDConn: raw read: %w", err)
	}
	if rerr != nil {
		return 0, rerr
	}
	if flags&syscall.MSG_CTRUNC != 0 {
		return 0, fmt.Errorf("unixFDConn: SCM_RIGHTS truncated (buffer too small)")
	}
	if oobN > 0 {
		if err := r.u.stashFDsFromOOB(oobBuf[:oobN]); err != nil {
			return 0, err
		}
	}
	if dataN == 0 {
		return 0, io.EOF
	}
	return dataN, nil
}

// unixFDConn implements fdConn using an AF_UNIX socket with SCM_RIGHTS
// ancillary messages for out-of-band FD transfer.
//
// Read path uses a json.Decoder backed by recvmsgReader — every Read call
// invokes Recvmsg with an OOB buffer so that SCM_RIGHTS arriving alongside
// JSON data is captured into pendingFDs rather than silently dropped.
// The decoder owns all internal buffering; there is no separate rbuf field.
//
// Write path uses raw Sendmsg for FD transfer only; plain Write for JSON
// (the runtime poller serialises plain Writes, which is fine since they
// never carry ancillary data).
type unixFDConn struct {
	conn       *net.UnixConn
	dec        *json.Decoder // backed by recvmsgReader; owns read buffering
	pendingFDs []uintptr     // SCM_RIGHTS captured mid-Decode by recvmsgReader
}

// newUnixFDConn wraps an existing *net.UnixConn. Caller owns the lifetime.
func newUnixFDConn(c *net.UnixConn) *unixFDConn {
	u := &unixFDConn{conn: c}
	u.dec = json.NewDecoder(&recvmsgReader{u: u})
	return u
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

// ReadJSON reads one JSON message via the decoder backed by recvmsgReader.
// The decoder consumes whitespace/newlines between messages automatically.
func (u *unixFDConn) ReadJSON(v any) error {
	if err := u.dec.Decode(v); err != nil {
		return fmt.Errorf("unixFDConn: decode: %w", err)
	}
	return nil
}

// recvmsgOnce runs a single syscall.Recvmsg inside raw.Read, handling
// EAGAIN correctly (returning false from the callback tells the poller
// to wait for readability, then invoke the callback again). Returns
// (dataN, oobN, err) where err is the final syscall error, if any.
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

// SendFDs transfers a slice of OS file descriptors out-of-band via SCM_RIGHTS.
// The header bytes are sent on the data channel in the same Sendmsg call
// (ancillary + data are delivered atomically).
//
// If header is nil a 1-byte sentinel (0x00) is substituted: macOS/BSD reject
// SCM_RIGHTS datagrams with zero-length data on SOCK_STREAM.
//
// Short-write retry: if the first Sendmsg writes fewer bytes than len(header),
// the remainder is sent via additional Write calls — SCM_RIGHTS is NOT
// re-attached to continuation writes (kernel delivers it exactly once with
// the first datagram).
func (u *unixFDConn) SendFDs(fds []uintptr, header []byte) error {
	if len(fds) == 0 {
		return fmt.Errorf("unixFDConn: SendFDs: no fds provided")
	}
	if header == nil {
		header = []byte{0}
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
//
// Two paths:
//
//  1. pendingFDs non-empty: FDs were captured by recvmsgReader during a prior
//     Decode call. Drain them, read any sentinel bytes from the decoder's
//     internal buffer, rebuild the decoder, and return.
//
//  2. pendingFDs empty: the SCM_RIGHTS datagram has not arrived yet. Drain
//     any bytes already buffered by the decoder (from a prior read-ahead),
//     issue a direct Recvmsg to fetch the ancillary datagram, combine both
//     byte slices as the header, rebuild the decoder, and return.
//
// In both paths the decoder is rebuilt after draining so subsequent ReadJSON
// calls operate on a clean, consistent state.
func (u *unixFDConn) RecvFDs() ([]uintptr, []byte, error) {
	oobBufSize := syscall.CmsgSpace(maxFDs * 4)

	if len(u.pendingFDs) > 0 {
		// FDs already captured by recvmsgReader during a Decode call.
		fds := append([]uintptr(nil), u.pendingFDs...)
		u.pendingFDs = nil

		// Drain any bytes the decoder buffered from the sentinel datagram.
		headerBytes, _ := io.ReadAll(u.dec.Buffered())

		// Rebuild decoder so subsequent ReadJSON calls start fresh.
		u.dec = json.NewDecoder(&recvmsgReader{u: u})
		return fds, headerBytes, nil
	}

	// FDs not yet arrived: drain decoder buffer first, then do a raw recvmsg.
	buffered, _ := io.ReadAll(u.dec.Buffered())

	hdrBuf := make([]byte, 4096)
	oobBuf := make([]byte, oobBufSize)
	dataN, oobN, rerr := u.recvmsgOnce(hdrBuf, oobBuf)
	if rerr != nil {
		return nil, nil, fmt.Errorf("unixFDConn: recvmsg: %w", rerr)
	}

	// Prepend any buffered bytes to the data-channel bytes from recvmsg.
	combined := append(buffered, hdrBuf[:dataN]...)

	msgs, err := syscall.ParseSocketControlMessage(oobBuf[:oobN])
	if err != nil {
		return nil, nil, fmt.Errorf("unixFDConn: parse oob: %w", err)
	}
	var fds []uintptr
	for _, m := range msgs {
		if m.Header.Level != syscall.SOL_SOCKET || m.Header.Type != syscall.SCM_RIGHTS {
			continue
		}
		rights, perr := syscall.ParseUnixRights(&m)
		if perr != nil {
			return nil, nil, fmt.Errorf("unixFDConn: parse rights: %w", perr)
		}
		for _, f := range rights {
			fds = append(fds, uintptr(f))
		}
	}

	// Rebuild decoder so subsequent ReadJSON calls work correctly.
	u.dec = json.NewDecoder(&recvmsgReader{u: u})
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
