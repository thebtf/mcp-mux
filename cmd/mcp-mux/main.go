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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/thebtf/mcp-mux/internal/mcpserver"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/session"
	"github.com/thebtf/mcp-mux/muxcore/upgrade"
)

// engineName is the stable identifier for this binary's daemon/owner namespace.
// All serverid path helpers use this so every socket, lock, and control file
// is scoped to mcp-mux and cannot collide with other engines (e.g. aimux).
const engineName = "mcp-mux"

// ownSocketPrefix is the prefix for all temp socket/lock files owned by this engine.
// Derived from engineName — single source of truth; use this constant, not the raw string.
const ownSocketPrefix = engineName + "-"

func main() {
	if handled, exitCode := maybeRunLauncher(); handled {
		os.Exit(exitCode)
	}

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
			if os.Getenv(envEngineMode) == "1" {
				fmt.Fprintln(os.Stderr, "error: upgrade command is only supported through the stable launcher.")
				os.Exit(1)
			}
			upgradeFlags := flag.NewFlagSet("upgrade", flag.ExitOnError)
			restart := upgradeFlags.Bool("restart", false, "Safely restart daemon after upgrade only when no live sessions are attached")
			forceDaemonRestart := upgradeFlags.Bool("force-daemon-restart", false, "Maintenance: restart daemon even with live sessions; existing old transports may close")
			upgradeFlags.Parse(os.Args[2:])
			runUpgrade(*restart, *forceDaemonRestart)
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

	// Determine sharing mode — env vars take precedence over flags.
	//
	// Default flipped from ModeCwd to ModeGlobal in CR-002 (muxcore-global-
	// first-identity). Original intent of mcp-mux was "one upstream per
	// (cmd, args), isolation as exception" but the historical ModeCwd
	// default produced one upstream per (cmd, args, cwd) and defeated that
	// intent (Engram #244 Bug 1). The new ModeGlobal default restores it:
	// post-init tools/list classification (handled daemon-side) splits
	// genuinely-isolated upstreams off via the admission gate; everything
	// else shares one upstream per (cmd, args).
	//
	// Escape valve: set MCP_MUX_DEFAULT_MODE=cwd to revert to legacy
	// per-cwd behavior (D1 If-Wrong pivot from the spec — emergency
	// rollback without redeploy).
	mode := serverid.ModeGlobal
	if envMode := strings.TrimSpace(strings.ToLower(os.Getenv("MCP_MUX_DEFAULT_MODE"))); envMode != "" {
		switch envMode {
		case "cwd":
			mode = serverid.ModeCwd
		case "git":
			mode = serverid.ModeGit
		case "global":
			mode = serverid.ModeGlobal
		case "isolated":
			mode = serverid.ModeIsolated
		}
	}
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
	ipcPath := serverid.IPCPath("", engineName, sid)
	controlPath := serverid.ControlPath("", engineName, sid)

	// Log to stderr (CC captures) + optionally to file for debugging shim issues.
	// Set MCP_MUX_SHIM_LOG to a file path to enable shim file logging.
	var logger *log.Logger
	if shimLogPath := os.Getenv("MCP_MUX_SHIM_LOG"); shimLogPath != "" {
		f, err := os.OpenFile(shimLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			multi := io.MultiWriter(os.Stderr, f)
			logger = log.New(multi, fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags|log.Lmicroseconds)
		} else {
			logger = log.New(os.Stderr, fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags)
		}
	} else {
		logger = log.New(os.Stderr, fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags)
	}

	// Startup-time diagnostic: measure total shim startup so the
	// per-server jsonl can show slow/hung sessions at a glance.
	shimStart := time.Now()

	// Daemon mode (default): shim → daemon → spawn → connect
	// Disable with MCP_MUX_NO_DAEMON=1 to fall back to legacy per-session owner.
	//
	// Important: daemon mode MUST run before any direct IPC shortcut. A reused
	// daemon-managed owner requires a one-time handshake token, and only the
	// daemon's spawn path can mint and pre-register that token for the shim.
	// Connecting directly to the owner's IPC socket skips that registration and
	// gets rejected as "invalid/missing token".
	noDaemon := os.Getenv("MCP_MUX_NO_DAEMON") == "1"
	if !noDaemon {
		ensureStart := time.Now()
		if err := ensureDaemon(logger); err != nil {
			logger.Printf("shim startup step=ensure_daemon status=error duration=%v err=%q daemon_required=true",
				time.Since(ensureStart), err.Error())
			os.Exit(1)
		} else {
			logger.Printf("shim startup step=ensure_daemon status=ok duration=%v",
				time.Since(ensureStart))
			modeStr := string(mode)
			shimEnv := collectEnv()
			spawnStart := time.Now()
			daemonIPC, daemonToken, err := spawnViaDaemon(command, cmdArgs, cwd, modeStr, shimEnv, logger)
			if err != nil {
				logger.Printf("shim startup step=daemon_spawn status=error duration=%v err=%q daemon_required=true",
					time.Since(spawnStart), err.Error())
				os.Exit(1)
			} else {
				logger.Printf("shim startup step=daemon_spawn status=ok duration=%v ipc=%q",
					time.Since(spawnStart), daemonIPC)
				logger.Printf("shim startup step=resilient_begin path=%q total_before_client=%v",
					daemonIPC, time.Since(shimStart))
				// currentIPC/currentToken track the latest successful bind target.
				// They must be mutable closure state so refresh after either a
				// token refresh or fallback spawn uses the latest token/path rather
				// than replaying a consumed token into "unknown token".
				currentIPC := daemonIPC
				currentToken := daemonToken
				refreshFn := func() (string, string, error) {
					jitter := time.Duration(os.Getpid()%500) * time.Millisecond
					time.Sleep(jitter)

					if err := ensureDaemon(logger); err != nil {
						return "", "", err
					}
					newToken, err := refreshTokenViaDaemon(currentToken, logger)
					if err != nil {
						return "", "", err
					}
					currentToken = newToken
					return currentIPC, newToken, nil
				}
				reconnectFn := func() (string, string, error) {
					// Retry ensureDaemon with jitter to avoid thundering herd.
					// Multiple shims reconnecting simultaneously compete for lock;
					// random delay spreads the load.
					jitter := time.Duration(os.Getpid()%500) * time.Millisecond
					time.Sleep(jitter)

					deadline := time.Now().Add(10 * time.Second)
					for {
						if err := ensureDaemonWithin(logger, time.Until(deadline)); err != nil {
							if isTransientDaemonReconnectErr(err) && time.Now().Before(deadline) {
								logger.Printf("shim.reconnect.fallback_spawn transient=%q retrying", err.Error())
								if !sleepWithin(deadline, 100*time.Millisecond) {
									return "", "", err
								}
								continue
							}
							return "", "", err
						}
						newIPC, newToken, err := spawnViaDaemonWithReasonTimeout(command, cmdArgs, cwd, modeStr, shimEnv, "fallback_spawn", logger, time.Until(deadline))
						if err != nil {
							if isTransientDaemonReconnectErr(err) && time.Now().Before(deadline) {
								logger.Printf("shim.reconnect.fallback_spawn transient=%q retrying", err.Error())
								if !sleepWithin(deadline, 100*time.Millisecond) {
									return "", "", err
								}
								continue
							}
							return "", "", err
						}
						currentIPC = newIPC
						currentToken = newToken
						return newIPC, newToken, nil
					}
				}
				idleDelay, dormantGrace := shimLifecycleDurations(os.Getenv)
				if os.Getenv(envLauncherExe) == "" {
					dormantGrace = -1
				}
				var suspendGate func() (bool, string, error)
				if idleDelay > 0 {
					suspendGate = func() (bool, string, error) {
						return canSuspendViaDaemon(currentToken)
					}
				}

				resilientStart := time.Now()
				err = owner.RunResilientClient(owner.ResilientClientConfig{
					Stdin:            os.Stdin,
					Stdout:           os.Stdout,
					InitialIPCPath:   daemonIPC,
					Token:            daemonToken,
					RefreshToken:     refreshFn,
					Reconnect:        reconnectFn,
					IdleSuspendDelay: idleDelay,
					IdleSuspendGate:  suspendGate,
					IdleDormantGrace: dormantGrace,
					Logger:           logger,
				})
				if err != nil {
					exitCode := resilientClientExitCode(err)
					if errors.Is(err, owner.ErrIdleDormant) {
						logger.Printf("shim startup step=resilient_end status=dormant duration=%v total=%v",
							time.Since(resilientStart), time.Since(shimStart))
						os.Exit(exitCode)
					}
					if strings.Contains(err.Error(), "reconnect timeout") {
						reportReconnectGiveUp("timeout", logger)
					}
					logger.Printf("shim startup step=resilient_end status=error duration=%v err=%q total=%v",
						time.Since(resilientStart), err.Error(), time.Since(shimStart))
					os.Exit(exitCode)
				}
				logger.Printf("shim startup step=resilient_end status=ok duration=%v total=%v reason=session_ended",
					time.Since(resilientStart), time.Since(shimStart))
				return
			}
		}
	}

	// Legacy compatibility fallback: connect directly only after daemon mode is
	// explicitly disabled. This path has no handshake token, so it is only safe
	// for legacy owners.
	ipcProbeStart := time.Now()
	ipcAvail := ipc.IsAvailable(ipcPath)
	logger.Printf("shim startup step=ipc_probe result=%v duration=%v path=%q",
		ipcAvail, time.Since(ipcProbeStart), ipcPath)
	if ipcAvail {
		runClientStart := time.Now()
		logger.Printf("shim startup step=run_client_begin target=existing_owner path=%q", ipcPath)
		if err := owner.RunClient(ipcPath, os.Stdin, os.Stdout); err != nil {
			logger.Printf("shim startup step=run_client_end status=error duration=%v err=%q total=%v",
				time.Since(runClientStart), err.Error(), time.Since(shimStart))
			os.Exit(1)
		}
		logger.Printf("shim startup step=run_client_end status=ok duration=%v total=%v reason=stdin_closed",
			time.Since(runClientStart), time.Since(shimStart))
		return
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
		effectiveIPCPath = filepath.Join(os.TempDir(), fmt.Sprintf("%s%s%s.sock", ownSocketPrefix, sid, pidSuffix))
		effectiveControlPath = filepath.Join(os.TempDir(), fmt.Sprintf("%s%s%s.ctl.sock", ownSocketPrefix, sid, pidSuffix))
	}

	o, err := owner.NewOwner(owner.OwnerConfig{
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
	sess := session.NewSession(os.Stdin, os.Stdout)
	o.AddSession(sess)

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Printf("received signal %v, shutting down", sig)
		o.Shutdown()
	case <-o.Done():
		// Owner shut down (upstream exited)
	case <-sess.Done():
		// Our own session ended (stdin closed)
		logger.Printf("stdin closed, shutting down")
		o.Shutdown()
	}
}

// runLegacyDaemon starts an owner without a stdio session (headless).
// Used by mux_restart to spawn a new owner in the background.
func runLegacyDaemon(args []string, cwd, ipcPath, controlPath, _ string, logger *log.Logger) {
	command := args[0]
	cmdArgs := args[1:]

	env := make(map[string]string)

	o, err := owner.NewOwner(owner.OwnerConfig{
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
		o.Shutdown()
	case <-o.Done():
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
	ctlPath := serverid.DaemonControlPath("", engineName)
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
	daemonControlName := filepath.Base(serverid.DaemonControlPath(tmpDir, engineName))

	// Track which server IDs we've already handled via control socket
	handled := make(map[string]bool)

	// Phase 1: Stop instances with control sockets (new protocol)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ownSocketPrefix) || !strings.HasSuffix(name, ".ctl.sock") {
			continue
		}
		if name == daemonControlName {
			continue
		}

		path := filepath.Join(tmpDir, name)
		id := strings.TrimPrefix(strings.TrimSuffix(name, ".ctl.sock"), ownSocketPrefix)
		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		clientTimeout := drainTimeout + 5*time.Second
		if force {
			clientTimeout = 5 * time.Second
		}

		resp, err := control.SendWithTimeout(path, control.Request{
			Cmd:            "shutdown",
			DrainTimeoutMs: drainMs,
		}, clientTimeout)
		if err != nil {
			dataPath := serverid.IPCPath(tmpDir, engineName, id)
			if ipc.IsAvailable(dataPath) && !force {
				handled[id] = true
				fmt.Fprintf(os.Stderr, "  [%s] control unavailable but data socket is live; skipping stale cleanup and legacy fallback\n", shortID)
				continue
			}
			_ = os.Remove(path)
			_ = os.Remove(dataPath)
			handled[id] = true
			stale++
			fmt.Fprintf(os.Stderr, "  [%s] stale socket removed\n", shortID)
			continue
		}

		handled[id] = true
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
		if !strings.HasPrefix(name, ownSocketPrefix) || !strings.HasSuffix(name, ".sock") {
			continue
		}
		// Skip control sockets (already handled) and lock files
		if strings.HasSuffix(name, ".ctl.sock") || strings.HasSuffix(name, ".lock") {
			continue
		}

		id := strings.TrimPrefix(strings.TrimSuffix(name, ".sock"), ownSocketPrefix)
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

func runUpgrade(restart bool, forceDaemonRestart bool) {
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

	// Guard: verify staged binary differs from current to prevent no-op upgrades.
	// go build -o mcp-mux.exe silently fails when the daemon holds the file lock,
	// leaving the staged binary identical to the running one.
	if sameFile(exe, pendingPath) {
		fmt.Fprintln(os.Stderr, "error: staged binary is identical to current binary (no-op upgrade).")
		fmt.Fprintln(os.Stderr, "This usually means `go build -o mcp-mux.exe` failed silently because")
		fmt.Fprintln(os.Stderr, "the daemon process holds a lock on the file. Build to the staging path:")
		fmt.Fprintf(os.Stderr, "  go build -o %s ./cmd/mcp-mux\n", pendingPath)
		fmt.Fprintln(os.Stderr, "Then run: mcp-mux upgrade --restart")
		os.Exit(1)
	}

	// Zero-downtime upgrade: rename-swap binary WITHOUT stopping daemon or killing connections.
	//
	// On Windows, a running exe CAN be renamed (not deleted/overwritten).
	// Daemon + owners + shims continue running from memory with old code.
	// New shim processes launched after swap use the new binary.
	// Daemon gets new code on next natural restart (idle timeout, CC restart, or explicit stop).
	//
	// NEVER call runStop here — it kills daemon, owners, upstreams, and all sessions.

	oldPath, swapErr := upgrade.Swap(exe, pendingPath)
	if swapErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", swapErr)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "The binary may be locked. This can happen when another mcp-mux")
		fmt.Fprintln(os.Stderr, "process holds a file handle. Try:")
		fmt.Fprintln(os.Stderr, "  1. Close all CC sessions")
		fmt.Fprintln(os.Stderr, "  2. mcp-mux stop --force")
		fmt.Fprintln(os.Stderr, "  3. Retry: mcp-mux upgrade")
		os.Exit(1)
	}

	// Best-effort immediate cleanup of this upgrade's old binary (may be locked — that's fine)
	_ = os.Remove(oldPath)
	// Clean any other stale artefacts from previous upgrades
	upgrade.CleanStale(exe)

	// Report
	ctlPath := serverid.DaemonControlPath("", engineName)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "Upgrade complete: %s swapped.\n", filepath.Base(exe))

	if restart && isDaemonRunning(ctlPath) {
		if !forceDaemonRestart {
			liveSessions, err := launcherDaemonLiveSessionCount(ctlPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Daemon restart deferred: could not prove zero live sessions: %v\n", err)
				fmt.Fprintln(os.Stderr, "Existing host transports are preserved.")
				fmt.Fprintln(os.Stderr, "Run with --force-daemon-restart only during an explicit maintenance window.")
				return
			}
			if liveSessions > 0 {
				fmt.Fprintf(os.Stderr, "Daemon restart deferred: %d live session(s) attached; preserving host transports.\n", liveSessions)
				fmt.Fprintln(os.Stderr, "Daemon can be restarted after sessions drain.")
				fmt.Fprintln(os.Stderr, "Run with --force-daemon-restart only during an explicit maintenance window.")
				return
			}
		}

		// Acquire daemon lock BEFORE sending graceful-restart.
		// This prevents shims from spawning a competing daemon during the restart window.
		// Shims that detect IPC loss will call ensureDaemon → lockFile → block until we release.
		lockPath := serverid.DaemonLockPath("", engineName)
		lock, lockErr := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
		if lockErr == nil {
			if flockErr := lockFile(lock); flockErr == nil {
				defer func() {
					unlockFile(lock)
					lock.Close()
				}()
				fmt.Fprintln(os.Stderr, "Acquired daemon lock (shims blocked from respawning).")
			} else {
				lock.Close()
				fmt.Fprintf(os.Stderr, "Warning: could not acquire daemon lock: %v (proceeding anyway)\n", flockErr)
			}
		}

		// Graceful restart: serialize state snapshot, then shutdown.
		// New daemon loads snapshot → owners restored with cached state → instant reconnect.
		fmt.Fprintln(os.Stderr, "Graceful restart: serializing state...")
		resp, err := control.SendWithTimeout(ctlPath, control.Request{
			Cmd:            "graceful-restart",
			DrainTimeoutMs: 30000,
		}, 60*time.Second)
		if err != nil {
			// Fallback to plain shutdown if graceful-restart not supported (old daemon)
			fmt.Fprintf(os.Stderr, "  graceful-restart not available: %v, falling back to shutdown\n", err)
			resp, err = control.Send(ctlPath, control.Request{Cmd: "shutdown"})
			if err != nil {
				fmt.Fprintf(os.Stderr, "  warning: shutdown failed: %v\n", err)
			}
			waitForDaemonExit(ctlPath, "  Waiting for old daemon to exit...")
		} else if !resp.OK {
			fmt.Fprintf(os.Stderr, "  graceful-restart failed: %s, falling back to shutdown\n", resp.Message)
			control.Send(ctlPath, control.Request{Cmd: "shutdown"})
			waitForDaemonExit(ctlPath, "  Waiting for old daemon to exit...")
		} else {
			waitForDaemonExit(ctlPath, "  snapshot written. Waiting for daemon to exit...")
		}

		// Clean up stale daemon control socket — old daemon may not have removed it.
		if _, statErr := os.Stat(ctlPath); statErr == nil {
			if !isDaemonRunning(ctlPath) {
				_ = os.Remove(ctlPath)
				fmt.Fprintln(os.Stderr, "Cleaned stale daemon control socket.")
			}
		}

		// Clean up stale old binary files from previous upgrades.
		upgrade.CleanStale(exe)

		// Start new daemon while holding lock — shims will connect to it.
		fmt.Fprintln(os.Stderr, "Starting new daemon...")
		if startErr := startDaemonProcess(); startErr != nil {
			fmt.Fprintf(os.Stderr, "  warning: failed to start new daemon: %v\n", startErr)
			fmt.Fprintln(os.Stderr, "  Shims will start it on next reconnect.")
		} else {
			if waitErr := waitForDaemon(ctlPath, 10*time.Second); waitErr != nil {
				fmt.Fprintf(os.Stderr, "  warning: new daemon not ready: %v\n", waitErr)
			} else {
				fmt.Fprintln(os.Stderr, "  New daemon ready. Releasing lock — shims will reconnect.")
			}
		}
		// Lock released by defer — shims unblock and connect to new daemon.
	} else if isDaemonRunning(ctlPath) {
		fmt.Fprintln(os.Stderr, "Daemon running (old code) — all connections preserved.")
		fmt.Fprintln(os.Stderr, "New shims use new binary. Daemon updates on next restart.")
		fmt.Fprintln(os.Stderr, "Use: mcp-mux upgrade --restart after sessions drain, or --force-daemon-restart for maintenance.")
	} else {
		fmt.Fprintln(os.Stderr, "Daemon will start with new code on next tool call.")
	}
}

// waitForDaemonExit polls isDaemonRunning until it returns false or 10s elapse.
// Prints prefix on entry, " done." on success, " timeout (...)" on timeout.
// Checks before sleeping, so a daemon that already exited returns immediately.
func waitForDaemonExit(ctlPath, prefix string) {
	fmt.Fprint(os.Stderr, prefix)
	for i := 0; i < 20; i++ {
		if !isDaemonRunning(ctlPath) {
			fmt.Fprintln(os.Stderr, " done.")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, " timeout (daemon may still be shutting down).")
}

// sameFile returns true if two files have the same size and SHA-256 hash.
func sameFile(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	if infoA.Size() != infoB.Size() {
		return false
	}
	hashA, errA := fileHash(a)
	hashB, errB := fileHash(b)
	if errA != nil || errB != nil {
		return false
	}
	return hashA == hashB
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
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

func isTransientDaemonReconnectErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, daemon.ErrDaemonShuttingDown) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "daemon shutting down") ||
		strings.Contains(msg, "control: dial") ||
		strings.Contains(msg, "control: read response") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "daemon did not start")
}

