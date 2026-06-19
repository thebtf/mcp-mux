package owner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
)

const (
	defaultReconnectTimeout   = 30 * time.Second
	reconnectPollInterval     = 500 * time.Millisecond
	degradedDrainInterval     = 100 * time.Millisecond
	msgFromCCBufferSize       = 1000
	msgFromIPCBufferSize      = 1000
	defaultMaxRefreshAttempts = 3
)

// StdinEOFPolicy controls shim behavior when stdin returns EOF.
type StdinEOFPolicy int

const (
	// StdinEOFEagerExit drains in-flight requests then exits (default).
	// Suitable for MCP lifecycle where stdin close = shutdown signal.
	StdinEOFEagerExit StdinEOFPolicy = iota

	// StdinEOFWaitForDisconnect ignores stdin EOF entirely.
	// Exit only on IPC disconnect or stdout dead.
	// Suitable for engine consumers where stdin is an internal pipe.
	StdinEOFWaitForDisconnect
)

// ReconnectFunc reconnects to the daemon and returns the new IPC path and handshake token.
// Called when the IPC connection to the owner breaks.
// Must handle: ensureDaemon + spawnViaDaemon + return ipcPath, token.
type ReconnectFunc func() (ipcPath string, token string, err error)

// ResilientClientConfig configures the resilient shim proxy.
type ResilientClientConfig struct {
	Stdin          io.Reader
	Stdout         io.Writer
	InitialIPCPath string
	Token          string // handshake token from initial spawn; sent to owner on connect
	// OnInject, when non-nil, is invoked exactly once after the initial IPC
	// handshake completes. The closure pushes raw JSON-RPC frames into msgFromCC
	// via select-default semantics. Single-fire across reconnects. Closure is
	// safe for concurrent use, lifecycle-aware (returns ErrInjectClosed after
	// proxy exit). Zero value (nil) preserves pre-v0.23 behavior.
	OnInject           func(inject func([]byte) error)
	RefreshToken       ReconnectFunc
	Reconnect          ReconnectFunc
	MaxRefreshAttempts int
	ReconnectTimeout   time.Duration  // default: 30s
	StdinEOFPolicy     StdinEOFPolicy // default: EagerExit (drain pending, then exit)

	// KeepaliveInterval is no longer used. Previous revisions emitted a
	// synthetic notifications/progress with progressToken="mux-reconnect"
	// every KeepaliveInterval during reconnect. That violated the MCP spec
	// (Claude Code tears down the stdio transport on unknown progress
	// tokens). The field is kept for API compatibility with v0.19.x
	// consumers (aimux, engram); setting it has no effect.
	KeepaliveInterval time.Duration

	ProbeGracePeriod time.Duration // default: 10s; 0 disables probe detection
	Logger           *log.Logger

	// EnginePrefix is the engine name used as the prefix in IPC socket filenames
	// (e.g. "mcp-mux", "aimux"). When empty, defaults to "mcp-mux" for backward
	// compatibility with pre-v0.22 callers that don't set it.
	EnginePrefix string
}

// initCache stores the first initialize request and response for replay on reconnect.
type initCache struct {
	mu      sync.Mutex
	request []byte // raw JSON of the initialize request
	// response is intentionally not stored here — we only need to replay the request
	// to warm the new daemon cache; we discard the response and resume normal proxying.
}

// resilientClient holds the runtime state for RunResilientClient.
type resilientClient struct {
	cfg        ResilientClientConfig
	token      string        // current handshake token; updated on reconnect
	msgFromCC  chan []byte   // stdin → proxy (buffered 1000)
	msgFromIPC chan []byte   // ipc → stdout (buffered 1000)
	ipcEOF     chan struct{} // closed when IPC reader detects EOF/error
	stdoutDead chan struct{} // closed when CC stdout pipe breaks
	stdoutOnce sync.Once     // ensures stdoutDead is closed once
	startTime  time.Time     // when the client started (for probe detection)
	ipcMsgSent atomic.Bool   // true after first IPC→stdout message forwarded (disables probe detection)
	initCache  initCache
	inflight   sync.Map // request ID (json.RawMessage string) → true; tracks sent-but-unanswered
	closed     atomic.Bool
	log        *log.Logger
}

