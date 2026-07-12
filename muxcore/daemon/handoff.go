package daemon

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fdConn abstracts over platform-specific FD transfer channels (Unix socket
// + SCM_RIGHTS, Windows named pipe + DuplicateHandle). Platform-specific
// implementations live in handoff_unix.go / handoff_windows.go (T009-T010,
// T016-T017). This file uses only the interface.
type fdConn interface {
	// WriteJSON writes one newline-delimited JSON message.
	WriteJSON(v any) error
	// ReadJSON reads one newline-delimited JSON message into v.
	ReadJSON(v any) error
	// SendFDs transfers OS file descriptors out-of-band alongside a
	// metadata header (sent via WriteJSON). On Unix this is SCM_RIGHTS;
	// on Windows it's DuplicateHandle + pipe message.
	SendFDs(fds []uintptr, header []byte) error
	// RecvFDs receives OS file descriptors alongside an out-of-band header.
	RecvFDs() (fds []uintptr, header []byte, err error)
	// handoffSchema describes the exact v2 handle payload for this transport.
	handoffSchema() handoffHandleSchema
	// closeReceivedHandles releases handles received into this process.
	closeReceivedHandles(fds []uintptr)
	// Close releases the underlying transport.
	Close() error
}

// errPlatformUnsupported is returned by stubFDConn for all FD operations.
var errPlatformUnsupported = errors.New("handoff: platform not yet implemented")

// stubFDConn is a placeholder for platforms not yet implemented.
// All operations return errPlatformUnsupported except Close.
type handoffHandleSchema struct {
	count             int
	requiresAuthority bool
}

type stubFDConn struct{}

func (stubFDConn) WriteJSON(v any) error                      { return errPlatformUnsupported }
func (stubFDConn) ReadJSON(v any) error                       { return errPlatformUnsupported }
func (stubFDConn) SendFDs(fds []uintptr, header []byte) error { return errPlatformUnsupported }
func (stubFDConn) RecvFDs() ([]uintptr, []byte, error)        { return nil, nil, errPlatformUnsupported }
func (stubFDConn) handoffSchema() handoffHandleSchema         { return handoffHandleSchema{} }
func (stubFDConn) closeReceivedHandles([]uintptr)             {}
func (stubFDConn) Close() error                               { return nil }

// ErrTokenMismatch is returned by performHandoff when the successor daemon
// presents a token that does not match the pre-shared secret (FR-11).
var ErrTokenMismatch = errors.New("handoff: token mismatch")

// HandoffResult describes the outcome of a performHandoff call.
type HandoffResult struct {
	Transferred []string // server_ids successfully handed off
	Aborted     []string // server_ids that fell back to FR-8
	Phase       string   // last phase reached for observability
}

// HandoffUpstream is the caller-provided description of one upstream owner
// to be handed off to the successor daemon.
type HandoffUpstream struct {
	ServerID    string
	Command     string
	PID         int
	StdinFD     uintptr
	StdoutFD    uintptr
	AuthorityFD uintptr

	abort  func() error
	commit func() error
}

func (d *Daemon) retireOldOwnerSockets(ipcPath, controlPath string) bool {
	retired := true
	for _, path := range []string{ipcPath, controlPath} {
		if path == "" {
			continue
		}
		if err := removeOwnerSocketWithRetry(path); err != nil {
			retired = false
			if d.logger != nil {
				d.logger.Printf("handoff.retire_old_owner_socket_fail path=%s error=%v", path, err)
			}
		}
	}
	if retired {
		d.oldOwnerSocketRetiredCount.Add(1)
	}
	return retired
}

func removeOwnerSocketWithRetry(path string) error {
	var err error
	for attempt := 0; attempt < 40; attempt++ {
		err = os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return err
}

// performHandoff runs the old-daemon side of the v2 two-phase handoff.
// Receipt ACKs only prove that the successor owns valid handles. The predecessor
// commits a detached tree only after the final adoption ACK names its server ID.
func performHandoff(ctx context.Context, conn fdConn, token string, upstreams []HandoffUpstream) (HandoffResult, error) {
	stop := bindHandoffContext(ctx, conn)
	defer stop()
	if err := acceptHandoffHello(conn, token); err != nil {
		return HandoffResult{Phase: "hello"}, handoffContextError(ctx, err)
	}
	return performHandoffAfterHello(ctx, conn, upstreams)
}

func bindHandoffContext(ctx context.Context, conn fdConn) func() {
	if deadline, ok := ctx.Deadline(); ok {
		if setter, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
			_ = setter.SetDeadline(deadline)
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return func() {
		stop()
		if setter, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
			_ = setter.SetDeadline(time.Time{})
		}
	}
}

func handoffContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, err)
	}
	return err
}

