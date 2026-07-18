// Package attest provides generation-bound local parent/child attestation for
// stdio supervisors. It proves an exact directly related process pair; product
// executable, layout, and update authorization remain outside this package.
package attest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const (
	// VersionV2 identifies the exact launcher attestation wire shared with the
	// protocol-v2 mcp-mux engine.
	VersionV2 = "2"

	attestationRequest  = "mcp-mux launcher attestation v2\n"
	attestationResponse = "mcp-mux launcher capable v2\n"

	defaultLifetime  = 5 * time.Second
	defaultIOTimeout = 2 * time.Second
)

// Advertisement is the non-secret location and parent claim passed to a
// prospective child. Peer process credentials, not these values alone, decide
// admission.
type Advertisement struct {
	Version   string
	ParentPID int
	Endpoint  string
}

// ParentConfig configures one ephemeral parent-side receipt.
type ParentConfig struct {
	Lifetime  time.Duration
	IOTimeout time.Duration
}

// Parent owns one ephemeral endpoint and records whether the exact child PID
// bound by BindChildPID completed the v2 exchange.
type Parent struct {
	advertisement Advertisement
	listener      net.Listener
	ioTimeout     time.Duration
	expiresAt     time.Time

	bindMu      sync.Mutex
	expectedPID int
	closed      bool
	bound       chan struct{}

	verified  atomic.Bool
	closeOnce sync.Once
	closeErr  error
	closing   chan struct{}
	done      chan struct{}
}

// StartParent creates the ephemeral endpoint before the child is spawned. The
// caller must call BindChildPID with the PID returned by its start operation.
func StartParent(ctx context.Context, config ParentConfig) (*Parent, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	lifetime := config.Lifetime
	if lifetime <= 0 {
		lifetime = defaultLifetime
	}
	ioTimeout := config.IOTimeout
	if ioTimeout <= 0 {
		ioTimeout = defaultIOTimeout
	}
	endpoint, err := randomEndpoint()
	if err != nil {
		return nil, err
	}
	listener, err := ipc.Listen(endpoint)
	if err != nil {
		return nil, fmt.Errorf("attest: listen: %w", err)
	}
	parent := &Parent{
		advertisement: Advertisement{Version: VersionV2, ParentPID: os.Getpid(), Endpoint: endpoint},
		listener:      listener,
		ioTimeout:     ioTimeout,
		expiresAt:     time.Now().Add(lifetime),
		bound:         make(chan struct{}),
		closing:       make(chan struct{}),
		done:          make(chan struct{}),
	}
	go parent.serve()
	go func() {
		select {
		case <-ctx.Done():
			_ = parent.Close()
		case <-parent.Done():
		}
	}()
	return parent, nil
}

// Advertisement returns the value to pass to the prospective child.
func (parent *Parent) Advertisement() Advertisement {
	if parent == nil {
		return Advertisement{}
	}
	return parent.advertisement
}

// BindChildPID binds the receipt to the exact PID returned by the caller's child
// start operation. Binding is single-use.
func (parent *Parent) BindChildPID(pid int) error {
	if parent == nil {
		return errors.New("attest: nil parent")
	}
	if pid <= 0 {
		return fmt.Errorf("attest: invalid child PID %d", pid)
	}
	parent.bindMu.Lock()
	defer parent.bindMu.Unlock()
	if parent.closed {
		return errors.New("attest: parent is closed")
	}
	if parent.expectedPID != 0 {
		return errors.New("attest: child PID already bound")
	}
	parent.expectedPID = pid
	close(parent.bound)
	return nil
}

// Verified reports whether the bound child completed the exact exchange.
func (parent *Parent) Verified() bool {
	return parent != nil && parent.verified.Load()
}

// Done closes when the endpoint expires, verifies, or is closed.
func (parent *Parent) Done() <-chan struct{} {
	if parent == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return parent.done
}

// Close closes and cleans the endpoint. It is idempotent.
func (parent *Parent) Close() error {
	if parent == nil {
		return nil
	}
	parent.closeOnce.Do(func() {
		parent.bindMu.Lock()
		parent.closed = true
		parent.bindMu.Unlock()
		close(parent.closing)
		parent.closeErr = parent.listener.Close()
		ipc.Cleanup(parent.advertisement.Endpoint)
		close(parent.done)
	})
	return parent.closeErr
}

func (parent *Parent) serve() {
	remaining := time.Until(parent.expiresAt)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.AfterFunc(remaining, func() { _ = parent.Close() })
	defer timer.Stop()
	defer parent.Close()
	for {
		conn, err := parent.listener.Accept()
		if err != nil {
			return
		}
		verified := parent.verifyConnection(conn)
		_ = conn.Close()
		if verified && parent.markVerified() {
			return
		}
	}
}

func (parent *Parent) markVerified() bool {
	parent.bindMu.Lock()
	defer parent.bindMu.Unlock()
	if parent.closed || !time.Now().Before(parent.expiresAt) {
		return false
	}
	parent.verified.Store(true)
	return true
}