// ErrReconnectExit is returned when the shim should exit after successful
// reconnect so the MCP host restarts it with a fresh handshake. It is retained
// for API compatibility; the default resilient path keeps stdio alive and does
// not use this sentinel.
var (
	ErrInjectFull    = errors.New("muxcore: inject buffer full")
	ErrInjectClosed  = errors.New("muxcore: inject channel closed")
	ErrReconnectExit = errors.New("reconnect: exit for fresh handshake")
)

// RunResilientClient proxies CC stdio ↔ IPC with automatic reconnect on IPC failure.
//
// State machine:
//
//	CONNECTED     — normal proxy: stdin→IPC, IPC→stdout
//	RECONNECTING  — IPC broken; buffer stdin, tombstone inflight, poll Reconnect()
//	DEGRADED      — reconnect exceeded grace; keep retrying and error new requests
//	EXIT          — MCP host stdin/stdout is gone
//
// Returns only on MCP host disconnect (stdin EOF/stdout broken). Reconnect
// failures keep the shim process alive so the parent stdio transport survives
// daemon/owner restarts and temporary backend outages.
func RunResilientClient(cfg ResilientClientConfig) error {
	if cfg.ReconnectTimeout == 0 {
		cfg.ReconnectTimeout = defaultReconnectTimeout
	}
	if cfg.RefreshToken != nil && cfg.MaxRefreshAttempts == 0 {
		cfg.MaxRefreshAttempts = defaultMaxRefreshAttempts
	}
	if cfg.ProbeGracePeriod == 0 {
		cfg.ProbeGracePeriod = 10 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	rc := &resilientClient{
		cfg:        cfg,
		token:      cfg.Token,
		msgFromCC:  make(chan []byte, msgFromCCBufferSize),
		msgFromIPC: make(chan []byte, msgFromIPCBufferSize),
		ipcEOF:     make(chan struct{}),
		stdoutDead: make(chan struct{}),
		startTime:  time.Now(),
		log:        logger,
	}
	defer rc.closed.Store(true)

	// Connect to initial IPC path.
	conn, err := ipc.Dial(cfg.InitialIPCPath)
	if err != nil {
		return fmt.Errorf("resilient client: initial dial %s: %w", cfg.InitialIPCPath, err)
	}

	// Send handshake token so the owner can bind this connection to a session context.
	if rc.token != "" {
		if _, err := fmt.Fprintf(conn, "%s\n", rc.token); err != nil {
			conn.Close()
			return fmt.Errorf("resilient client: send token: %w", err)
		}
	}

	// Wire OnInject after handshake (if any). Lives outside the token branch so
	// the callback fires for token-less consumers too. RunResilientClient is
	// invoked once per ResilientClient lifecycle, so this code path itself
	// already provides the single-fire guarantee — no sync.Once needed.
	if cfg.OnInject != nil {
		rc.log.Printf("proxy.inject.armed")
		go cfg.OnInject(rc.injectFrame)
	}

	// stdinReader: reads CC stdin, sends raw message bytes to msgFromCC.
	// Runs forever until stdin EOF (i.e., CC disconnected).
	stdinDone := make(chan error, 1)
	go rc.runStdinReader(stdinDone)

	// stdoutWriter mu: protects stdout writes shared between ipcReader forward
	// path and keepalive sender.
	var stdoutMu sync.Mutex

	// stdoutWriter: drains msgFromIPC to CC stdout.
	go rc.runStdoutWriter(&stdoutMu)

	// Run the main proxy loop.
	return rc.runProxy(conn, &stdoutMu, stdinDone)
}

// runStdinReader reads newline-delimited JSON-RPC messages from CC stdin and
// puts raw bytes into rc.msgFromCC. On channel full, the oldest message is
// dropped and a warning is logged (NFR2: capped at 1000 messages).
// Sends io.EOF to stdinDone when stdin closes.
func (rc *resilientClient) runStdinReader(done chan<- error) {
	scanner := jsonrpc.NewScanner(rc.cfg.Stdin)
	for {
		msg, err := scanner.Scan()
		if err != nil {
			done <- err
			return
		}

		// Cache the first initialize request for replay.
		if msg.IsRequest() && msg.Method == "initialize" {
			rc.initCache.mu.Lock()
			if rc.initCache.request == nil {
				raw := make([]byte, len(msg.Raw))
				copy(raw, msg.Raw)
				rc.initCache.request = raw
				rc.log.Printf("resilient: cached initialize request (%d bytes)", len(raw))
			}
			rc.initCache.mu.Unlock()
		}

		raw := make([]byte, len(msg.Raw))
		copy(raw, msg.Raw)

		if msg.IsRequest() {
			rc.log.Printf("resilient: CC sent request method=%s id=%s (%d bytes)", msg.Method, string(msg.ID), len(raw))
		}

		select {
		case rc.msgFromCC <- raw:
		default:
			// Buffer full: drop oldest message to make room.
			select {
			case dropped := <-rc.msgFromCC:
				rc.log.Printf("resilient: msgFromCC buffer full, dropped oldest message (%d bytes)", len(dropped))
			default:
			}
			rc.msgFromCC <- raw
		}
	}
}

// runStdoutWriter drains rc.msgFromIPC and writes each message to CC stdout.
// Uses stdoutMu to coordinate with keepalive writes.
func (rc *resilientClient) runStdoutWriter(mu *sync.Mutex) {
	for data := range rc.msgFromIPC {
		mu.Lock()
		_, err := fmt.Fprintf(rc.cfg.Stdout, "%s\n", data)
		mu.Unlock()
		if err != nil {
			rc.log.Printf("resilient: stdout broken (CC gone): %v", err)
			rc.stdoutOnce.Do(func() { close(rc.stdoutDead) })
			return
		}
		// Mark that we successfully forwarded data to CC.
		// Once set, stdin EOF is treated as intentional disconnect (not probe).
		rc.ipcMsgSent.Store(true)
	}
}

// runProxy is the main state machine loop. It manages the IPC connection,
// starts/stops the IPC reader and writer goroutines, handles reconnect,
// and coordinates keepalive during RECONNECTING state.
func (rc *resilientClient) runProxy(conn interface {
	io.Reader
	io.Writer
	io.Closer
}, stdoutMu *sync.Mutex, stdinDone <-chan error) error {

	for {
		// CONNECTED state: start IPC reader and writer for this connection.
		ipcEOF := make(chan struct{})

		// ipcReader: reads lines from IPC conn, forwards to msgFromIPC.
		// On EOF/error: closes ipcEOF to trigger reconnect.
		go rc.runIPCReader(conn, ipcEOF)

		// ipcWriter: drains msgFromCC, writes to IPC conn.
		// Stops when ipcEOF is closed (conn will be replaced by reconnect).
		writerDone := make(chan struct{})
		go rc.runIPCWriter(conn, ipcEOF, writerDone)

		// Wait for: IPC EOF (reconnect), CC stdin EOF (exit), or stdout dead (CC gone).
		select {
		case err := <-stdinDone:
			// WaitForDisconnect: stdin EOF is not an exit signal.
			if rc.cfg.StdinEOFPolicy == StdinEOFWaitForDisconnect && err == io.EOF {
				rc.log.Printf("resilient: stdin EOF (policy=wait_for_disconnect), continuing")
				stdinDone = nil // prevent re-firing; exit via ipcEOF or stdoutDead
				continue
			}

			if err == io.EOF && rc.cfg.ProbeGracePeriod > 0 &&
				time.Since(rc.startTime) < rc.cfg.ProbeGracePeriod &&
				!rc.ipcMsgSent.Load() {
				// CC probe: stdin closed too quickly (< 10s after start) AND
				// we haven't forwarded any IPC data to CC yet. This is a real
				// probe — CC is checking if the process starts, not a post-init
				// restart. Wait briefly for CC stdout to break (confirming exit).
				rc.log.Printf("resilient: stdin EOF after %.1fs (probe, no data sent), waiting for stdout", time.Since(rc.startTime).Seconds())
				select {
				case <-rc.stdoutDead:
					rc.log.Printf("resilient: stdout also dead after probe, exiting")
				case <-time.After(5 * time.Second):
					rc.log.Printf("resilient: probe grace expired, exiting")
				}
			} else if err == io.EOF {
				pending := rc.countInflight()
				if pending > 0 {
					rc.log.Printf("resilient: stdin EOF with %d in-flight request(s), draining (max 5s)", pending)
					deadline := time.After(5 * time.Second)
					ticker := time.NewTicker(50 * time.Millisecond)
				drain:
					for {
						select {
						case <-deadline:
							rc.log.Printf("resilient: drain timeout, %d response(s) lost", rc.countInflight())
							break drain
						case <-rc.stdoutDead:
							rc.log.Printf("resilient: stdout dead during drain, exiting")
							break drain
						case <-ipcEOF:
							rc.log.Printf("resilient: IPC disconnected during drain")
							break drain
						case <-ticker.C:
							if rc.countInflight() == 0 {
								rc.log.Printf("resilient: drain complete, all responses delivered")
								break drain
							}
						}
					}
					ticker.Stop()
				} else {
					rc.log.Printf("resilient: stdin EOF after %.1fs, no pending requests, exiting",
						time.Since(rc.startTime).Seconds())
				}
			}
			// Exit — either normal disconnect, probe confirmed dead, grace expired, or drain done.
			conn.Close()
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("resilient: stdin: %w", err)

		case <-rc.stdoutDead:
			// CC stdout pipe broken — CC is gone. Exit to prevent zombie shim.
			conn.Close()
			rc.log.Printf("resilient: CC stdout dead, exiting")
			return nil

		case <-ipcEOF:
			// IPC broken — enter RECONNECTING state.
			conn.Close()
			<-writerDone // wait for ipcWriter to stop

			rc.log.Printf("resilient: IPC connection lost, reconnecting...")

			newConn, err := rc.reconnect(stdoutMu, stdinDone)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			conn = newConn
			rc.log.Printf("resilient: reconnected, resuming proxy")
			// Loop back to CONNECTED state.
		}
	}
}

// runIPCReader reads newline-delimited messages from the IPC connection and
// forwards raw bytes to rc.msgFromIPC. On EOF or error, closes ipcEOF.
func (rc *resilientClient) runIPCReader(conn io.Reader, ipcEOF chan<- struct{}) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		data := make([]byte, len(line))
		copy(data, line)

		// Clear inflight tracking when we receive a response (has "id" + "result"/"error")
		if id := extractResponseID(data); id != "" {
			rc.inflight.Delete(id)
		}

		select {
		case rc.msgFromIPC <- data:
		default:
			// Drop oldest if IPC output buffer is full.
			select {
			case dropped := <-rc.msgFromIPC:
				rc.log.Printf("resilient: msgFromIPC buffer full, dropped %d bytes", len(dropped))
			default:
			}
			rc.msgFromIPC <- data
		}
	}

	// EOF or scan error — signal reconnect.
	close(ipcEOF)
}