func acceptHandoffHello(conn fdConn, token string) error {
	var hello HelloMsg
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("performHandoff: read hello: %w", err)
	}
	if hello.Type != MsgHello {
		return fmt.Errorf("performHandoff: expected hello, got %q", hello.Type)
	}
	if err := validateProtocolVersion(hello.ProtocolVersion); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(token)) != 1 {
		return ErrTokenMismatch
	}
	if setter, ok := conn.(interface{ SetTargetPID(int) }); ok {
		if hello.SourcePID <= 0 {
			return fmt.Errorf("performHandoff: invalid source_pid: %d", hello.SourcePID)
		}
		setter.SetTargetPID(hello.SourcePID)
	}
	return nil
}

func performHandoffAfterHello(ctx context.Context, conn fdConn, upstreams []HandoffUpstream) (result HandoffResult, retErr error) {
	stop := bindHandoffContext(ctx, conn)
	defer func() {
		stop()
		if retErr != nil {
			retErr = handoffContextError(ctx, retErr)
		}
	}()

	settled := false
	defer func() {
		if !settled {
			retErr = errors.Join(retErr, settleHandoffUpstreams(upstreams, nil))
		}
	}()

	schema := conn.handoffSchema()
	seen := make(map[string]struct{}, len(upstreams))
	ready := make([]HandoffUpstream, 0, len(upstreams))
	preAborted := make([]string, 0)
	for _, u := range upstreams {
		if u.ServerID == "" {
			return HandoffResult{Phase: "ready"}, errors.New("performHandoff: empty server_id")
		}
		if _, ok := seen[u.ServerID]; ok {
			return HandoffResult{Phase: "ready"}, fmt.Errorf("performHandoff: duplicate server_id %q", u.ServerID)
		}
		seen[u.ServerID] = struct{}{}
		if u.PID <= 0 || u.StdinFD == 0 || u.StdoutFD == 0 ||
			(schema.requiresAuthority && u.AuthorityFD == 0) ||
			(!schema.requiresAuthority && u.AuthorityFD != 0) {
			preAborted = append(preAborted, u.ServerID)
			continue
		}
		ready = append(ready, u)
	}

	refs := make([]UpstreamRef, len(ready))
	for i, u := range ready {
		refs[i] = UpstreamRef{ServerID: u.ServerID, Command: u.Command, PID: u.PID}
	}
	if err := conn.WriteJSON(NewReadyMsg(refs)); err != nil {
		return HandoffResult{Phase: "ready"}, fmt.Errorf("performHandoff: send ready: %w", err)
	}

	receipted := make([]string, 0, len(ready))
	rejected := make([]string, 0)
	for _, u := range ready {
		meta := NewFdTransferMsg(
			u.ServerID,
			HandleMeta{Kind: "stdin"},
			HandleMeta{Kind: "stdout"},
		)
		handles := []uintptr{u.StdinFD, u.StdoutFD}
		if schema.requiresAuthority {
			authority := HandleMeta{Kind: "tree_authority"}
			meta.AuthorityHandleMeta = &authority
			handles = append(handles, u.AuthorityFD)
		}
		if err := conn.WriteJSON(meta); err != nil {
			return HandoffResult{Phase: "fd-transfer"}, fmt.Errorf("performHandoff: send fd_transfer %s: %w", u.ServerID, err)
		}
		if err := conn.SendFDs(handles, nil); err != nil {
			return HandoffResult{Phase: "fd-transfer"}, fmt.Errorf("performHandoff: send handles %s: %w", u.ServerID, err)
		}

		var ack AckTransferMsg
		if err := conn.ReadJSON(&ack); err != nil {
			return HandoffResult{Phase: "receipt-ack"}, fmt.Errorf("performHandoff: read receipt ack %s: %w", u.ServerID, err)
		}
		if ack.Type != MsgAckTransfer {
			return HandoffResult{Phase: "receipt-ack"}, fmt.Errorf("performHandoff: expected ack_transfer, got %q", ack.Type)
		}
		if err := validateProtocolVersion(ack.ProtocolVersion); err != nil {
			return HandoffResult{Phase: "receipt-ack"}, err
		}
		if ack.ServerID != u.ServerID {
			return HandoffResult{Phase: "receipt-ack"}, fmt.Errorf("performHandoff: receipt ack server_id %q, want %q", ack.ServerID, u.ServerID)
		}
		if ack.OK {
			receipted = append(receipted, u.ServerID)
		} else {
			rejected = append(rejected, u.ServerID)
		}
	}

	if err := conn.WriteJSON(NewDoneMsg(receipted, rejected)); err != nil {
		return HandoffResult{Transferred: nil, Aborted: append(preAborted, rejected...), Phase: "done-send"},
			fmt.Errorf("performHandoff: send done: %w", err)
	}

	var finalAck HandoffAckMsg
	if err := conn.ReadJSON(&finalAck); err != nil {
		return HandoffResult{Phase: "final-ack"}, fmt.Errorf("performHandoff: read handoff_ack: %w", err)
	}
	if finalAck.Type != MsgHandoffAck {
		return HandoffResult{Phase: "final-ack"}, fmt.Errorf("performHandoff: expected handoff_ack, got %q", finalAck.Type)
	}
	if err := validateProtocolVersion(finalAck.ProtocolVersion); err != nil {
		return HandoffResult{Phase: "final-ack"}, err
	}

	readyIDs := make([]string, len(ready))
	for i := range ready {
		readyIDs[i] = ready[i].ServerID
	}
	acceptedSet, err := validateFinalPartition(readyIDs, receipted, finalAck.Accepted, finalAck.Aborted)
	if err != nil {
		return HandoffResult{Phase: "final-ack"}, err
	}

	settled = true
	settleErr := settleHandoffUpstreams(upstreams, acceptedSet)
	result = HandoffResult{
		Transferred: orderedIDs(upstreams, acceptedSet, true),
		Aborted:     orderedIDs(upstreams, acceptedSet, false),
		Phase:       "committed",
	}
	if settleErr != nil {
		return result, fmt.Errorf("performHandoff: settle leases: %w", settleErr)
	}
	return result, nil
}

