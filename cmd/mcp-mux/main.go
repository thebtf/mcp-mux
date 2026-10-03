// mcp-mux is a transparent command wrapper that multiplexes MCP server instances.
//
// Usage:
//
//	mcp-mux [flags] <command> [args...]
//	mcp-mux status
//	mcp-mux stop [--drain-timeout 30s] [--force]
//
// mcp-mux wraps any MCP server command. Its local daemon admits and owns the
// upstream process; shim sessions connect through managed IPC. Standalone
// owners cannot bypass daemon maintenance admission.
//
// Example:
//
//	mcp-mux uvx --from git+https://... serena start-mcp-server
//	mcp-mux node D:/Dev/openrouter-mcp/dist/server.js
//	mcp-mux --isolated npx playwright-mcp  # per-session mode
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/internal/mcpserver"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/supervisor"
	"github.com/thebtf/mcp-mux/muxcore/upgrade"
)

// engineName is the stable identifier for this binary's daemon/owner namespace.
// All serverid path helpers use this so every socket, lock, and control file
// is scoped to mcp-mux and cannot collide with other engines (e.g. aimux).
const engineName = "mcp-mux"

// ownSocketPrefix is the prefix for all temp socket/lock files owned by this engine.
// Derived from engineName — single source of truth; use this constant, not the raw string.
const ownSocketPrefix = engineName + "-"

func supervisedDaemonReconnectError(err error) error {
	if errors.Is(err, errLauncherManagedDaemonUnavailable) {
		return fmt.Errorf("%w: %v", owner.ErrReconnectExit, err)
	}
	return err
}

