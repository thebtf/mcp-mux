package owner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
)

const (
	defaultReconnectTimeout  = 30 * time.Second
	defaultKeepaliveInterval = 5 * time.Second
	reconnectPollInterval    = 500 * time.Millisecond
	msgFromCCBufferSize      = 1000
	msgFromIPCBufferSize     = 1000
)

// ReconnectFunc reconnects to the daemon and returns the new IPC path and handshake token.
// Called when the IPC connection to the owner breaks.
// Must handle: ensureDaemon + spawnViaDaemon + return ipcPath, token.
type ReconnectFunc func() (ipcPath string, token string, err error)

// ResilientClientConfig configures the resilient shim proxy.
type ResilientClientConfig struct {
	Stdin             io.Reader
	Stdout            io.Writer
	InitialIPCPath    string
	Token             string        // handshake token from initial spawn; sent to owner on connect
	Reconnect         ReconnectFunc
	ReconnectTimeout  time.Duration // default: 30s
	KeepaliveInterval time.Duration // default: 5s
	ProbeGracePeriod  time.Duration // default: 10s; 0 disables probe detection
	Logger            *log.Logger
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
	token      string     // current handshake token; updated on reconnect
	msgFromCC  chan []byte // stdin → proxy (buffered 1000)
	msgFromIPC chan []byte // ipc → stdout (buffered 1000)
	ipcEOF      chan struct{} // closed when IPC reader detects EOF/error
	stdoutDead  chan struct{} // closed when CC stdout pipe breaks
	stdoutOnce  sync.Once    // ensures stdoutDead is closed once
	startTime   time.Time    // when the client started (for probe detection)
	ipcMsgSent  atomic.Bool  // true after first IPC→stdout message forwarded (disables probe detection)
	initCache   initCache
	inflight    sync.Map     // request ID (json.RawMessage string) → true; tracks sent-but-unanswered
	log         *log.Logger
}

// ErrReconnectExit is returned by reconnect when the shim should exit
// so CC restarts it with a fresh MCP handshake.
// ErrReconnectExit is returned when the shim should exit after successful reconnect
// so CC restarts it with a fresh MCP handshake.
var ErrReconnectExit = errors.New("reconnect: exit for fresh handshake")

// RunResilientClient proxies CC stdio ↔ IPC with automatic reconnect on IPC failure.
//
// State machine:
//
//	CONNECTED     — normal proxy: stdin→IPC, IPC→stdout
//	RECONNECTING  — IPC broken; buffer stdin, keepalive to stdout, poll Reconnect()
//	EXIT          — reconnect timed out; return error
//
// Returns only on: CC stdin EOF (io.EOF), or reconnect timeout (error).
func RunResilientClient(cfg ResilientClientConfig) error {
	if cfg.ReconnectTimeout == 0 {
		cfg.ReconnectTimeout = defaultReconnectTimeout
	}
	if cfg.KeepaliveInterval == 0 {
		cfg.KeepaliveInterval = defaultKeepaliveInterval
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
				rc.log.Printf("resilient: stdin EOF after %.1fs (data_sent=%v), exiting immediately",
					time.Since(rc.startTime).Seconds(), rc.ipcMsgSent.Load())
			}
			// Exit — either normal disconnect, probe confirmed dead, or grace expired.
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
				return err // timeout or stdin EOF during reconnect
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
			// Track request IDs (messages with "id" field that are not responses)
			if id := extractRequestID(data); id != "" {
				rc.inflight.Store(id, true)
				rc.log.Printf("resilient: forwarding request id=%s (%d bytes) to IPC", id, len(data))
			}
			_, err := fmt.Fprintf(conn, "%s\n", data)
			if err != nil {
				rc.log.Printf("resilient: ipc write error: %v", err)
				return
			}
		}
	}
}

// reconnectResult carries the result of an async Reconnect() attempt.
type reconnectResult struct {
	path  string
	token string
	err   error
}

