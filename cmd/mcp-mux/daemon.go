package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/thebtf/mcp-mux/internal/control"
	"github.com/thebtf/mcp-mux/internal/daemon"
	"github.com/thebtf/mcp-mux/internal/ipc"
	"github.com/thebtf/mcp-mux/internal/serverid"
)

// runGlobalDaemon starts the global daemon process. This is invoked via
// `mcp-mux daemon` subcommand. The daemon manages all upstream MCP servers,
// accepts spawn requests via its control socket, and auto-exits after idle timeout.
func runGlobalDaemon() {
	ctlPath := serverid.DaemonControlPath()

	gracePeriod := 30 * time.Second
	if g := os.Getenv("MCP_MUX_GRACE"); g != "" {
		if d, err := time.ParseDuration(g); err == nil {
			gracePeriod = d
		}
	}

	idleTimeout := 5 * time.Minute
	if t := os.Getenv("MCP_MUX_IDLE_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			idleTimeout = d
		}
	}

	logger := log.New(os.Stderr, "[mcp-muxd] ", log.LstdFlags)

	d, err := daemon.New(daemon.Config{
		ControlPath: ctlPath,
		GracePeriod: gracePeriod,
		IdleTimeout: idleTimeout,
		Logger:      logger,
	})
	if err != nil {
		logger.Fatalf("failed to start daemon: %v", err)
	}

	// Start reaper
	reaper := daemon.NewReaper(d, 10*time.Second)

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Printf("received signal %v, shutting down", sig)
		reaper.Stop()
		d.Shutdown()
	case <-d.Done():
		// Daemon shut down (idle auto-exit or control command)
		reaper.Stop()
	}
}

// ensureDaemon checks if the global daemon is running. If not, starts it
// as a detached background process. Returns nil when daemon is ready.
// Uses a file lock to prevent multiple shims from spawning concurrent daemons.
func ensureDaemon(logger *log.Logger) error {
	ctlPath := serverid.DaemonControlPath()

	// Fast path: daemon already running (no lock needed)
	if isDaemonRunning(ctlPath) {
		return nil
	}

	// Acquire lock to prevent multiple shims from starting daemon simultaneously
	lockPath := serverid.DaemonLockPath()
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lock.Close()
	defer os.Remove(lockPath)

	if err := lockFile(lock); err != nil {
		// Another shim is starting the daemon — just wait for it
		logger.Printf("another shim is starting daemon, waiting...")
		return waitForDaemon(ctlPath, 5*time.Second)
	}
	defer unlockFile(lock)

	// Re-check after acquiring lock (another shim may have started it)
	if isDaemonRunning(ctlPath) {
		return nil
	}

	// Start daemon as detached process
	logger.Printf("starting daemon...")
	if err := startDaemonProcess(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	return waitForDaemon(ctlPath, 5*time.Second)
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

// isDaemonRunning checks if the daemon control socket responds to ping.
func isDaemonRunning(ctlPath string) bool {
	if !ipc.IsAvailable(ctlPath) {
		return false
	}
	resp, err := control.Send(ctlPath, control.Request{Cmd: "ping"})
	if err != nil {
		return false
	}
	return resp.OK
}

// startDaemonProcess spawns `mcp-mux daemon` as a detached background process.
func startDaemonProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	// Detach from parent process group
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon process: %w", err)
	}

	// Release — we don't wait for the daemon
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process: %w", err)
	}

	return nil
}

// spawnViaDaemon sends a spawn request to the daemon and returns the IPC path.
func spawnViaDaemon(command string, args []string, cwd, mode string, env map[string]string, logger *log.Logger) (string, error) {
	ctlPath := serverid.DaemonControlPath()

	// Spawn may take time — upstream processes like uvx/npx need 5-15s to start.
	// Use 30s timeout instead of default 5s.
	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:     "spawn",
		Command: command,
		Args:    args,
		Cwd:     cwd,
		Mode:    mode,
		Env:     env,
	}, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("spawn via daemon: %w", err)
	}
	if !resp.OK {
		return "", fmt.Errorf("daemon spawn failed: %s", resp.Message)
	}

	logger.Printf("daemon spawned server %s at %s", resp.ServerID[:8], resp.IPCPath)
	return resp.IPCPath, nil
}

// setSysProcAttr is defined in platform-specific files:
// daemon_windows.go and daemon_unix.go
