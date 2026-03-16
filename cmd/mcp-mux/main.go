// mcp-mux is a transparent command wrapper that multiplexes MCP server instances.
//
// Usage:
//
//	mcp-mux [flags] <command> [args...]
//	mcp-mux status
//	mcp-mux stop [--drain-timeout 30s] [--force]
//
// mcp-mux wraps any MCP server command. The first instance for a given server
// becomes the "owner" (spawns the real process, listens on IPC). Subsequent
// instances connect as clients, sharing the single upstream process.
//
// Example:
//
//	mcp-mux uvx --from git+https://... serena start-mcp-server
//	mcp-mux node D:/Dev/openrouter-mcp/dist/server.js
//	mcp-mux --isolated npx playwright-mcp  # per-session mode
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bitswan-space/mcp-mux/internal/control"
	"github.com/bitswan-space/mcp-mux/internal/ipc"
	"github.com/bitswan-space/mcp-mux/internal/mcpserver"
	"github.com/bitswan-space/mcp-mux/internal/mux"
	"github.com/bitswan-space/mcp-mux/internal/serverid"
)

func main() {
	// Check for subcommands BEFORE flag.Parse() — subcommands have their own flags.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "status":
			runStatus()
			return
		case "stop":
			stopFlags := flag.NewFlagSet("stop", flag.ExitOnError)
			drainTimeout := stopFlags.Duration("drain-timeout", 30*time.Second, "Drain timeout before force kill")
			force := stopFlags.Bool("force", false, "Force immediate shutdown (no drain)")
			stopFlags.Parse(os.Args[2:])
			runStop(*drainTimeout, *force)
			return
		case "upgrade":
			upgradeFlags := flag.NewFlagSet("upgrade", flag.ExitOnError)
			drainTimeout := upgradeFlags.Duration("drain-timeout", 30*time.Second, "Drain timeout before force kill")
			force := upgradeFlags.Bool("force", false, "Force immediate shutdown (no drain)")
			upgradeFlags.Parse(os.Args[2:])
			runUpgrade(*drainTimeout, *force)
			return
		case "serve":
			runServe()
			return
		case "daemon":
			runGlobalDaemon()
			return
		}
	}

	isolated := flag.Bool("isolated", false, "Run in isolated mode (dedicated upstream per client)")
	stateless := flag.Bool("stateless", false, "Ignore cwd in server identity (for stateless servers like time, tavily)")
	daemon := flag.Bool("daemon", false, "Run as headless owner (no stdio session, for restart)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mcp-mux [flags] <command> [args...]")
		fmt.Fprintln(os.Stderr, "       mcp-mux stop [--drain-timeout 30s] [--force]")
		fmt.Fprintln(os.Stderr, "       mcp-mux status")
		fmt.Fprintln(os.Stderr, "       mcp-mux upgrade")
		os.Exit(1)
	}

	// Determine sharing mode — env vars take precedence over flags
	mode := serverid.ModeCwd
	if *stateless || os.Getenv("MCP_MUX_STATELESS") == "1" {
		mode = serverid.ModeGlobal
	}
	if *isolated || os.Getenv("MCP_MUX_ISOLATED") == "1" {
		mode = serverid.ModeIsolated
	}
	if os.Getenv("MCP_MUX_ISOLATED") == "1" {
		*isolated = true
	}
	if os.Getenv("MCP_MUX_DAEMON") == "1" {
		*daemon = true
	}

	// Get current working directory
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting cwd: %v\n", err)
		os.Exit(1)
	}

	// Compute server identity
	command := args[0]
	cmdArgs := args[1:]
	sid := serverid.GenerateContextKey(mode, command, cmdArgs, nil, cwd)
	ipcPath := serverid.IPCPath(sid)
	controlPath := serverid.ControlPath(sid)

	logger := log.New(os.Stderr, fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags)

	// In isolated mode, always become owner (skip IPC check)
	if *isolated {
		logger.Printf("isolated mode: starting dedicated upstream")
		runOwner(args, cwd, ipcPath, controlPath, sid, logger, true)
		return
	}

	// Fast path: per-server IPC socket exists → connect as client (works with both legacy and daemon)
	if ipc.IsAvailable(ipcPath) {
		logger.Printf("connecting to existing owner at %s", ipcPath)
		if err := mux.RunClient(ipcPath, os.Stdin, os.Stdout); err != nil {
			logger.Printf("client error: %v", err)
			os.Exit(1)
		}
		return
	}

	// Daemon mode (default): shim → daemon → spawn → connect
	// Disable with MCP_MUX_NO_DAEMON=1 to fall back to legacy per-session owner.
	if os.Getenv("MCP_MUX_NO_DAEMON") != "1" {
		if err := ensureDaemon(logger); err != nil {
			logger.Printf("daemon unavailable: %v, falling back to legacy owner", err)
		} else {
			modeStr := string(mode)
			// Pass current process env to daemon so upstream inherits CC-configured vars
			// (e.g., CCLSP_CONFIG_PATH, NIA_API_KEY, TAVILY_API_KEY).
			// env is NOT used in server ID hash — only command+args+cwd determine identity.
			daemonIPC, err := spawnViaDaemon(command, cmdArgs, cwd, modeStr, collectEnv(), logger)
			if err != nil {
				logger.Printf("daemon spawn failed: %v, falling back to legacy owner", err)
			} else {
				logger.Printf("connecting via daemon to %s", daemonIPC)
				if err := mux.RunClient(daemonIPC, os.Stdin, os.Stdout); err != nil {
					logger.Printf("client error: %v", err)
					os.Exit(1)
				}
				return
			}
		}
	}

	// Legacy fallback: become owner directly (MCP_MUX_NO_DAEMON=1)
	logger.Printf("becoming owner for %s (cwd: %s, mode: %s)", serverid.DescribeArgs(args), cwd, mode)
	if *daemon {
		runLegacyDaemon(args, cwd, ipcPath, controlPath, sid, logger)
	} else {
		runOwner(args, cwd, ipcPath, controlPath, sid, logger, *isolated)
	}
}