func writeCLIAdmissionError(output io.Writer, err error) {
	var admission *era.AdmissionError
	if !errors.As(err, &admission) {
		return
	}
	_, _ = output.Write(admission.JSONRPCResponse())
	_, _ = output.Write([]byte{'\n'})
}

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
		case "hold", "resume", "renew":
			os.Exit(runMaintenanceCommand(os.Args[1], os.Args[2:], os.Stdout, os.Stderr))
		case "stop":
			stopFlags := flag.NewFlagSet("stop", flag.ExitOnError)
			drainTimeout := stopFlags.Duration("drain-timeout", 30*time.Second, "Drain timeout before force kill")
			force := stopFlags.Bool("force", false, "Force immediate shutdown (no drain)")
			stopFlags.Parse(os.Args[2:])
			os.Exit(runStop(*drainTimeout, *force))
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
	standalone := flag.Bool("daemon", false, "Unsupported: direct headless owners require managed admission")
	mcpProtocol := flag.String("mcp-protocol", "", "MCP protocol era (2026-07-28)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mcp-mux [flags] <command> [args...]")
		fmt.Fprintln(os.Stderr, "       mcp-mux stop [--drain-timeout 30s] [--force]")
		fmt.Fprintln(os.Stderr, "       mcp-mux status")
		fmt.Fprintln(os.Stderr, "       mcp-mux hold <exact-server-id> [--ttl 5m] [--drain-timeout 10s] [--json]")
		fmt.Fprintln(os.Stderr, "       mcp-mux resume <hold-id> [--json]")
		fmt.Fprintln(os.Stderr, "       mcp-mux renew <hold-id> [--ttl 5m] [--json]")
		fmt.Fprintln(os.Stderr, "       mcp-mux upgrade")
		os.Exit(1)
	}
	protocolEra, err := era.ParseProtocolEra(*mcpProtocol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: unsupported MCP protocol %q\n", *mcpProtocol)
		os.Exit(1)
	}
	protocolWire, err := protocolEra.Wire()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: protocol era: %v\n", err)
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
		*standalone = true
	}

	noDaemon := os.Getenv("MCP_MUX_NO_DAEMON") == "1"
	if err := standaloneAdmissionError(noDaemon, *standalone); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: standalone owners require daemon-managed admission\n", lifecycleErrorText(err))
		os.Exit(1)
	}
	policy := era.PolicyLegacyOnly
	if protocolEra == era.EraModern20260728 {
		policy = era.PolicyModern20260728
	}
	selection, err := era.SelectOpening(policy, os.Stdin)
	if err != nil {
		writeCLIAdmissionError(os.Stdout, err)
		fmt.Fprintf(os.Stderr, "error: modern opening admission: %v\n", err)
		os.Exit(1)
	}
	clientStdin := selection.Remainder
	if selection.Frame != nil {
		rawOpening, available := selection.Frame.Take()
		if !available {
			fmt.Fprintln(os.Stderr, "error: modern opening frame unavailable")
			os.Exit(1)
		}
		clientStdin = io.MultiReader(bytes.NewReader(rawOpening), selection.Remainder)
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

	// Log to stderr (CC captures) + optionally to file for debugging shim issues.
	// Set MCP_MUX_SHIM_LOG to a file path to enable shim file logging.
	var logger *log.Logger
	if shimLogPath := os.Getenv("MCP_MUX_SHIM_LOG"); shimLogPath != "" {
		f, err := os.OpenFile(shimLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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

	// Managed daemon admission is mandatory for every upstream launch.
	//
	// Important: daemon mode MUST run before any direct IPC shortcut. A reused
	// daemon-managed owner requires a one-time handshake token, and only the
	// daemon's spawn path can mint and pre-register that token for the shim.
	// Connecting directly to the owner's IPC socket skips that registration and
	// gets rejected as "invalid/missing token".
	if !noDaemon {
		// Consume the one-shot launcher attestation before daemon startup can
		// delay the child beyond the bounded parent listener lifetime.
		launcherLifecycleOK := launcherLifecycleCapable()
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
			daemonIPC, daemonServerID, daemonToken, err := spawnViaDaemonForEra(command, cmdArgs, cwd, modeStr, shimEnv, protocolWire, logger)
			held := errors.Is(err, control.ErrMaintenanceHeld) || errors.Is(err, control.ErrMaintenanceRetirementBlocked) || errors.Is(err, control.ErrMaintenancePersistenceFailed)
			if err != nil && !held {
				if errors.Is(err, era.AdmissionControlEraMismatch) {
					writeCLIAdmissionError(os.Stdout, selection.AdmissionError(era.AdmissionControlEraMismatch))
				}
				logger.Printf("shim startup step=daemon_spawn status=error duration=%v err=%q daemon_required=true",
					time.Since(spawnStart), err.Error())
				os.Exit(1)
			} else {
				if held {
					logger.Printf("shim startup step=daemon_spawn status=held duration=%v err=%q",
						time.Since(spawnStart), err.Error())
				} else {
					logger.Printf("shim startup step=daemon_spawn status=ok duration=%v ipc=%q",
						time.Since(spawnStart), daemonIPC)
				}
				logger.Printf("shim startup step=resilient_begin path=%q total_before_client=%v",
					daemonIPC, time.Since(shimStart))
				// currentIPC/currentToken track the latest successful bind target.
				// They must be mutable closure state so refresh after either a
				// token refresh or fallback spawn uses the latest token/path rather
				// than replaying a consumed token into "unknown token".
				currentIPC := daemonIPC
				currentToken := daemonToken
				currentServerID := daemonServerID
				refreshFn := func() (string, string, error) {
					jitter := time.Duration(os.Getpid()%500) * time.Millisecond
					time.Sleep(jitter)

					if err := ensureDaemon(logger); err != nil {
						return "", "", supervisedDaemonReconnectError(err)
					}
					newToken, err := refreshTokenViaDaemon(currentToken, protocolWire, logger)
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
									return "", "", supervisedDaemonReconnectError(err)
								}
								continue
							}
							return "", "", supervisedDaemonReconnectError(err)
						}
						newIPC, newServerID, newToken, err := spawnViaDaemonWithReasonTimeoutForEra(command, cmdArgs, cwd, modeStr, shimEnv, "fallback_spawn", protocolWire, logger, time.Until(deadline))
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
						currentServerID = newServerID
						currentToken = newToken
						return newIPC, newToken, nil
					}
				}
				idleDelay, dormantGrace := shimLifecycleDurations(os.Getenv)
				lifecycleProtocol := supervisor.Protocol{}
				if launcherLifecycleOK {
					lifecycleProtocol = supervisor.ProtocolV2()
				}
				if !launcherLifecycleOK {
					// A v0.26 launcher cannot understand the private dormant control
					// frames. Bootstrap only when the active child can prove its direct
					// launcher identity; this session stays fail-closed and the next
					// invocation receives the upgraded capability.
					if launcherBootstrapEligible() {
						if updated, bootstrapErr := bootstrapStableLauncher(); bootstrapErr != nil {
							logger.Printf("launcher.bootstrap status=skipped error=%q", bootstrapErr.Error())
						} else if updated {
							logger.Printf("launcher.bootstrap status=updated future_invocations=true")
						}
					}
					dormantGrace = -1
				}
				var suspendGate func() (bool, string, error)
				if idleDelay > 0 {
					suspendGate = func() (bool, string, error) {
						return canSuspendViaDaemon(currentToken, currentServerID)
					}
				}

				resilientStart := time.Now()
				err = owner.RunResilientClient(owner.ResilientClientConfig{
					Stdin:             clientStdin,
					Stdout:            os.Stdout,
					InitialIPCPath:    daemonIPC,
					InitialError:      err,
					Token:             daemonToken,
					ProtocolEra:       protocolEra,
					RefreshToken:      refreshFn,
					Reconnect:         reconnectFn,
					IdleSuspendDelay:  idleDelay,
					IdleSuspendGate:   suspendGate,
					IdleDormantGrace:  dormantGrace,
					LifecycleProtocol: lifecycleProtocol,
					Logger:            logger,
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
}

// runServe starts as an MCP server on stdio, providing control plane tools.
func runServe() {
	logger := log.New(os.Stderr, "[mcp-mux:serve] ", log.LstdFlags)
	srv := mcpserver.NewServer(os.Stdin, os.Stdout, logger)
	if err := srv.Run(); err != nil {
		logger.Printf("serve error: %v", err)
	}
}

func runStop(drainTimeout time.Duration, force bool) int {
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
		if err == nil {
			err = resp.Err()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  daemon: error: %s\n", lifecycleErrorText(err))
			return 1
		} else {
			fmt.Fprintf(os.Stderr, "  daemon: %s\n", resp.Message)
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
		if err := resp.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] shutdown failed: %s\n", shortID, lifecycleErrorText(err))
			if isMaintenanceError(err) {
				return 1
			}
		} else {
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", shortID, resp.Message)
			stopped++
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
	return 0
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
	mutationLock, err := acquireMaintenanceMutation()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: upgrade refused: %s\n", lifecycleErrorText(err))
		os.Exit(1)
	}
	defer mutationLock.Close()

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
		if err := restartDaemonAfterEngineSwitchUnderLock(exe, exe, forceDaemonRestart); err != nil {
			fmt.Fprintf(os.Stderr, "error: daemon restart refused or incomplete: %s\n", lifecycleErrorText(err))
			os.Exit(1)
		}
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
	if err == nil || isMaintenanceError(err) {
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

var (
	statusControlSendWithTimeout = control.SendWithTimeout
	statusDaemonControlTimeout   = 15 * time.Second
	statusDaemonRetryWindow      = 5 * time.Second
	statusDaemonRetryDelay       = 25 * time.Millisecond
	statusSleep                  = time.Sleep
	statusPipeHints              = discoverStatusPipeHints
)

func runStatus() {
	runStatusWithWriters(os.Stdout, os.Stderr)
}

func sanitizeModernStatus(value any) {
	switch value := value.(type) {
	case map[string]any:
		if protocolEra, _ := value["protocol_era"].(string); protocolEra == "2026-07-28" {
			delete(value, "sessions")
			delete(value, "inflight")
			delete(value, "oldest_request_age_ms")
			delete(value, "finalization_error")
			delete(value, "owner_generation")
			delete(value, "restored_from_owner_generation")
			delete(value, "restore_source")
		}
		for _, child := range value {
			sanitizeModernStatus(child)
		}
	case []any:
		for _, child := range value {
			sanitizeModernStatus(child)
		}
	}
}

func runStatusWithWriters(stdout, stderr io.Writer) {
	// Try daemon first
	ctlPath := serverid.DaemonControlPath("", engineName)
	resp, err := queryDaemonStatusForCLI(ctlPath)
	daemonResp, daemonErr := resp, err
	invalidSuccessfulStatus := false
	if err == nil && resp != nil && resp.OK {
		var status map[string]any
		if resp.Data == nil {
			daemonErr = fmt.Errorf("control: read response: invalid JSON: empty response")
			invalidSuccessfulStatus = true
		} else if err := json.Unmarshal(resp.Data, &status); err != nil {
			daemonErr = errors.New("control: read response: invalid JSON")
			invalidSuccessfulStatus = true
		} else if status == nil {
			daemonErr = fmt.Errorf("control: read response: invalid JSON: expected object")
			invalidSuccessfulStatus = true
		} else {
			sanitizeModernStatus(status)
			formatted, err := json.MarshalIndent(status, "", "  ")
			if err != nil {
				daemonErr = errors.New("control: read response: invalid JSON")
				invalidSuccessfulStatus = true
			} else {
				fmt.Fprintln(stdout, string(formatted))
				return
			}
		}
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
	if invalidSuccessfulStatus {
		printStatusUnknown(stdout, daemonResp, daemonErr)
		return
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
			if err := json.Unmarshal(resp.Data, &data); err == nil && data != nil {
				sanitizeModernStatus(data)
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