func sleepWithin(deadline time.Time, requested time.Duration) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	if remaining < requested {
		time.Sleep(remaining)
		return false
	}
	time.Sleep(requested)
	return true
}

var statusControlSendWithTimeout = control.SendWithTimeout
var statusDaemonControlTimeout = 15 * time.Second
var statusDaemonRetryWindow = 5 * time.Second
var statusDaemonRetryDelay = 25 * time.Millisecond
var statusSleep = time.Sleep
var statusPipeHints = discoverStatusPipeHints

func runStatus() {
	runStatusWithWriters(os.Stdout, os.Stderr)
}

func runStatusWithWriters(stdout, stderr io.Writer) {
	// Try daemon first
	ctlPath := serverid.DaemonControlPath("", engineName)
	resp, err := queryDaemonStatusForCLI(ctlPath)
	daemonResp, daemonErr := resp, err
	if err == nil && resp.OK && resp.Data != nil {
		var pretty json.RawMessage
		if json.Valid(resp.Data) {
			pretty = resp.Data
		}
		formatted, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Fprintln(stdout, string(formatted))
		return
	}
	if os.Getenv("MCPMUX_STATUS_TRACE") == "1" {
		if err != nil {
			fmt.Fprintf(stderr, "mcp-mux status trace: daemon_status path=%q error=%v\n", ctlPath, err)
		} else if resp == nil {
			fmt.Fprintf(stderr, "mcp-mux status trace: daemon_status path=%q nil_response\n", ctlPath)
		} else {
			fmt.Fprintf(stderr, "mcp-mux status trace: daemon_status path=%q ok=%v message=%q data_len=%d\n", ctlPath, resp.OK, resp.Message, len(resp.Data))
		}
	}

	// Fallback: legacy per-server scan
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		fmt.Fprintf(stderr, "error reading temp dir: %v\n", err)
		os.Exit(1)
	}

	var results []json.RawMessage
	handled := make(map[string]bool)

	// Phase 1: Query instances with control sockets (rich status)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ownSocketPrefix) || !strings.HasSuffix(name, ".ctl.sock") {
			continue
		}

		path := filepath.Join(tmpDir, name)
		id := strings.TrimPrefix(strings.TrimSuffix(name, ".ctl.sock"), ownSocketPrefix)
		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		handled[id] = true

		resp, err := statusControlSendWithTimeout(path, control.Request{Cmd: "status"}, 5*time.Second)
		if err != nil {
			fmt.Fprintf(stderr, "  [%s] unreachable (stale socket)\n", shortID)
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
		if !strings.HasPrefix(name, ownSocketPrefix) || !strings.HasSuffix(name, ".sock") {
			continue
		}
		if strings.HasSuffix(name, ".ctl.sock") || strings.HasSuffix(name, ".lock") {
			continue
		}

		id := strings.TrimPrefix(strings.TrimSuffix(name, ".sock"), ownSocketPrefix)
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
		if shouldReportStatusUnknown(daemonResp, daemonErr) {
			printStatusUnknown(stdout, daemonResp, daemonErr)
			return
		}
		fmt.Fprintln(stdout, "No active mcp-mux instances found.")
		return
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	fmt.Fprintln(stdout, string(data))
}

