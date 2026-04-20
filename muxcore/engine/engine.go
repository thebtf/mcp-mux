// Package engine is the unified entry point for embedding muxcore into any Go
// MCP server. It detects the operating mode (daemon, proxy, or client/shim) and
// runs the appropriate path.
package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// Internal timing defaults for the engine. Kept as named constants (not inline
// literals) so the rationale is discoverable and future tuning has a single
// place to change.
const (
	// defaultReaperInterval is how often the daemon reaper scans for dead or
	// idle owners. 10s balances responsiveness against scan cost.
	defaultReaperInterval = 10 * time.Second

	// daemonStartupTimeout is the maximum time startDaemon waits for the
	// newly spawned daemon process to become reachable on its control socket.
	daemonStartupTimeout = 10 * time.Second

	// daemonPollInterval is how often startDaemon probes the control socket
	// while waiting for the daemon to come up.
	daemonPollInterval = 100 * time.Millisecond

	// spawnRPCTimeout covers daemon processing + upstream process start +
	// proactive MCP init handshake for the "spawn" control command.
	spawnRPCTimeout = 30 * time.Second
)

// Mode is the runtime role of the engine selected by Run().
type Mode int

const (
	// ModeUnset is the zero value before Run() has chosen a mode.
	ModeUnset Mode = iota
	// ModeDaemon is the global daemon process that manages owners.
	ModeDaemon
	// ModeClient is a shim that talks to an external daemon over IPC.
	ModeClient
	// ModeProxy is a pass-through between a parent mcp-mux shim and this process.
	ModeProxy
)

// String returns a human-readable name for the Mode.
func (m Mode) String() string {
	switch m {
	case ModeDaemon:
		return "daemon"
	case ModeClient:
		return "client"
	case ModeProxy:
		return "proxy"
	default:
		return "unset"
	}
}

// Handler is the MCP server implementation function.
// When running in daemon mode, this is called with the upstream's stdin/stdout.
// When running in proxy mode, this is called with the CC session's stdin/stdout.
type Handler func(ctx context.Context, stdin io.Reader, stdout io.Writer) error

// Config configures the muxcore engine.
type Config struct {
	// Name is used for log prefixes and socket file naming.
	// Required.
	Name string

	// Command and Args define the upstream MCP server to spawn.
	// Used in daemon mode to start the real server process.
	// If Handler is set, Command/Args are ignored (server runs in-process).
	Command string
	Args    []string

	// Handler is the in-process MCP server implementation.
	// If set, the daemon runs the handler instead of spawning a subprocess.
	// Mutually exclusive with Command (if both set, Handler wins).
	Handler Handler

	// SessionHandler is a structured in-process MCP server implementation.
	// When set, Owner calls HandleRequest directly for each downstream request
	// instead of routing through a pipe or subprocess.
	// Mutually exclusive with Handler and Command.
	// If both Handler and SessionHandler are set, SessionHandler takes priority.
	SessionHandler muxcore.SessionHandler

	// IdleTimeout is how long the daemon waits with zero sessions before exiting.
	// Default: 5 minutes.
	IdleTimeout time.Duration

	// ProgressInterval is the synthetic progress notification interval.
	// Default: 5 seconds. Range: 1-60 seconds.
	ProgressInterval time.Duration

	// Persistent means the daemon stays alive even with zero sessions.
	// Useful for servers that maintain long-running state (like aimux).
	Persistent bool

	// BaseDir overrides os.TempDir() for socket file locations.
	// Empty string = use system temp dir.
	BaseDir string

	// DaemonFlag is the CLI flag that triggers daemon mode.
	// When the engine re-execs the binary, it appends this flag.
	// Default: "--muxcore-daemon"
	DaemonFlag string

	// Logger for debug output. Uses log.Default() if nil.
	Logger *log.Logger

	// SkipSnapshot disables daemon snapshot loading on startup.
	// Zero value (false) means snapshots are enabled — the normal production
	// behaviour used by aimux / engram / mcp-mux.
	// Set to true for ephemeral daemons (tests, one-shot tools) that should
	// not rehydrate state from prior runs.
	SkipSnapshot bool
}

// MuxEngine manages the muxcore multiplexer lifecycle.
type MuxEngine struct {
	cfg    Config
	logger *log.Logger

	mu    sync.RWMutex
	d     *daemon.Daemon // non-nil only while runDaemon is active
	mode  Mode
	ready chan struct{} // closed once mode is set (and, in daemon mode, the daemon is bound)
}

