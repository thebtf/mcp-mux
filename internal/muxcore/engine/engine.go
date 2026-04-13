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
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/daemon"
	"github.com/thebtf/mcp-mux/internal/muxcore/serverid"
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

// runClient connects to (or starts) the global daemon and runs as a shim.
// Implemented in T024.
func (e *MuxEngine) runClient(ctx context.Context) error {
	return fmt.Errorf("engine: client/shim mode not yet implemented (T024)")
}

// runProxy runs as a pass-through proxy when behind a parent mcp-mux shim.
// Implemented in T025.
func (e *MuxEngine) runProxy(ctx context.Context) error {
	return fmt.Errorf("engine: proxy mode not yet implemented (T025)")
}