func runOwner(args []string, cwd, ipcPath, controlPath, sid string, logger *log.Logger, isolated bool) {
	command := args[0]
	cmdArgs := args[1:]

	// Collect environment variables that were passed to us
	env := make(map[string]string)
	// MCP servers receive env from their config — those are passed through
	// our environment. We don't filter here; the upstream inherits our full env
	// via upstream.Start which uses os.Environ().

	effectiveIPCPath := ipcPath
	effectiveControlPath := controlPath
	if isolated {
		// In isolated mode, embed PID into the server ID portion (before extension)
		// so suffix matching (.sock, .ctl.sock) still works for stop/status commands.
		pidSuffix := fmt.Sprintf("-%d", os.Getpid())
		effectiveIPCPath = filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s%s.sock", sid, pidSuffix))
		effectiveControlPath = filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s%s.ctl.sock", sid, pidSuffix))
	}

	owner, err := mux.NewOwner(mux.OwnerConfig{
		Command:     command,
		Args:        cmdArgs,
		Env:         env,
		Cwd:         cwd,
		IPCPath:     effectiveIPCPath,
		ControlPath: effectiveControlPath,
		Logger:      logger,
	})
	if err != nil {
		logger.Fatalf("failed to start owner: %v", err)
	}

	// Add our own stdio as the first session
	session := mux.NewSession(os.Stdin, os.Stdout)
	owner.AddSession(session)

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Printf("received signal %v, shutting down", sig)
		owner.Shutdown()
	case <-owner.Done():
		// Owner shut down (upstream exited)
	case <-session.Done():
		// Our own session ended (stdin closed)
		logger.Printf("stdin closed, shutting down")
		owner.Shutdown()
	}
}

// runLegacyDaemon starts an owner without a stdio session (headless).
// Used by mux_restart to spawn a new owner in the background.
func runLegacyDaemon(args []string, cwd, ipcPath, controlPath, _ string, logger *log.Logger) {
	command := args[0]
	cmdArgs := args[1:]

	env := make(map[string]string)

	owner, err := mux.NewOwner(mux.OwnerConfig{
		Command:     command,
		Args:        cmdArgs,
		Env:         env,
		Cwd:         cwd,
		IPCPath:     ipcPath,
		ControlPath: controlPath,
		Logger:      logger,
	})
	if err != nil {
		logger.Fatalf("failed to start daemon owner: %v", err)
	}

	logger.Printf("daemon owner started (no stdio session)")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Printf("received signal %v, shutting down", sig)
		owner.Shutdown()
	case <-owner.Done():
		// Owner shut down (upstream exited)
	}
}