// runIPCWriter reads from rc.msgFromCC and writes to the IPC connection.
// Stops when ipcEOF is closed (preventing writes to a dead connection).
// Closes writerDone when it exits.
// Tracks request IDs so orphaned in-flight requests can get error responses on reconnect.
func (rc *resilientClient) runIPCWriter(conn io.Writer, ipcEOF <-chan struct{}, writerDone chan<- struct{}) {
	defer close(writerDone)
	for {
		select {
		case <-ipcEOF:
			return
		case data := <-rc.msgFromCC:
			if err := rc.forwardToIPC(conn, data); err != nil {
				rc.log.Printf("resilient: ipc write error: %v", err)
				return
			}
		}
	}
}

func (rc *resilientClient) forwardToIPC(conn io.Writer, data []byte) error {
	// Track request IDs (messages with "id" field that are not responses).
	if id := extractRequestID(data); id != "" {
		rc.inflight.Store(id, true)
		rc.log.Printf("resilient: forwarding request id=%s (%d bytes) to IPC", id, len(data))
	}
	_, err := fmt.Fprintf(conn, "%s\n", data)
	return err
}

// reconnectResult carries the result of an async Reconnect() attempt.
type reconnectResult struct {
	path  string
	token string
	err   error
}

// reconnect enters the RECONNECTING state: immediately tombstones every
// in-flight request with an RPC error response (so CC does not hang), polls
// cfg.Reconnect asynchronously, and buffers new CC stdin traffic via msgFromCC.
//
// On success: dials new IPC, replays cached initialize, and returns the new
// connection. Buffered CC messages remain in msgFromCC; the CONNECTED state's
// normal IPC writer drains them after the IPC reader is running, which avoids
// half-duplex deadlocks on Windows named pipes.
//
// If reconnect is still unavailable after ReconnectTimeout, the shim enters a
// degraded retry state: it keeps retrying forever, but responds to new client
// requests with JSON-RPC errors instead of letting the MCP host mark the stdio
// transport dead while requests hang. Returns only when the MCP host disconnects.
//
// Why no keepalive: a previous revision emitted a synthetic
// notifications/progress with progressToken="mux-reconnect" every 5s as a
// "stay alive" signal to CC. That violated the MCP spec — progressTokens
// must reference a _meta.progressToken the client issued, and Claude Code
// enforces this by tearing down the stdio transport the moment it sees an
// unknown token ("Received a progress notification for an unknown token").
// The keepalive therefore guaranteed that every reconnect window longer
// than KeepaliveInterval destroyed the transport it was trying to preserve.
// CC's stdio transport does NOT time out on silence — it times out on
// unanswered requests. Draining the in-flight map with error responses
// below is the spec-compliant substitute.
func (rc *resilientClient) reconnect(stdoutMu *sync.Mutex, stdinDone <-chan error) (interface {
	io.Reader
	io.Writer
	io.Closer
}, error) {
	// Send error responses for every request that was in-flight when the IPC
	// died, BEFORE we start the poll loop. Without this, CC waits on each
	// request until its own timeout (tens of seconds) and marks the transport
	// broken when it fires — even though the reconnect itself would have
	// succeeded in under a second.
	rc.drainOrphanedInflight(stdoutMu)

	if rc.cfg.RefreshToken == nil && rc.cfg.Reconnect == nil {
		return nil, fmt.Errorf("resilient: reconnect function not configured")
	}

	graceDeadline := time.Now().Add(rc.cfg.ReconnectTimeout)
	degraded := false
	if rc.cfg.RefreshToken != nil {
		fallbackReason := ""
		for attempt := 0; attempt < rc.cfg.MaxRefreshAttempts; attempt++ {
			res, err := rc.awaitReconnectAttempt(&graceDeadline, stdinDone, stdoutMu, &degraded, rc.cfg.RefreshToken)
			if err != nil {
				if err == io.EOF {
					return nil, io.EOF
				}
				// Terminal refresh errors (owner_gone, unknown_token) cannot
				// be resolved by retrying — every attempt would fail identically.
				// Break early and fall back to fresh spawn. Other errors are
				// treated as transient and retried up to MaxRefreshAttempts.
				if reason := terminalRefreshErrorReason(err); reason != "" {
					rc.log.Printf("shim.reconnect.refresh_fail reason=%s", reason)
					fallbackReason = reason
					break
				}
				rc.log.Printf("shim.reconnect.refresh_fail reason=%s", refreshFailureReason(err))
				if waitErr := rc.waitBeforeReconnectRetry(stdinDone, stdoutMu, degraded); waitErr != nil {
					return nil, waitErr
				}
				continue
			}

			conn, reason, err := rc.finishReconnect(res.path, res.token, stdoutMu)
			if err != nil {
				rc.log.Printf("shim.reconnect.refresh_fail reason=%s", reason)
				if reason == "owner_gone" {
					fallbackReason = "owner_gone"
					break
				}
				if waitErr := rc.waitBeforeReconnectRetry(stdinDone, stdoutMu, degraded); waitErr != nil {
					return nil, waitErr
				}
				continue
			}

			rc.log.Printf("shim.reconnect.refresh_ok owner=%s", rc.ownerPrefixFromIPCPath(res.path))
			return conn, nil
		}
		if fallbackReason == "" {
			fallbackReason = "N_refresh_fail"
		}
		rc.log.Printf("shim.reconnect.fallback_spawn reason=%s", fallbackReason)
	}

	for {
		res, err := rc.awaitReconnectAttempt(&graceDeadline, stdinDone, stdoutMu, &degraded, rc.cfg.Reconnect)
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			rc.log.Printf("shim.reconnect.fallback_spawn_fail reason=%s err=%v", refreshFailureReason(err), err)
			if waitErr := rc.waitBeforeReconnectRetry(stdinDone, stdoutMu, degraded); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		conn, _, err := rc.finishReconnect(res.path, res.token, stdoutMu)
		if err != nil {
			rc.log.Printf("resilient: reconnect handshake failed: %v (retrying)", err)
			if waitErr := rc.waitBeforeReconnectRetry(stdinDone, stdoutMu, degraded); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		return conn, nil
	}
}

func (rc *resilientClient) awaitReconnectAttempt(graceDeadline *time.Time, stdinDone <-chan error, stdoutMu *sync.Mutex, degraded *bool, fn ReconnectFunc) (reconnectResult, error) {
	if fn == nil {
		return reconnectResult{}, fmt.Errorf("resilient: reconnect function not configured")
	}

	resultCh := make(chan reconnectResult, 1)
	go func() {
		newPath, newToken, err := fn()
		resultCh <- reconnectResult{path: newPath, token: newToken, err: err}
	}()

	drainTicker := time.NewTicker(degradedDrainInterval)
	defer drainTicker.Stop()

	var graceTimer *time.Timer
	var graceC <-chan time.Time
	if graceDeadline != nil && !*degraded {
		until := time.Until(*graceDeadline)
		if until < 0 {
			until = 0
		}
		graceTimer = time.NewTimer(until)
		graceC = graceTimer.C
		defer graceTimer.Stop()
	}

	for {
		select {
		case err := <-stdinDone:
			if err == io.EOF {
				return reconnectResult{}, io.EOF
			}
			return reconnectResult{}, fmt.Errorf("resilient: stdin during reconnect: %w", err)
		case res := <-resultCh:
			return res, res.err
		case <-graceC:
			*degraded = true
			graceC = nil
			rc.log.Printf("shim.reconnect.degraded after=%s action=keep_retrying", rc.cfg.ReconnectTimeout)
			rc.failBufferedRequestsDuringReconnect(stdoutMu)
		case <-drainTicker.C:
			if *degraded {
				rc.failBufferedRequestsDuringReconnect(stdoutMu)
			}
		}
	}
}

func (rc *resilientClient) waitBeforeReconnectRetry(stdinDone <-chan error, stdoutMu *sync.Mutex, degraded bool) error {
	timer := time.NewTimer(reconnectPollInterval)
	defer timer.Stop()
	drainTicker := time.NewTicker(degradedDrainInterval)
	defer drainTicker.Stop()
	for {
		select {
		case err := <-stdinDone:
			if err == io.EOF {
				return io.EOF
			}
			return fmt.Errorf("resilient: stdin during reconnect retry: %w", err)
		case <-timer.C:
			if degraded {
				rc.failBufferedRequestsDuringReconnect(stdoutMu)
			}
			return nil
		case <-drainTicker.C:
			if degraded {
				rc.failBufferedRequestsDuringReconnect(stdoutMu)
			}
		}
	}
}

func (rc *resilientClient) finishReconnect(path, token string, stdoutMu *sync.Mutex) (interface {
	io.Reader
	io.Writer
	io.Closer
}, string, error) {
	conn, err := ipc.Dial(path)
	if err != nil {
		return nil, "dial", err
	}

	rc.token = token
	if rc.token != "" {
		if _, err := fmt.Fprintf(conn, "%s\n", rc.token); err != nil {
			conn.Close()
			return nil, "handshake", err
		}
	}

	if err := rc.replayInit(conn); err != nil {
		conn.Close()
		return nil, "handshake", err
	}

	rc.sendListChangedNotifications(stdoutMu)
	return conn, "", nil
}

func refreshFailureReason(err error) string {
	if isOwnerGoneError(err) {
		return "owner_gone"
	}
	if isUnknownTokenError(err) {
		return "unknown_token"
	}
	if isDaemonShuttingDownError(err) {
		return "daemon_shutting_down"
	}
	return "other"
}

func isOwnerGoneError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "owner gone")
}