func orderedIDs(upstreams []HandoffUpstream, accepted map[string]struct{}, wantAccepted bool) []string {
	ids := make([]string, 0, len(upstreams))
	for _, u := range upstreams {
		_, ok := accepted[u.ServerID]
		if ok == wantAccepted {
			ids = append(ids, u.ServerID)
		}
	}
	return ids
}

func settleHandoffUpstreams(upstreams []HandoffUpstream, accepted map[string]struct{}) error {
	var errs []error
	for i := range upstreams {
		u := &upstreams[i]
		_, ok := accepted[u.ServerID]
		var err error
		if ok {
			if u.commit != nil {
				err = u.commit()
			}
		} else if u.abort != nil {
			err = u.abort()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u.ServerID, err))
		}
	}
	return errors.Join(errs...)
}

func validateFinalPartition(ready, receipted, accepted, aborted []string) (map[string]struct{}, error) {
	readySet, err := exactIDSet("ready", ready)
	if err != nil {
		return nil, err
	}
	receiptSet, err := exactIDSet("receipted", receipted)
	if err != nil {
		return nil, err
	}
	acceptedSet, err := exactIDSet("accepted", accepted)
	if err != nil {
		return nil, err
	}
	abortedSet, err := exactIDSet("aborted", aborted)
	if err != nil {
		return nil, err
	}
	for id := range acceptedSet {
		if _, ok := receiptSet[id]; !ok {
			return nil, fmt.Errorf("handoff: accepted server_id %q was not receipted", id)
		}
	}
	for id := range acceptedSet {
		if _, duplicate := abortedSet[id]; duplicate {
			return nil, fmt.Errorf("handoff: server_id %q is both accepted and aborted", id)
		}
	}
	if len(acceptedSet)+len(abortedSet) != len(readySet) {
		return nil, fmt.Errorf("handoff: final partition has %d ids, want %d", len(acceptedSet)+len(abortedSet), len(readySet))
	}
	for id := range readySet {
		if _, ok := acceptedSet[id]; ok {
			continue
		}
		if _, ok := abortedSet[id]; !ok {
			return nil, fmt.Errorf("handoff: final partition missing server_id %q", id)
		}
	}
	for id := range acceptedSet {
		if _, ok := readySet[id]; !ok {
			return nil, fmt.Errorf("handoff: unknown accepted server_id %q", id)
		}
	}
	for id := range abortedSet {
		if _, ok := readySet[id]; !ok {
			return nil, fmt.Errorf("handoff: unknown aborted server_id %q", id)
		}
	}
	return acceptedSet, nil
}

