package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

	// Owner idle timeout: prefer MCP_MUX_OWNER_IDLE, fall back to MCP_MUX_GRACE
	// (v0.10.x legacy name). Default 10 minutes (was 30s grace in v0.10.x).
	ownerIdleTimeout := 10 * time.Minute
	if g := os.Getenv("MCP_MUX_OWNER_IDLE"); g != "" {
		if d, err := time.ParseDuration(g); err == nil {
			ownerIdleTimeout = d
		}
	} else if g := os.Getenv("MCP_MUX_GRACE"); g != "" {
		if d, err := time.ParseDuration(g); err == nil {
			ownerIdleTimeout = d
		}
	}

	idleTimeout := 5 * time.Minute
	if t := os.Getenv("MCP_MUX_IDLE_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			idleTimeout = d
		}
	}

	// Debug log to file — daemon is detached, stderr goes nowhere.
	// Rotate: if log > 50MB, shift .1 → .2, current → .1, start fresh.
	// Keeps up to ~2 days of history across restarts (3 files × 50MB max).
	debugLogPath := filepath.Join(os.TempDir(), "mcp-muxd-debug.log")
	if info, err := os.Stat(debugLogPath); err == nil && info.Size() > 50*1024*1024 {
		os.Remove(debugLogPath + ".2")
		os.Rename(debugLogPath+".1", debugLogPath+".2")
		os.Rename(debugLogPath, debugLogPath+".1")
	}
	logFile, err := os.OpenFile(debugLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	var logger *log.Logger
	if err == nil {
		logger = log.New(logFile, "[mcp-muxd] ", log.LstdFlags|log.Lmicroseconds)
		logger.Printf("=== daemon starting, debug log: %s ===", debugLogPath)
	} else {
		logger = log.New(os.Stderr, "[mcp-muxd] ", log.LstdFlags)
	}

	d, err := daemon.New(daemon.Config{
		ControlPath:      ctlPath,
		OwnerIdleTimeout: ownerIdleTimeout,
		IdleTimeout:      idleTimeout,
		Logger:           logger,
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

	// Acquire lock to prevent multiple shims from starting daemon simultaneously.
	// Lock file is NOT deleted — it persists for coordination across shims.
	lockPath := serverid.DaemonLockPath()
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lock.Close()

	if err := lockFile(lock); err != nil {
		// Another shim holds the lock (starting daemon) — wait longer for it.
		// 15s covers: daemon startup + upstream spawn + init response.
		logger.Printf("another shim is starting daemon, waiting...")
		return waitForDaemon(ctlPath, 15*time.Second)
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

	return waitForDaemon(ctlPath, 10*time.Second)
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

// spawnViaDaemon sends a spawn request to the daemon and returns the IPC path and handshake token.
func spawnViaDaemon(command string, args []string, cwd, mode string, env map[string]string, logger *log.Logger) (string, string, error) {
	ctlPath := serverid.DaemonControlPath()

	// Spawn returns immediately after creating the owner — proactive init runs
	// in background. 30s timeout covers daemon processing + upstream process start.
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

	logger.Printf("daemon spawned server %s at %s", resp.ServerID[:8], resp.IPCPath)
	return resp.IPCPath, resp.Token, nil
}

// setSysProcAttr is defined in platform-specific files:
// daemon_windows.go and daemon_unix.go
