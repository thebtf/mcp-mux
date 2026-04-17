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

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// runGlobalDaemon starts the global daemon process. This is invoked via
// `mcp-mux daemon` subcommand. The daemon manages all upstream MCP servers,
// accepts spawn requests via its control socket, and auto-exits after idle timeout.
func runGlobalDaemon() {
	ctlPath := serverid.DaemonControlPath("", "")

	// Single-daemon validation: if another daemon is already running on this
	// control socket, exit immediately instead of competing.
	if isDaemonRunning(ctlPath) {
		log.Printf("another daemon is already running on %s, exiting", ctlPath)
		os.Exit(0)
	}

	// Owner idle timeout: prefer MCP_MUX_OWNER_IDLE, fall back to MCP_MUX_GRACE
	// (v0.10.x legacy name). Default 10 minutes (was 30s grace in v0.10.x).
	ownerIdleTimeout := 10 * time.Minute
	if g := os.Getenv("MCP_MUX_OWNER_IDLE"); g != "" {
		if d, err := time.ParseDuration(g); err == nil {
			ownerIdleTimeout = d
		} else {
			log.Printf("warning: invalid MCP_MUX_OWNER_IDLE=%q (%v), using default %s", g, err, ownerIdleTimeout)
		}
	} else if g := os.Getenv("MCP_MUX_GRACE"); g != "" {
		if d, err := time.ParseDuration(g); err == nil {
			ownerIdleTimeout = d
		} else {
			log.Printf("warning: invalid MCP_MUX_GRACE=%q (%v), using default %s", g, err, ownerIdleTimeout)
		}
	}

	idleTimeout := 5 * time.Minute
	if t := os.Getenv("MCP_MUX_IDLE_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			idleTimeout = d
		} else {
			log.Printf("warning: invalid MCP_MUX_IDLE_TIMEOUT=%q (%v), using default %s", t, err, idleTimeout)
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
//
// Emits structured "ensure_daemon substep=<name>" log lines so the shim's
// startup timeline can be reconstructed from the CC mcp-logs jsonl.
func ensureDaemon(logger *log.Logger) error {
	ctlPath := serverid.DaemonControlPath("", "")

	// Fast path: daemon already running (no lock needed).
	// Log lines use key=value with no trailing prose so post-mortem grep/awk
	// can parse them without surprises.
	pingStart := time.Now()
	if isDaemonRunning(ctlPath) {
		logger.Printf("ensure_daemon substep=fast_ping status=ok duration=%v",
			time.Since(pingStart))
		return nil
	}
	logger.Printf("ensure_daemon substep=fast_ping status=miss duration=%v",
		time.Since(pingStart))

	// Acquire lock to prevent multiple shims from starting daemon simultaneously.
	// Lock file is NOT deleted — it persists for coordination across shims.
	lockPath := serverid.DaemonLockPath("", "")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lock.Close()

	lockStart := time.Now()
	if err := lockFile(lock); err != nil {
		// Another shim holds the lock (starting daemon) — wait longer for it.
		// 15s covers: daemon startup + upstream spawn + init response.
		logger.Printf("ensure_daemon substep=lock_attempt status=contended duration=%v err=%q",
			time.Since(lockStart), err.Error())
		waitStart := time.Now()
		waitErr := waitForDaemon(ctlPath, 15*time.Second)
		if waitErr != nil {
			logger.Printf("ensure_daemon substep=wait_for_other_shim status=error duration=%v err=%q",
				time.Since(waitStart), waitErr.Error())
			return waitErr
		}
		logger.Printf("ensure_daemon substep=wait_for_other_shim status=ok duration=%v",
			time.Since(waitStart))
		return nil
	}
	defer unlockFile(lock)
	logger.Printf("ensure_daemon substep=lock_attempt status=acquired duration=%v",
		time.Since(lockStart))

	// Re-check after acquiring lock (another shim may have started it)
	recheckStart := time.Now()
	if isDaemonRunning(ctlPath) {
		logger.Printf("ensure_daemon substep=lock_recheck status=already_running duration=%v",
			time.Since(recheckStart))
		return nil
	}
	logger.Printf("ensure_daemon substep=lock_recheck status=still_missing duration=%v",
		time.Since(recheckStart))

	// Start daemon as detached process
	spawnStart := time.Now()
	if err := startDaemonProcess(); err != nil {
		logger.Printf("ensure_daemon substep=spawn_process status=error duration=%v err=%q",
			time.Since(spawnStart), err.Error())
		return fmt.Errorf("start daemon: %w", err)
	}
	logger.Printf("ensure_daemon substep=spawn_process status=ok duration=%v",
		time.Since(spawnStart))

	waitStart := time.Now()
	if err := waitForDaemon(ctlPath, 10*time.Second); err != nil {
		logger.Printf("ensure_daemon substep=wait_for_self status=timeout duration=%v err=%q",
			time.Since(waitStart), err.Error())
		return err
	}
	logger.Printf("ensure_daemon substep=wait_for_self status=ok duration=%v",
		time.Since(waitStart))
	return nil
}

// waitForDaemon polls until the daemon control socket responds (up to timeout).
// Counts polling attempts so slow daemon startup is observable via log.
func waitForDaemon(ctlPath string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	attempts := 0
	for {
		select {
		case <-deadline:
			return fmt.Errorf("daemon did not start within %s (polls=%d)", timeout, attempts)
		case <-ticker.C:
			attempts++
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
// Emits a "daemon_rpc_spawn" log line with the total RPC duration for post-mortem latency analysis.
func spawnViaDaemon(command string, args []string, cwd, mode string, env map[string]string, logger *log.Logger) (string, string, error) {
	ctlPath := serverid.DaemonControlPath("", "")

	// Spawn returns immediately after creating the owner — proactive init runs
	// in background. 30s timeout covers daemon processing + upstream process start.
	rpcStart := time.Now()
	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:     "spawn",
		Command: command,
		Args:    args,
		Cwd:     cwd,
		Mode:    mode,
		Env:     env,
	}, 30*time.Second)
	rpcDur := time.Since(rpcStart)
	if err != nil {
		logger.Printf("daemon_rpc_spawn status=error duration=%v err=%q", rpcDur, err.Error())
		return "", "", fmt.Errorf("spawn via daemon: %w", err)
	}
	if !resp.OK {
		logger.Printf("daemon_rpc_spawn status=not_ok duration=%v msg=%q", rpcDur, resp.Message)
		return "", "", fmt.Errorf("daemon spawn failed: %s", resp.Message)
	}

	// Safe ID truncation: resp.ServerID may be shorter than 8 chars in edge cases
	// (test daemons, non-hashed IDs). Matches the pattern used in
	// muxcore/engine/engine.go and muxcore/daemon/snapshot.go.
	shortID := resp.ServerID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	logger.Printf("daemon_rpc_spawn status=ok duration=%v server=%s ipc=%q",
		rpcDur, shortID, resp.IPCPath)
	return resp.IPCPath, resp.Token, nil
}

// setSysProcAttr is defined in platform-specific files:
// daemon_windows.go and daemon_unix.go