func isUnknownTokenError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown token")
}

func isDaemonShuttingDownError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "daemon shutting down")
}

// terminalRefreshErrorReason returns a non-empty fallback reason if err is
// known to be unrecoverable across retries (the new daemon will never accept
// the cached token regardless of how many times we ask). Returns "" for
// transient errors that should be retried up to MaxRefreshAttempts.
func terminalRefreshErrorReason(err error) string {
	switch {
	case isOwnerGoneError(err):
		return "owner_gone"
	case isUnknownTokenError(err):
		return "unknown_token"
	default:
		return ""
	}
}

// ownerPrefixFromIPCPath extracts the short server-ID prefix (up to 8 chars)
// from an IPC socket path for use in log messages. Strips the engine prefix
// from rc.cfg.EnginePrefix (defaulting to "mcp-mux" for backward compat).
// Converted to method to avoid the hardcoded "mcp-mux-" literal — engine name
// is now caller-supplied via ResilientClientConfig.EnginePrefix.
func (rc *resilientClient) ownerPrefixFromIPCPath(ipcPath string) string {
	enginePrefix := rc.cfg.EnginePrefix
	if enginePrefix == "" {
		enginePrefix = "mcp-mux"
	}
	base := filepath.Base(ipcPath)
	base = strings.TrimSuffix(base, ".sock")
	base = strings.TrimPrefix(base, enginePrefix+"-")
	if idx := strings.IndexByte(base, '.'); idx >= 0 {
		base = base[:idx]
	}
	if len(base) > 8 {
		base = base[:8]
	}
	if base == "" {
		return "unknown"
	}
	return base
}