func queryDaemonStatusForCLI(ctlPath string) (*control.Response, error) {
	deadline := time.Now().Add(statusDaemonRetryWindow)
	var resp *control.Response
	var err error
	for {
		resp, err = statusControlSendWithTimeout(ctlPath, control.Request{Cmd: "status"}, statusDaemonControlTimeout)
		if err == nil || !isRetryableDaemonStatusError(err) || time.Now().After(deadline) {
			return resp, err
		}
		delay := statusDaemonRetryDelay
		if remaining := time.Until(deadline); remaining < delay {
			delay = remaining
		}
		if delay <= 0 {
			return resp, err
		}
		statusSleep(delay)
	}
}

func isRetryableDaemonStatusError(err error) bool {
	if err == nil {
		return false
	}
	if ipc.IsEndpointOccupiedError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "all pipe instances are busy") ||
		strings.Contains(msg, "error_pipe_busy") ||
		strings.Contains(msg, "pipe busy") ||
		strings.Contains(msg, "access is denied")
}

func shouldReportStatusUnknown(resp *control.Response, err error) bool {
	if err != nil {
		return isAmbiguousDaemonStatusError(err)
	}
	return resp != nil && !resp.OK
}

func isAmbiguousDaemonStatusError(err error) bool {
	if err == nil {
		return false
	}
	if ipc.IsEndpointOccupiedError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "read response") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline") ||
		strings.Contains(msg, "closed") ||
		strings.Contains(msg, "pipe busy") ||
		strings.Contains(msg, "access is denied")
}