// runServe starts as an MCP server on stdio, providing control plane tools.
func runServe() {
	logger := log.New(os.Stderr, "[mcp-mux:serve] ", log.LstdFlags)
	srv := mcpserver.NewServer(os.Stdin, os.Stdout, logger)
	if err := srv.Run(); err != nil {
		logger.Printf("serve error: %v", err)
	}
}

func runStop(drainTimeout time.Duration, force bool) {
	// Try stopping daemon first
	ctlPath := serverid.DaemonControlPath()
	if isDaemonRunning(ctlPath) {
		fmt.Fprintln(os.Stderr, "Stopping daemon...")
		drainMs := int(drainTimeout.Milliseconds())
		if force {
			drainMs = 0
		}
		clientTimeout := drainTimeout + 5*time.Second
		if force {
			clientTimeout = 5 * time.Second
		}
		resp, err := control.SendWithTimeout(ctlPath, control.Request{
			Cmd:            "shutdown",
			DrainTimeoutMs: drainMs,
		}, clientTimeout)
		if err == nil && resp.OK {
			fmt.Fprintf(os.Stderr, "  daemon: %s\n", resp.Message)
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "  daemon: error: %v\n", err)
		}
	}

	// Also stop any legacy per-server instances
	fmt.Fprintln(os.Stderr, "Stopping all mcp-mux instances...")

	tmpDir := os.TempDir()
	entries, _ := os.ReadDir(tmpDir)
	stopped := 0
	stale := 0

	drainMs := int(drainTimeout.Milliseconds())
	if force {
		drainMs = 0
	}

	// Track which server IDs we've already handled via control socket
	handled := make(map[string]bool)

	// Phase 1: Stop instances with control sockets (new protocol)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "mcp-mux-") || !strings.HasSuffix(name, ".ctl.sock") {
			continue
		}

		path := filepath.Join(tmpDir, name)
		id := strings.TrimPrefix(strings.TrimSuffix(name, ".ctl.sock"), "mcp-mux-")
		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		handled[id] = true

		clientTimeout := drainTimeout + 5*time.Second
		if force {
			clientTimeout = 5 * time.Second
		}

		resp, err := control.SendWithTimeout(path, control.Request{
			Cmd:            "shutdown",
			DrainTimeoutMs: drainMs,
		}, clientTimeout)
		if err != nil {
			_ = os.Remove(path)
			dataPath := filepath.Join(tmpDir, fmt.Sprintf("mcp-mux-%s.sock", id))
			_ = os.Remove(dataPath)
			stale++
			fmt.Fprintf(os.Stderr, "  [%s] stale socket removed\n", shortID)
			continue
		}

		if resp.OK {
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", shortID, resp.Message)
			stopped++
		} else {
			fmt.Fprintf(os.Stderr, "  [%s] shutdown failed: %s\n", shortID, resp.Message)
		}
	}

	// Phase 2: Fallback for old instances without control sockets
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "mcp-mux-") || !strings.HasSuffix(name, ".sock") {
			continue
		}
		// Skip control sockets (already handled) and lock files
		if strings.HasSuffix(name, ".ctl.sock") || strings.HasSuffix(name, ".lock") {
			continue
		}

		id := strings.TrimPrefix(strings.TrimSuffix(name, ".sock"), "mcp-mux-")
		if handled[id] {
			continue // already stopped via control socket
		}

		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		path := filepath.Join(tmpDir, name)
		conn, err := ipc.Dial(path)
		if err != nil {
			_ = os.Remove(path)
			stale++
			fmt.Fprintf(os.Stderr, "  [%s] stale socket removed (legacy)\n", shortID)
			continue
		}

		// Send legacy mux/shutdown via data socket
		shutdownMsg := `{"jsonrpc":"2.0","method":"mux/shutdown"}` + "\n"
		_, err = conn.Write([]byte(shutdownMsg))
		conn.Close()

		if err != nil {
			_ = os.Remove(path)
			stale++
			fmt.Fprintf(os.Stderr, "  [%s] failed to send shutdown (legacy): %v\n", shortID, err)
		} else {
			fmt.Fprintf(os.Stderr, "  [%s] shutdown signal sent (legacy)\n", shortID)
			stopped++
		}
	}

	if stopped == 0 && stale == 0 {
		fmt.Fprintln(os.Stderr, "No mcp-mux instances found.")
	} else {
		fmt.Fprintf(os.Stderr, "Done: %d stopped, %d stale cleaned.\n", stopped, stale)
	}
}