// replayInit replays the cached initialize request to the new IPC connection
// and reads (discards) the response. This warms the new daemon's cache so
// subsequent CC requests get correct responses.
func (rc *resilientClient) replayInit(conn interface {
	io.Reader
	io.Writer
}) error {
	rc.initCache.mu.Lock()
	req := rc.initCache.request
	rc.initCache.mu.Unlock()

	if req == nil {
		// No initialize seen yet — nothing to replay.
		return nil
	}

	// Send the cached initialize request.
	_, err := fmt.Fprintf(conn, "%s\n", req)
	if err != nil {
		return fmt.Errorf("write init replay: %w", err)
	}
	rc.log.Printf("resilient: replayed initialize request")

	// Read and discard the response — byte-by-byte to avoid bufio buffering.
	// A bufio.Scanner would read ahead into its internal buffer, consuming
	// subsequent messages (tools/list, prompts/list) that belong to runIPCReader.
	buf := make([]byte, 0, 4096)
	one := make([]byte, 1)
	for {
		_, err = conn.Read(one)
		if err != nil {
			return fmt.Errorf("read init response: %w", err)
		}
		if one[0] == '\n' {
			break
		}
		buf = append(buf, one[0])
		if len(buf) > 1024*1024 {
			return fmt.Errorf("read init response: line too long (%d bytes)", len(buf))
		}
	}
	rc.log.Printf("resilient: discarded init replay response (%d bytes)", len(buf))
	return nil
}