func exactIDSet(label string, ids []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("handoff: %s contains empty server_id", label)
		}
		if _, exists := set[id]; exists {
			return nil, fmt.Errorf("handoff: %s contains duplicate server_id %q", label, id)
		}
		set[id] = struct{}{}
	}
	return set, nil
}

type handoffReceipt struct {
	conn      fdConn
	order     []string
	received  map[string]HandoffUpstream
	rejected  map[string]struct{}
	owned     map[string]bool
	taken     map[string]bool
	finalized bool
}

func prepareHandoffReceive(ctx context.Context, conn fdConn, token string) (receipt *handoffReceipt, retErr error) {
	stop := bindHandoffContext(ctx, conn)
	defer func() {
		stop()
		if retErr != nil {
			retErr = handoffContextError(ctx, retErr)
		}
	}()
	cleanup := true
	defer func() {
		if cleanup {
			if receipt != nil {
				receipt.closeOwned()
			}
			_ = conn.Close()
		}
	}()

	if err := conn.WriteJSON(NewHelloMsgWithPID(token, os.Getpid())); err != nil {
		return nil, fmt.Errorf("receiveHandoff: send hello: %w", err)
	}
	var ready ReadyMsg
	if err := conn.ReadJSON(&ready); err != nil {
		return nil, fmt.Errorf("receiveHandoff: read ready: %w", err)
	}
	if ready.Type != MsgReady {
		return nil, fmt.Errorf("receiveHandoff: expected ready, got %q", ready.Type)
	}
	if err := validateProtocolVersion(ready.ProtocolVersion); err != nil {
		return nil, err
	}

	refs := make(map[string]UpstreamRef, len(ready.Upstreams))
	order := make([]string, 0, len(ready.Upstreams))
	for _, ref := range ready.Upstreams {
		if ref.ServerID == "" || ref.PID <= 0 {
			return nil, fmt.Errorf("receiveHandoff: invalid ready upstream %+v", ref)
		}
		if _, duplicate := refs[ref.ServerID]; duplicate {
			return nil, fmt.Errorf("receiveHandoff: duplicate ready server_id %q", ref.ServerID)
		}
		refs[ref.ServerID] = ref
		order = append(order, ref.ServerID)
	}
	receipt = &handoffReceipt{
		conn:     conn,
		order:    order,
		received: make(map[string]HandoffUpstream, len(order)),
		rejected: make(map[string]struct{}),
		owned:    make(map[string]bool, len(order)),
		taken:    make(map[string]bool, len(order)),
	}
	schema := conn.handoffSchema()

	for range ready.Upstreams {
		var transfer FdTransferMsg
		if err := conn.ReadJSON(&transfer); err != nil {
			return receipt, fmt.Errorf("receiveHandoff: read fd_transfer: %w", err)
		}

		fds, _, recvErr := conn.RecvFDs()
		protocolErr := validateTransfer(transfer, refs, receipt, schema, fds, recvErr)
		if protocolErr != nil {
			conn.closeReceivedHandles(fds)
			reason := protocolErr.Error()
			if err := conn.WriteJSON(NewAckTransferMsg(transfer.ServerID, false, &reason)); err != nil {
				return receipt, fmt.Errorf("receiveHandoff: send receipt nack: %w", err)
			}
			if _, known := refs[transfer.ServerID]; !known {
				return receipt, protocolErr
			}
			if _, duplicate := receipt.received[transfer.ServerID]; duplicate {
				return receipt, protocolErr
			}
			if _, duplicate := receipt.rejected[transfer.ServerID]; duplicate {
				return receipt, protocolErr
			}
			receipt.rejected[transfer.ServerID] = struct{}{}
			continue
		}

		ref := refs[transfer.ServerID]
		u := HandoffUpstream{
			ServerID: transfer.ServerID,
			Command:  ref.Command,
			PID:      ref.PID,
			StdinFD:  fds[0],
			StdoutFD: fds[1],
		}
		if schema.requiresAuthority {
			u.AuthorityFD = fds[2]
		}
		receipt.received[u.ServerID] = u
		receipt.owned[u.ServerID] = true
		if err := conn.WriteJSON(NewAckTransferMsg(u.ServerID, true, nil)); err != nil {
			return receipt, fmt.Errorf("receiveHandoff: send receipt ack: %w", err)
		}
	}

	var done DoneMsg
	if err := conn.ReadJSON(&done); err != nil {
		return receipt, fmt.Errorf("receiveHandoff: read done: %w", err)
	}
	if done.Type != MsgDone {
		return receipt, fmt.Errorf("receiveHandoff: expected done, got %q", done.Type)
	}
	if err := validateProtocolVersion(done.ProtocolVersion); err != nil {
		return receipt, err
	}
	if err := receipt.validateDone(done); err != nil {
		return receipt, err
	}

	cleanup = false
	return receipt, nil
}

