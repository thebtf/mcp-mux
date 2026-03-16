package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/bitswan-space/mcp-mux/internal/control"
	"github.com/bitswan-space/mcp-mux/internal/daemon"
	"github.com/bitswan-space/mcp-mux/internal/ipc"
	"github.com/bitswan-space/mcp-mux/internal/serverid"
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
func ensureDaemon(logger *log.Logger) error {
	ctlPath := serverid.DaemonControlPath()

	// Fast path: daemon already running
	if isDaemonRunning(ctlPath) {
		return nil
	}

	// Start daemon as detached process
	logger.Printf("starting daemon...")
	if err := startDaemonProcess(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Poll until daemon is ready (up to 5s)
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("daemon did not start within 5s")
		case <-ticker.C:
			if isDaemonRunning(ctlPath) {
				logger.Printf("daemon ready")
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

	resp, err := control.Send(ctlPath, control.Request{
		Cmd:     "spawn",
		Command: command,
		Args:    args,
		Cwd:     cwd,
		Mode:    mode,
		Env:     env,
	})
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