func (parent *Parent) verifyConnection(conn net.Conn) bool {
	select {
	case <-parent.bound:
	case <-parent.closing:
		return false
	}
	parent.bindMu.Lock()
	expectedPID := parent.expectedPID
	parent.bindMu.Unlock()
	peerPID, err := clientPID(conn)
	if err != nil || peerPID != expectedPID {
		return false
	}
	deadline := time.Now().Add(parent.ioTimeout)
	if parent.expiresAt.Before(deadline) {
		deadline = parent.expiresAt
	}
	if !deadline.After(time.Now()) || conn.SetDeadline(deadline) != nil {
		return false
	}
	request := make([]byte, len(attestationRequest))
	if _, err := io.ReadFull(conn, request); err != nil || string(request) != attestationRequest {
		return false
	}
	if _, err := io.WriteString(conn, attestationResponse); err != nil {
		return false
	}
	return time.Now().Before(parent.expiresAt)
}

// VerifyConfig configures child-side verification.
type VerifyConfig struct {
	Advertisement   Advertisement
	DirectParentPID int
	IOTimeout       time.Duration
}

// VerifyParent proves that the advertised endpoint is served by the supplied
// direct parent PID and completes the exact v2 exchange. Unsupported platforms
// fail closed.
func VerifyParent(ctx context.Context, config VerifyConfig) error {
	advertisement := config.Advertisement
	if advertisement.Version != VersionV2 {
		return fmt.Errorf("attest: unsupported protocol %q", advertisement.Version)
	}
	if advertisement.ParentPID <= 0 || config.DirectParentPID <= 0 || advertisement.ParentPID != config.DirectParentPID {
		return errors.New("attest: advertised parent is not the direct parent")
	}
	if advertisement.Endpoint == "" {
		return errors.New("attest: empty endpoint")
	}
	ioTimeout := boundedTimeout(ctx, config.IOTimeout)
	if ioTimeout <= 0 {
		return context.Cause(ctx)
	}
	conn, err := ipc.DialTimeout(advertisement.Endpoint, ioTimeout)
	if err != nil {
		return fmt.Errorf("attest: dial: %w", err)
	}
	defer conn.Close()
	serverPID, err := serverPID(conn)
	if err != nil {
		return fmt.Errorf("attest: server PID: %w", err)
	}
	if serverPID != advertisement.ParentPID {
		return fmt.Errorf("attest: server PID %d does not match parent %d", serverPID, advertisement.ParentPID)
	}
	_ = conn.SetDeadline(time.Now().Add(ioTimeout))
	if _, err := io.WriteString(conn, attestationRequest); err != nil {
		return fmt.Errorf("attest: write request: %w", err)
	}
	response := make([]byte, len(attestationResponse))
	if _, err := io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("attest: read response: %w", err)
	}
	if string(response) != attestationResponse {
		return errors.New("attest: invalid response")
	}
	return nil
}

func boundedTimeout(ctx context.Context, configured time.Duration) time.Duration {
	if configured <= 0 {
		configured = defaultIOTimeout
	}
	if err := context.Cause(ctx); err != nil {
		return 0
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < configured {
			return remaining
		}
	}
	return configured
}

// unixPathMax is the Darwin RawSockaddrUnix.Path length (104 bytes), which is
// the tightest sun_path ceiling across supported platforms. Paths of this length
// or longer fail bind(2) with EINVAL on Darwin; Linux allows up to 108.
const unixPathMax = 104

// attestBasename is the fixed portion of the socket basename: "mcp-supervisor-attest-<hex>.sock".
// Fixed overhead = len("mcp-supervisor-attest-") + len(".sock") = 27.
const attestBasenameOverhead = 27

// minHexLen is the minimum random hex string length (8 bytes → 2^64 unique values).
const minHexLen = 16

// randomEndpoint returns a unique attestation socket path rooted in os.TempDir()
// that fits within the Darwin Unix socket path ceiling.
func randomEndpoint() (string, error) {
	return randomEndpointIn(os.TempDir())
}

// randomEndpointIn returns a unique attestation socket path rooted in dir that
// fits within the Darwin Unix socket path ceiling (unixPathMax-1 usable bytes).
// It uses the maximum hex length that fits, down to minHexLen. An error is
// returned only when dir itself is so long that even minHexLen chars would not
// fit; in practice this cannot happen with any real OS temp directory.
func randomEndpointIn(dir string) (string, error) {
	// usable = unixPathMax - 1 (null terminator byte reserved by the kernel).
	usable := unixPathMax - 1
	// path length = len(dir) + 1 (separator) + attestBasenameOverhead + hexLen
	hexLen := usable - len(dir) - 1 - attestBasenameOverhead
	if hexLen < minHexLen {
		return "", fmt.Errorf("attest: temp dir too long (%d bytes) to generate a bindable endpoint", len(dir))
	}
	// Round down to an even number so we read whole bytes from rand.
	hexLen = hexLen &^ 1
	randBytes := hexLen / 2
	random := make([]byte, randBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("attest: random endpoint: %w", err)
	}
	return serverid.IPCPath(dir, "mcp-supervisor-attest", hex.EncodeToString(random)), nil
}