func (rc *resilientClient) injectFrame(b []byte) error {
	if rc.closed.Load() {
		rc.log.Printf("proxy.inject.dropped reason=closed")
		return ErrInjectClosed
	}
	// Copy the caller's buffer — they may reuse or pool it after inject returns.
	data := make([]byte, len(b))
	copy(data, b)
	select {
	case rc.msgFromCC <- data:
		rc.log.Printf("proxy.inject.delivered bytes=%d", len(b))
		return nil
	default:
		rc.log.Printf("proxy.inject.dropped reason=full")
		return ErrInjectFull
	}
}

// sendListChangedNotifications sends *_list_changed notifications to CC after reconnect.
// This triggers CC to re-fetch tools/list, prompts/list, and resources/list from the
// new upstream, ensuring CC sees updated capabilities.
func (rc *resilientClient) sendListChangedNotifications(mu *sync.Mutex) {
	notifications := []string{
		`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`,
		`{"jsonrpc":"2.0","method":"notifications/prompts/list_changed"}`,
		`{"jsonrpc":"2.0","method":"notifications/resources/list_changed"}`,
	}
	mu.Lock()
	for _, n := range notifications {
		if _, err := fmt.Fprintf(rc.cfg.Stdout, "%s\n", n); err != nil {
			rc.log.Printf("resilient: failed to send list_changed notification: %v", err)
			break
		}
	}
	mu.Unlock()
	rc.log.Printf("resilient: sent list_changed notifications to CC")
}