func runUpgrade(drainTimeout time.Duration, force bool) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot resolve executable path: %v\n", err)
		os.Exit(1)
	}

	pendingPath := exe + "~"

	// Check if a new binary is waiting (built with: go build -o mcp-mux.exe~ ./cmd/mcp-mux)
	if _, err := os.Stat(pendingPath); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "No pending update found. Build the new binary first:")
		fmt.Fprintf(os.Stderr, "  go build -o %s ./cmd/mcp-mux\n", pendingPath)
		fmt.Fprintln(os.Stderr, "Then run: mcp-mux upgrade")
		os.Exit(1)
	}

	// Step 1: Stop all instances (unlocks the binary for rename)
	runStop(drainTimeout, force)

	// Step 2: Atomic swap — rename running exe to .old, pending to current
	// On Windows, a running exe can be renamed but not overwritten.
	// By the time we get here, all mcp-mux processes are stopped, so we can
	// safely overwrite. But we use rename-dance for robustness.
	oldPath := exe + ".old"
	_ = os.Remove(oldPath) // clean up any previous .old

	// Rename current → .old
	if err := os.Rename(exe, oldPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: rename %s → %s: %v\n", exe, oldPath, err)
		fmt.Fprintln(os.Stderr, "Hint: ensure all mcp-mux processes are stopped.")
		os.Exit(1)
	}

	// Rename pending → current
	if err := os.Rename(pendingPath, exe); err != nil {
		// Rollback: restore old binary
		_ = os.Rename(oldPath, exe)
		fmt.Fprintf(os.Stderr, "error: rename %s → %s: %v\n", pendingPath, exe, err)
		os.Exit(1)
	}

	// Cleanup old binary (best-effort)
	_ = os.Remove(oldPath)

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "Upgrade complete: %s replaced.\n", filepath.Base(exe))
	fmt.Fprintln(os.Stderr, "MCP servers will restart automatically on next CC tool call.")
}

// collectEnv returns the current process environment as a map.
// Used to forward CC-configured env vars (API keys, config paths) to daemon spawn.
func collectEnv() map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			env[e[:i]] = e[i+1:]
		}
	}
	return env
}

func runStatus() {
	// Try daemon first
	ctlPath := serverid.DaemonControlPath()
	if isDaemonRunning(ctlPath) {
		resp, err := control.Send(ctlPath, control.Request{Cmd: "status"})
		if err == nil && resp.OK && resp.Data != nil {
			var pretty json.RawMessage
			if json.Valid(resp.Data) {
				pretty = resp.Data
			}
			formatted, _ := json.MarshalIndent(pretty, "", "  ")
			fmt.Println(string(formatted))
			return
		}
	}

	// Fallback: legacy per-server scan
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading temp dir: %v\n", err)
		os.Exit(1)
	}

	var results []json.RawMessage
	handled := make(map[string]bool)

	// Phase 1: Query instances with control sockets (rich status)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "mcp-mux-") || !strings.HasSuffix(name, ".ctl.sock") {
			continue
		}

		path := filepath.Join(tmpDir, name)
		id := strings.TrimPrefix(strings.TrimSuffix(name, ".ctl.sock"), "mcp-mux-")
		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		handled[id] = true

		resp, err := control.Send(path, control.Request{Cmd: "status"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] unreachable (stale socket)\n", shortID)
			continue
		}

		if resp.OK && resp.Data != nil {
			var data map[string]any
			if err := json.Unmarshal(resp.Data, &data); err == nil {
				data["server_id"] = id
				enriched, _ := json.Marshal(data)
				results = append(results, enriched)
			}
		}
	}

	// Phase 2: Fallback for old instances (basic active/stale check)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "mcp-mux-") || !strings.HasSuffix(name, ".sock") {
			continue
		}
		if strings.HasSuffix(name, ".ctl.sock") || strings.HasSuffix(name, ".lock") {
			continue
		}

		id := strings.TrimPrefix(strings.TrimSuffix(name, ".sock"), "mcp-mux-")
		if handled[id] {
			continue
		}

		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		path := filepath.Join(tmpDir, name)
		active := ipc.IsAvailable(path)
		status := map[string]any{
			"server_id": id,
			"ipc_path":  path,
			"active":    active,
			"legacy":    true,
		}
		data, _ := json.Marshal(status)
		results = append(results, data)
	}

	if len(results) == 0 {
		fmt.Println("No active mcp-mux instances found.")
		return
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println(string(data))
}