// New creates a MuxEngine with the given configuration.
// Validates config and applies defaults.
func New(cfg Config) (*MuxEngine, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("engine: Name is required")
	}
	if cfg.Command == "" && cfg.Handler == nil && cfg.SessionHandler == nil {
		return nil, fmt.Errorf("engine: Command, Handler, or SessionHandler is required")
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = 5 * time.Second
	}
	if cfg.DaemonFlag == "" {
		cfg.DaemonFlag = "--muxcore-daemon"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	if cfg.Handler != nil && cfg.SessionHandler != nil {
		logger.Printf("engine: warning: both Handler and SessionHandler set, SessionHandler takes priority")
	}
	return &MuxEngine{cfg: cfg, logger: logger, ready: make(chan struct{})}, nil
}

// isDaemonMode checks whether the current process was invoked with the daemon flag.
func (e *MuxEngine) isDaemonMode() bool {
	for _, arg := range os.Args {
		if arg == e.cfg.DaemonFlag {
			return true
		}
	}
	return false
}

// isProxyMode checks whether the current process is running behind a parent
// mcp-mux shim (identified by the MCP_MUX_SESSION_ID environment variable).
func (e *MuxEngine) isProxyMode() bool {
	return os.Getenv("MCP_MUX_SESSION_ID") != ""
}

// Run detects the operating mode and dispatches to the appropriate path.
//   - If DaemonFlag is in os.Args → daemon mode (manage owners, accept IPC)
//   - If MCP_MUX_SESSION_ID env var is set → proxy mode (pass-through, T025)
//   - Otherwise → client/shim mode (find/start daemon, connect via IPC, T024)
//
// Blocks until ctx is cancelled or the engine exits naturally.
func (e *MuxEngine) Run(ctx context.Context) error {
	if e.isDaemonMode() {
		return e.runDaemon(ctx)
	}
	if e.isProxyMode() {
		return e.runProxy(ctx)
	}
	return e.runClient(ctx)
}

// markReady sets the engine mode and optional daemon reference, then closes the
// ready channel exactly once. Safe to call from any goroutine. If Run() is
// somehow called twice, the second call is a no-op on the channel (avoids panic
// on double-close).
func (e *MuxEngine) markReady(mode Mode, d *daemon.Daemon) {
	e.mu.Lock()
	e.d = d
	e.mode = mode
	e.mu.Unlock()
	// Guard against double close if Run() is invoked more than once.
	select {
	case <-e.ready:
		// already closed — no-op
	default:
		close(e.ready)
	}
}

// Daemon returns the live *daemon.Daemon when this engine is running in daemon
// mode. Returns nil in client/proxy mode and before Ready() fires.
//
// In-process consumers (e.g., aimux health handlers) should prefer this over
// calling control.Send against their own control socket — the socket hop is
// avoidable and introduces failure modes (startup race, pool saturation).
// In client/proxy mode the daemon lives in a different process; use
// ControlSocketPath() + control.Send there.
func (e *MuxEngine) Daemon() *daemon.Daemon {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.d
}

// Mode returns the runtime mode selected by Run().
// Returns ModeUnset until Run() has started dispatching.
func (e *MuxEngine) Mode() Mode {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mode
}

// Ready returns a channel closed once Run() has selected its mode and (in
// daemon mode) the daemon is listening on its control socket. After Ready
// fires, Mode() returns a stable non-Unset value and Daemon() returns the live
// daemon (daemon mode) or nil (client/proxy mode).
func (e *MuxEngine) Ready() <-chan struct{} {
	return e.ready
}

// ControlSocketPath returns the canonical daemon control socket path for this
// engine's Name and BaseDir. Mirrors serverid.DaemonControlPath exactly;
// exposed so consumers do not need to import serverid directly or duplicate the
// BaseDir/Name composition.
func (e *MuxEngine) ControlSocketPath() string {
	return serverid.DaemonControlPath(e.cfg.BaseDir, e.cfg.Name)
}