// reconnect enters the RECONNECTING state: polls cfg.Reconnect every 500ms
// (asynchronously so the select loop stays responsive), sends keepalive to
// CC stdout every KeepaliveInterval, and buffers CC stdin via msgFromCC.
//
// On success: dials new IPC, replays cached initialize, flushes buffered
// CC messages, and returns the new connection.
//
// Returns error on: reconnect timeout, or CC stdin EOF during reconnect.
func (rc *resilientClient) reconnect(stdoutMu *sync.Mutex, stdinDone <-chan error) (interface {
	io.Reader
	io.Writer
	io.Closer
}, error) {
	deadline := time.Now().Add(rc.cfg.ReconnectTimeout)
	keepaliveTicker := time.NewTicker(rc.cfg.KeepaliveInterval)
	pollTicker := time.NewTicker(reconnectPollInterval)
	defer keepaliveTicker.Stop()
	defer pollTicker.Stop()

	keepaliveN := 0
	resultCh := make(chan reconnectResult, 1)
	pending := false // true when an async Reconnect() call is in-flight

	for {
		// Check timeout.
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("resilient: reconnect timeout after %s", rc.cfg.ReconnectTimeout)
		}

		select {
		case err := <-stdinDone:
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("resilient: stdin during reconnect: %w", err)

		case <-keepaliveTicker.C:
			keepaliveN++
			ka := fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"mux-reconnect","progress":%d,"total":100}}`,
				keepaliveN,
			)
			stdoutMu.Lock()
			_, err := fmt.Fprintf(rc.cfg.Stdout, "%s\n", ka)
			stdoutMu.Unlock()
			if err != nil {
				// CC stdout broken pipe — CC is gone, exit to prevent zombie shim.
				rc.log.Printf("resilient: keepalive write failed (CC gone): %v", err)
				return nil, fmt.Errorf("resilient: CC stdout closed")
			}
			rc.log.Printf("resilient: sent keepalive %d", keepaliveN)

		case <-pollTicker.C:
			if pending {
				// Previous Reconnect() call still in flight — skip this tick.
				continue
			}
			pending = true
			go func() {
				newPath, newToken, err := rc.cfg.Reconnect()
				resultCh <- reconnectResult{path: newPath, token: newToken, err: err}
			}()

		case res := <-resultCh:
			pending = false
			if res.err != nil {
				rc.log.Printf("resilient: Reconnect() failed: %v (retrying)", res.err)
				continue
			}

			conn, err := ipc.Dial(res.path)
			if err != nil {
				rc.log.Printf("resilient: dial %s failed: %v (retrying)", res.path, err)
				continue
			}

			// Update and send the new handshake token before replaying init.
			rc.token = res.token
			if rc.token != "" {
				if _, err := fmt.Fprintf(conn, "%s\n", rc.token); err != nil {
					rc.log.Printf("resilient: send token on reconnect failed: %v (retrying)", err)
					conn.Close()
					continue
				}
			}

			// Replay cached initialize request to warm new daemon.
			if err := rc.replayInit(conn); err != nil {
				rc.log.Printf("resilient: init replay failed: %v (retrying)", err)
				conn.Close()
				continue
			}

			// Send error responses for in-flight requests that were lost
			// when the old IPC connection died. Without this, CC waits
			// forever for responses that will never come.
			rc.drainOrphanedInflight(stdoutMu)

			// Flush buffered CC messages that arrived during RECONNECTING.
			rc.flushBuffer(conn)

			// Notify CC that upstream capabilities may have changed.
			// This triggers CC to re-fetch tools/list, prompts/list, resources/list.
			rc.sendListChangedNotifications(stdoutMu)

			return conn, nil
		}
	}
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

// flushBuffer drains any messages buffered in msgFromCC and writes them to
// the new IPC connection. This replays CC requests that arrived during
// the RECONNECTING state.
func (rc *resilientClient) flushBuffer(conn io.Writer) {
	flushed := 0
	for {
		select {
		case data := <-rc.msgFromCC:
			_, err := fmt.Fprintf(conn, "%s\n", data)
			if err != nil {
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