func validateTransfer(transfer FdTransferMsg, refs map[string]UpstreamRef, receipt *handoffReceipt, schema handoffHandleSchema, fds []uintptr, recvErr error) error {
	if transfer.Type != MsgFdTransfer {
		return fmt.Errorf("receiveHandoff: expected fd_transfer, got %q", transfer.Type)
	}
	if err := validateProtocolVersion(transfer.ProtocolVersion); err != nil {
		return err
	}
	if _, ok := refs[transfer.ServerID]; !ok {
		return fmt.Errorf("receiveHandoff: unknown server_id %q", transfer.ServerID)
	}
	if _, ok := receipt.received[transfer.ServerID]; ok {
		return fmt.Errorf("receiveHandoff: duplicate server_id %q", transfer.ServerID)
	}
	if _, ok := receipt.rejected[transfer.ServerID]; ok {
		return fmt.Errorf("receiveHandoff: duplicate server_id %q", transfer.ServerID)
	}
	if recvErr != nil {
		return fmt.Errorf("receiveHandoff: recv handles: %w", recvErr)
	}
	if len(fds) != schema.count {
		return fmt.Errorf("receiveHandoff: server_id %q received %d handles, want %d", transfer.ServerID, len(fds), schema.count)
	}
	if transfer.StdinHandleMeta.Kind != "stdin" || transfer.StdoutHandleMeta.Kind != "stdout" {
		return fmt.Errorf("receiveHandoff: invalid stdio metadata for %q", transfer.ServerID)
	}
	if schema.requiresAuthority {
		if transfer.AuthorityHandleMeta == nil || transfer.AuthorityHandleMeta.Kind != "tree_authority" {
			return fmt.Errorf("receiveHandoff: missing tree authority metadata for %q", transfer.ServerID)
		}
	} else if transfer.AuthorityHandleMeta != nil {
		return fmt.Errorf("receiveHandoff: unexpected tree authority metadata for %q", transfer.ServerID)
	}
	for _, fd := range fds {
		if fd == 0 {
			return fmt.Errorf("receiveHandoff: zero handle for %q", transfer.ServerID)
		}
	}
	return nil
}

func (r *handoffReceipt) validateDone(done DoneMsg) error {
	received := make([]string, 0, len(r.received))
	rejected := make([]string, 0, len(r.rejected))
	for _, id := range r.order {
		if _, ok := r.received[id]; ok {
			received = append(received, id)
		} else if _, ok := r.rejected[id]; ok {
			rejected = append(rejected, id)
		}
	}
	transferredSet, err := exactIDSet("done.transferred", done.Transferred)
	if err != nil {
		return err
	}
	abortedSet, err := exactIDSet("done.aborted", done.Aborted)
	if err != nil {
		return err
	}
	if !sameIDSet(transferredSet, received) || !sameIDSet(abortedSet, rejected) {
		return fmt.Errorf("receiveHandoff: done partition does not match validated receipts")
	}
	return nil
}

func sameIDSet(got map[string]struct{}, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			return false
		}
	}
	return true
}

func (r *handoffReceipt) upstreams() []HandoffUpstream {
	out := make([]HandoffUpstream, 0, len(r.received))
	for _, id := range r.order {
		if u, ok := r.received[id]; ok {
			out = append(out, u)
		}
	}
	return out
}

func (r *handoffReceipt) take(serverID string) (HandoffUpstream, bool) {
	u, ok := r.received[serverID]
	if !ok || !r.owned[serverID] {
		return HandoffUpstream{}, false
	}
	r.owned[serverID] = false
	r.taken[serverID] = true
	return u, true
}

