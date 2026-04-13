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
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

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
}

// MuxEngine manages the muxcore multiplexer lifecycle.
type MuxEngine struct {
	cfg    Config
	logger *log.Logger
}

// New creates a MuxEngine with the given configuration.
// Validates config and applies defaults.
func New(cfg Config) (*MuxEngine, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("engine: Name is required")
	}
	if cfg.Command == "" && cfg.Handler == nil {
		return nil, fmt.Errorf("engine: Command or Handler is required")
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
	return &MuxEngine{cfg: cfg, logger: logger}, nil
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

// runDaemon starts the global daemon that manages owners and accepts IPC
// connections. It mirrors the behaviour of runGlobalDaemon() in cmd/mcp-mux/
// but uses the engine's Config for timeouts and base directory.
func (e *MuxEngine) runDaemon(ctx context.Context) error {
	ctlPath := serverid.DaemonControlPath(e.cfg.BaseDir)

	d, err := daemon.New(daemon.Config{
		ControlPath:      ctlPath,
		OwnerIdleTimeout: e.cfg.IdleTimeout,
		IdleTimeout:      e.cfg.IdleTimeout,
		Logger:           e.logger,
		SkipSnapshot:     false,
	})
	if err != nil {
		return fmt.Errorf("engine daemon: %w", err)
	}

	reaper := daemon.NewReaper(d, 10*time.Second)

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
	ctlPath := serverid.DaemonControlPath(e.cfg.BaseDir)

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
func (e *MuxEngine) runProxy(ctx context.Context) error {
	if e.cfg.Handler == nil {
		return fmt.Errorf("engine proxy: Handler is required for proxy mode")
	}
	e.logger.Printf("engine proxy: running handler directly on stdio (session=%s)", os.Getenv("MCP_MUX_SESSION_ID"))
	return e.cfg.Handler(ctx, os.Stdin, os.Stdout)
}

// startDaemon re-execs the current binary with DaemonFlag as a detached background
// process, then polls until the daemon control socket responds (up to 10 seconds).
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

	ctlPath := serverid.DaemonControlPath(e.cfg.BaseDir)
	return waitForDaemon(ctlPath, 10*time.Second)
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
	ticker := time.NewTicker(100 * time.Millisecond)
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
	// 30s covers daemon processing + upstream process start + proactive init.
	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:     "spawn",
		Command: command,
		Args:    args,
		Cwd:     cwd,
		Mode:    mode,
		Env:     env,
	}, 30*time.Second)
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