func printStatusUnknown(stdout io.Writer, resp *control.Response, err error) {
	if err != nil {
		fmt.Fprintf(stdout, "mcp-mux status unavailable: daemon control query failed: %v\n", err)
	} else {
		fmt.Fprintf(stdout, "mcp-mux status unavailable: daemon control query returned OK=false: %s\n", resp.Message)
	}
	fmt.Fprintln(stdout, "Active state is unknown; not reporting an empty instance set.")

	hints, hintErr := statusPipeHints()
	if hintErr != nil {
		fmt.Fprintf(stdout, "Named-pipe hint scan failed: %v\n", hintErr)
		return
	}
	if len(hints) == 0 {
		return
	}
	fmt.Fprintf(stdout, "Found %d mcp-mux named-pipe endpoint(s), which indicates live or recently-live transports.\n", len(hints))
	limit := len(hints)
	if limit > 10 {
		limit = 10
	}
	for _, hint := range hints[:limit] {
		fmt.Fprintf(stdout, "  %s\n", hint)
	}
	if len(hints) > limit {
		fmt.Fprintf(stdout, "  ... %d more\n", len(hints)-limit)
	}
}

func discoverStatusPipeHints() ([]string, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	entries, err := os.ReadDir(`\\.\pipe\`)
	if err != nil {
		return nil, err
	}
	var hints []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "mcp-mux-") {
			hints = append(hints, name)
		}
	}
	sort.Strings(hints)
	return hints, nil
}