// runDaemon starts the global daemon that manages owners and accepts IPC
// connections. It mirrors the behaviour of runGlobalDaemon() in cmd/mcp-mux/
// but uses the engine's Config for timeouts and base directory.
func (e *MuxEngine) runDaemon(ctx context.Context) error {
	ctlPath := serverid.DaemonControlPath(e.cfg.BaseDir, e.cfg.Name)

	// Clean stale control socket from a previous daemon crash.
	// On Windows, Unix domain socket files persist after process death.
	// Without cleanup, the new daemon cannot bind → startup fails.
	if _, err := os.Stat(ctlPath); err == nil {
		if !isDaemonRunning(ctlPath) {
			os.Remove(ctlPath)
			e.logger.Printf("removed stale daemon socket: %s", ctlPath)
		}
	}

	// SessionHandler takes priority over Handler when both are set.
	handlerFunc := e.cfg.Handler
	if e.cfg.SessionHandler != nil {
		handlerFunc = nil // SessionHandler is passed separately; clear HandlerFunc
	}
	d, err := daemon.New(daemon.Config{
		ControlPath:      ctlPath,
		OwnerIdleTimeout: e.cfg.IdleTimeout,
		IdleTimeout:      e.cfg.IdleTimeout,
		Logger:           e.logger,
		SkipSnapshot:     e.cfg.SkipSnapshot,
		HandlerFunc:      handlerFunc,
		SessionHandler:   e.cfg.SessionHandler,
	})
	if err != nil {
		return fmt.Errorf("engine daemon: %w", err)
	}

	// Publish the daemon reference and signal readiness. daemon.New() has already
	// bound the control socket, so callers that block on Ready() can immediately
	// call Daemon() and reach a live, accepting daemon.
	e.markReady(ModeDaemon, d)
	defer func() {
		e.mu.Lock()
		e.d = nil
		e.mu.Unlock()
	}()

	reaper := daemon.NewReaper(d, defaultReaperInterval)

	select {
	case <-ctx.Done():
		reaper.Stop()
		d.Shutdown()
		return ctx.Err()
	case <-d.Done():
		reaper.Stop()
		return nil
	}
}

// runClient connects to (or starts) the global daemon and runs as a shim (T024).
//
// Flow:
//  1. Ensure daemon is running (start it if not).
//  2. Send "spawn" to daemon with our server identity.
//  3. Connect to the returned IPC socket.
//  4. Bridge stdin/stdout ↔ IPC with automatic reconnect.
func (e *MuxEngine) runClient(ctx context.Context) error {
	e.markReady(ModeClient, nil)

	ctlPath := serverid.DaemonControlPath(e.cfg.BaseDir, e.cfg.Name)

	// 1. Ensure the daemon is running, starting it if necessary.
	if !isDaemonRunning(ctlPath) {
		e.logger.Printf("engine client: daemon not running, starting...")
		if err := e.startDaemon(); err != nil {
			return fmt.Errorf("engine client: start daemon: %w", err)
		}
	}

	// 2. Determine sharing mode from environment (mirrors cmd/mcp-mux/main.go logic).
	mode := serverid.ModeCwd
	if os.Getenv("MCP_MUX_STATELESS") == "1" {
		mode = serverid.ModeGlobal
	}
	if os.Getenv("MCP_MUX_ISOLATED") == "1" {
		mode = serverid.ModeIsolated
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("engine client: getwd: %w", err)
	}

	// Collect current environment for forwarding to daemon (API keys, config paths, etc.)
	env := collectEnv()

	// 3. Ask the daemon to spawn (or locate) an owner for our server identity.
	ipcPath, token, err := spawnViaDaemon(ctlPath, e.cfg.Command, e.cfg.Args, cwd, string(mode), env, e.logger)
	if err != nil {
		return fmt.Errorf("engine client: spawn: %w", err)
	}

	e.logger.Printf("engine client: connecting to %s (resilient)", ipcPath)

	// 4. Bridge stdin/stdout ↔ IPC with automatic reconnect on IPC failure.
	reconnectFn := func() (string, string, error) {
		// Jitter to spread thundering-herd reconnects from concurrent shims.
		jitter := time.Duration(os.Getpid()%500) * time.Millisecond
		time.Sleep(jitter)

		if !isDaemonRunning(ctlPath) {
			if err := e.startDaemon(); err != nil {
				return "", "", fmt.Errorf("engine client: reconnect: start daemon: %w", err)
			}
		}
		return spawnViaDaemon(ctlPath, e.cfg.Command, e.cfg.Args, cwd, string(mode), env, e.logger)
	}

	return owner.RunResilientClient(owner.ResilientClientConfig{
		Stdin:          os.Stdin,
		Stdout:         os.Stdout,
		InitialIPCPath: ipcPath,
		Token:          token,
		Reconnect:      reconnectFn,
		Logger:         e.logger,
	})
}