func (r *handoffReceipt) finalize(accepted []string) error {
	if r == nil {
		return nil
	}
	if r.finalized {
		return errors.New("receiveHandoff: receipt already finalized")
	}
	r.finalized = true
	defer r.conn.Close() //nolint:errcheck

	acceptedSet, err := exactIDSet("accepted", accepted)
	if err != nil {
		r.closeOwned()
		return err
	}
	for id := range acceptedSet {
		if !r.taken[id] {
			r.closeOwned()
			return fmt.Errorf("receiveHandoff: accepted server_id %q was not adopted", id)
		}
	}

	aborted := make([]string, 0, len(r.order)-len(accepted))
	orderedAccepted := make([]string, 0, len(accepted))
	for _, id := range r.order {
		if _, ok := acceptedSet[id]; ok {
			orderedAccepted = append(orderedAccepted, id)
		} else {
			aborted = append(aborted, id)
		}
	}
	r.closeOwned()
	if err := r.conn.WriteJSON(NewHandoffAckResult(orderedAccepted, aborted)); err != nil {
		return fmt.Errorf("receiveHandoff: send handoff_ack: %w", err)
	}
	return nil
}

func (r *handoffReceipt) closeOwned() {
	if r == nil || r.conn == nil {
		return
	}
	for id, owned := range r.owned {
		if !owned {
			continue
		}
		u := r.received[id]
		fds := []uintptr{u.StdinFD, u.StdoutFD}
		if u.AuthorityFD != 0 {
			fds = append(fds, u.AuthorityFD)
		}
		r.conn.closeReceivedHandles(fds)
		r.owned[id] = false
	}
}

func (r *handoffReceipt) abort() {
	if r == nil || r.finalized {
		return
	}
	r.finalized = true
	r.closeOwned()
	_ = r.conn.Close()
}

// receiveHandoff preserves the public receive API by treating receipt as
// adoption. The daemon startup path uses prepareHandoffReceive directly and
// finalizes only after Owner construction and registration.
func receiveHandoff(ctx context.Context, conn fdConn, token string) ([]HandoffUpstream, error) {
	receipt, err := prepareHandoffReceive(ctx, conn, token)
	if err != nil {
		return nil, err
	}
	received := receipt.upstreams()
	accepted := make([]string, 0, len(received))
	for i := range received {
		u, ok := receipt.take(received[i].ServerID)
		if !ok {
			receipt.abort()
			return nil, fmt.Errorf("receiveHandoff: take %q failed", received[i].ServerID)
		}
		received[i] = u
		accepted = append(accepted, u.ServerID)
	}
	if err := receipt.finalize(accepted); err != nil {
		fds := make([]uintptr, 0, len(received)*3)
		for _, u := range received {
			fds = append(fds, u.StdinFD, u.StdoutFD)
			if u.AuthorityFD != 0 {
				fds = append(fds, u.AuthorityFD)
			}
		}
		conn.closeReceivedHandles(fds)
		return nil, err
	}
	return received, nil
}

// writeHandoffToken generates a 128-bit handoff token and writes it to
// {dir}/mcp-mux-handoff.tok with 0600 perms. Returns (token, path, err).
// The caller MUST delete the file after the handoff window closes —
// use deleteHandoffToken for idempotent cleanup.
func writeHandoffToken(dir string) (token string, path string, err error) {
	token, err = generateToken()
	if err != nil {
		return "", "", fmt.Errorf("handoff: generate token: %w", err)
	}
	path = filepath.Join(dir, "mcp-mux-handoff.tok")
	// os.WriteFile with 0600 perm. On Windows the perm is advisory;
	// matches the existing daemon token discipline.
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		return "", "", fmt.Errorf("handoff: write token to %s: %w", path, err)
	}
	return token, path, nil
}

// readHandoffToken reads a previously written handoff token.
func readHandoffToken(path string) (token string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("handoff: read token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// deleteHandoffToken removes the token file if it exists. Idempotent —
// missing file is NOT an error. Call from defer in performHandoff.
func deleteHandoffToken(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("handoff: delete token: %w", err)
	}
	return nil
}

// verifyHandoffToken reads a HelloMsg from conn, validates the protocol
// version, then constant-time compares the token. Returns
// ErrProtocolVersionMismatch OR ErrTokenMismatch on failure.
func verifyHandoffToken(conn fdConn, expected string) error {
	var hello HelloMsg
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("handoff: read hello: %w", err)
	}
	if err := validateProtocolVersion(hello.ProtocolVersion); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(expected)) != 1 {
		return ErrTokenMismatch
	}
	return nil
}
