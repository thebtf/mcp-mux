package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
)

// ErrHandoffV2HandlesRequired reports that a direct PerformHandoff caller
// supplied the pre-v0.27 two-FD shape. Protocol v2 requires stderr ownership on
// every platform and transferred tree authority on Windows; live handoff cannot
// safely infer either resource. Callers should rebuild their HandoffUpstream
// values from DetachWithAuthority and use bounded cold restart on this error.
var ErrHandoffV2HandlesRequired = errors.New("handoff: protocol v2 requires complete stdio and tree authority handles")

// PerformHandoff runs the old-daemon side of the two-daemon handoff protocol.
// Transfers the given upstream file descriptors to the successor daemon after
// pre-shared token authentication.
//
// conn must be a connected Unix domain socket (*net.UnixConn on Unix) or named
// pipe connection (Windows, via winio). On Windows the successor PID is obtained
// from the HelloMsg.SourcePID field automatically.
//
// Returns per-upstream transfer outcome. Does NOT close conn.
func PerformHandoff(ctx context.Context, conn net.Conn, token string, upstreams []HandoffUpstream) (HandoffResult, error) {
	if err := validatePublicHandoffHandles(upstreams); err != nil {
		return HandoffResult{Aborted: handoffUpstreamIDs(upstreams), Phase: "validation"}, err
	}
	fc, err := wrapNetConnAsFDConn(conn)
	if err != nil {
		return HandoffResult{}, fmt.Errorf("PerformHandoff: wrap conn: %w", err)
	}
	return performHandoff(ctx, fc, token, upstreams)
}

func validatePublicHandoffHandles(upstreams []HandoffUpstream) error {
	for _, upstream := range upstreams {
		if upstream.StderrFD == 0 {
			return fmt.Errorf("%w: server_id %q has no stderr handle", ErrHandoffV2HandlesRequired, upstream.ServerID)
		}
		if runtime.GOOS == "windows" && upstream.AuthorityFD == 0 {
			return fmt.Errorf("%w: server_id %q has no Windows Job authority", ErrHandoffV2HandlesRequired, upstream.ServerID)
		}
	}
	return nil
}

func handoffUpstreamIDs(upstreams []HandoffUpstream) []string {
	ids := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		ids = append(ids, upstream.ServerID)
	}
	return ids
}

// ReceiveHandoff runs the new-daemon side of the two-daemon handoff protocol.
// Called from the successor's startup path after it accepts a connection from
// the predecessor.
//
// Returns the list of upstream descriptors received; callers re-attach these to
// their own Owner instances.
func ReceiveHandoff(ctx context.Context, conn net.Conn, token string) ([]HandoffUpstream, error) {
	fc, err := wrapNetConnAsFDConn(conn)
	if err != nil {
		return nil, fmt.Errorf("ReceiveHandoff: wrap conn: %w", err)
	}
	return receiveHandoff(ctx, fc, token)
}

// WriteHandoffToken generates a 128-bit handoff token and writes it to
// {dir}/mcp-mux-handoff.tok with 0600 permissions. Returns (token, path, err).
// Callers MUST delete the file after the handoff window closes —
// use DeleteHandoffToken for idempotent cleanup.
func WriteHandoffToken(dir string) (token string, path string, err error) {
	return writeHandoffToken(dir)
}

// ReadHandoffToken reads a previously written handoff token file.
func ReadHandoffToken(path string) (token string, err error) {
	return readHandoffToken(path)
}

// DeleteHandoffToken removes the token file if it exists. Idempotent —
// missing file is NOT an error. Safe to call from defer.
func DeleteHandoffToken(path string) error {
	return deleteHandoffToken(path)
}