// runProxy runs the Handler directly on stdin/stdout (T025).
//
// Used when MCP_MUX_SESSION_ID is set, meaning this process is already running
// behind a parent mcp-mux shim. There is no need to create a daemon or IPC
// layer — the shim above us handles multiplexing.
//
// SessionHandler takes priority over Handler when both are set. If only
// SessionHandler is set (no Handler), proxy mode is unsupported because
// SessionHandler requires per-request dispatch, not a raw stdio bridge.
func (e *MuxEngine) runProxy(ctx context.Context) error {
	e.markReady(ModeProxy, nil)

	// Proxy mode: we are a subprocess of an EXTERNAL parent shim (e.g. the
	// user wrapped us via `mcp-mux <our-binary>`). The parent owns stdio and
	// expects us to serve one request/response per MCP message.
	//
	// SessionHandler cannot serve raw stdio on its own — it needs an Owner
	// with session routing, which only exists in daemon mode. In proxy mode
	// we rely on the legacy Handler callback for stdio I/O. Consumers that
	// use SessionHandler in daemon mode should ALSO keep Handler set as a
	// "proxy mode compatibility" fallback (aimux does exactly this).
	//
	// Historical note: v0.18.0–v0.19.3 had a branch here that logged
	// "SessionHandler set, skipping Handler" and returned nil when both
	// handlers were set, causing the subprocess to exit instantly. That
	// broke every muxcore consumer wrapped by mcp-mux (observed: aimux
	// crash-loop into FAILED state). The branch was based on the wrong
	// mental model — in proxy mode there is no daemon "handling
	// SessionHandler" for us; we ARE the thing being wrapped.
	if e.cfg.Handler == nil {
		if e.cfg.SessionHandler != nil {
			return fmt.Errorf("engine proxy: proxy mode requires Handler; SessionHandler alone cannot serve raw stdio — consumers should keep Handler set for proxy-mode compatibility")
		}
		return fmt.Errorf("engine proxy: Handler is required for proxy mode")
	}
	if e.cfg.SessionHandler != nil {
		e.logger.Printf("engine proxy: both handlers set, using Handler for stdio passthrough (session=%s)", os.Getenv("MCP_MUX_SESSION_ID"))
	} else {
		e.logger.Printf("engine proxy: running handler directly on stdio (session=%s)", os.Getenv("MCP_MUX_SESSION_ID"))
	}
	return e.cfg.Handler(ctx, os.Stdin, os.Stdout)
}

// startDaemon re-execs the current binary with DaemonFlag as a detached background
// process, then polls until the daemon control socket responds (up to
// daemonStartupTimeout).
func (e *MuxEngine) startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	cmd := exec.Command(exe, e.cfg.DaemonFlag)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	setDetached(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon process: %w", err)
	}

	// Release: we don't wait for the daemon — it runs independently.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process: %w", err)
	}

	ctlPath := serverid.DaemonControlPath(e.cfg.BaseDir, e.cfg.Name)
	return waitForDaemon(ctlPath, daemonStartupTimeout)
}

// isDaemonRunning checks whether the daemon control socket responds to ping.
func isDaemonRunning(ctlPath string) bool {
	if !ipc.IsAvailable(ctlPath) {
		return false
	}
	resp, err := control.Send(ctlPath, control.Request{Cmd: "ping"})
	return err == nil && resp.OK
}

// waitForDaemon polls until the daemon control socket responds (up to timeout).
func waitForDaemon(ctlPath string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(daemonPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("daemon did not start within %s", timeout)
		case <-ticker.C:
			if isDaemonRunning(ctlPath) {
				return nil
			}
		}
	}
}

// spawnViaDaemon sends a spawn request to the daemon and returns the IPC path
// and handshake token for the owner that will serve our server identity.
func spawnViaDaemon(ctlPath, command string, args []string, cwd, mode string, env map[string]string, logger *log.Logger) (string, string, error) {
	// spawnRPCTimeout covers daemon processing + upstream process start + proactive init.
	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:     "spawn",
		Command: command,
		Args:    args,
		Cwd:     cwd,
		Mode:    mode,
		Env:     env,
	}, spawnRPCTimeout)
	if err != nil {
		return "", "", fmt.Errorf("spawn via daemon: %w", err)
	}
	if !resp.OK {
		return "", "", fmt.Errorf("daemon spawn failed: %s", resp.Message)
	}

	sid := resp.ServerID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	logger.Printf("engine client: daemon spawned server %s at %s", sid, resp.IPCPath)
	return resp.IPCPath, resp.Token, nil
}

// collectEnv returns the current process environment as a map.
// Used to forward CC-configured env vars (API keys, config paths) to the daemon.
func collectEnv() map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			env[e[:i]] = e[i+1:]
		}
	}
	return env
}