// failBufferedRequestsDuringReconnect drains client messages that arrived
// after the reconnect grace window expired. Requests receive an immediate
// JSON-RPC error by original id; notifications/responses are dropped because
// there is no live upstream generation to consume them. This keeps the parent
// MCP host's stdio transport alive while still making individual tool calls
// fail fast and retryable during a backend outage.
func (rc *resilientClient) failBufferedRequestsDuringReconnect(stdoutMu *sync.Mutex) {
	failedRequests := 0
	droppedMessages := 0
	stdoutBroken := false
	for {
		select {
		case data := <-rc.msgFromCC:
			id := extractRequestID(data)
			if id == "" {
				droppedMessages++
				continue
			}
			rc.inflight.Delete(id)
			failedRequests++
			if stdoutBroken {
				continue
			}
			errResp := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"mux backend reconnecting; request was not sent upstream; retry shortly"}}`,
				id,
			)
			stdoutMu.Lock()
			_, err := fmt.Fprintf(rc.cfg.Stdout, "%s\n", errResp)
			stdoutMu.Unlock()
			if err != nil {
				stdoutBroken = true
				rc.log.Printf("resilient: stdout broken while failing reconnect-buffered request: %v", err)
				rc.stdoutOnce.Do(func() { close(rc.stdoutDead) })
			}
		default:
			if failedRequests > 0 || droppedMessages > 0 {
				rc.log.Printf(
					"shim.reconnect.degraded_drain failed_requests=%d dropped_messages=%d",
					failedRequests, droppedMessages,
				)
			}
			return
		}
	}
}

// flushBuffer drains any messages buffered in msgFromCC and writes them to
// the new IPC connection. This replays CC requests that arrived during
// the RECONNECTING state.
func (rc *resilientClient) flushBuffer(conn io.Writer) {
	flushed := 0
	for {
		select {
		case data := <-rc.msgFromCC:
			if err := rc.forwardToIPC(conn, data); err != nil {
				rc.log.Printf("resilient: flush write error: %v", err)
				return
			}
			flushed++
		default:
			if flushed > 0 {
				rc.log.Printf("resilient: flushed %d buffered messages", flushed)
			}
			return
		}
	}
}

// drainOrphanedInflight sends JSON-RPC error responses to CC stdout for any
// requests that were in-flight when the IPC connection died. Without this,
// CC waits forever for responses that will never come (request was sent to
// old upstream which was killed during reconnect).
//
// Stdout-failure handling: a write error on stdout is a terminal condition
// (same rule as runStdoutWriter and the keepalive path). On the FIRST write
// failure we log once, signal stdoutDead so the client exits gracefully,
// and then continue iterating only to drain the inflight map — we do NOT
// attempt further writes, so a broken pipe cannot produce N log lines for
// N orphaned requests.
func (rc *resilientClient) drainOrphanedInflight(stdoutMu *sync.Mutex) {
	var orphaned []string
	rc.inflight.Range(func(key, _ any) bool {
		orphaned = append(orphaned, key.(string))
		return true
	})
	if len(orphaned) == 0 {
		return
	}

	rc.log.Printf("resilient: sending error responses for %d orphaned in-flight requests", len(orphaned))
	stdoutBroken := false
	for _, id := range orphaned {
		// Always clear the inflight entry — drain completes regardless of
		// write outcome so repeated reconnects do not leak tracking state.
		rc.inflight.Delete(id)

		if stdoutBroken {
			// Stdout is dead; don't try to write N more lines only to log
			// N more failures. Keep draining the map though.
			continue
		}

		errResp := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"upstream restarted, request lost during reconnect"}}`,
			id,
		)
		stdoutMu.Lock()
		_, err := fmt.Fprintf(rc.cfg.Stdout, "%s\n", errResp)
		stdoutMu.Unlock()
		if err != nil {
			stdoutBroken = true
			rc.log.Printf("resilient: stdout broken while draining orphaned in-flight requests: %v", err)
			// Same terminal-condition signaling as runStdoutWriter (line ~194):
			// close stdoutDead so the main loop unblocks and exits gracefully.
			rc.stdoutOnce.Do(func() { close(rc.stdoutDead) })
		}
	}
	if stdoutBroken {
		rc.log.Printf("resilient: drainOrphanedInflight: stdout broken — CC may hang on the remaining orphaned requests")
	}
}

// countInflight returns the number of in-flight requests currently tracked.
func (rc *resilientClient) countInflight() int {
	count := 0
	rc.inflight.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// extractRequestID returns the JSON "id" field from a request message (has "method").
// Returns empty string for notifications (no id) or responses.
func extractRequestID(data []byte) string {
	var msg struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Method string          `json:"method,omitempty"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Method == "" || msg.ID == nil {
		return "" // not a request (notification or response)
	}
	return string(msg.ID)
}

// extractResponseID returns the JSON "id" field from a response message (has "result" or "error").
// Returns empty string for requests or notifications.
func extractResponseID(data []byte) string {
	var msg struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.ID == nil {
		return ""
	}
	if msg.Result == nil && msg.Error == nil {
		return "" // request, not response
	}
	return string(msg.ID)
}
