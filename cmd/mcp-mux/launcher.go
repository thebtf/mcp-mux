package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/procgroup"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/supervisor"
	"github.com/thebtf/mcp-mux/muxcore/supervisor/attest"
	"github.com/thebtf/mcp-mux/muxcore/upgrade"
)

const (
	envEngineMode          = "MCPMUX_ENGINE"
	envDisableLauncher     = "MCPMUX_DISABLE_LAUNCHER"
	envLauncherExe         = "MCPMUX_LAUNCHER_EXE"
	envActiveEngineFile    = "MCPMUX_ACTIVE_ENGINE_FILE"
	envLauncherProtocol    = "MCPMUX_LAUNCHER_PROTOCOL"
	envLauncherAttestation = "MCPMUX_LAUNCHER_ATTESTATION"
	envLauncherTrace       = "MCPMUX_LAUNCHER_TRACE"
	envLauncherOwnsDaemon  = "MCPMUX_LAUNCHER_OWNS_DAEMON"
)

var errLauncherManagedDaemonUnavailable = errors.New("mcp-mux: launcher-managed daemon unavailable")

func maybeRunLauncher() (bool, int) {
	if os.Getenv(envEngineMode) == "1" || os.Getenv(envDisableLauncher) == "1" {
		launcherTracef("bypass env_engine=%q disable=%q args=%v", os.Getenv(envEngineMode), os.Getenv(envDisableLauncher), os.Args[1:])
		return false, 0
	}

	exe, err := os.Executable()
	if err != nil {
		launcherTracef("bypass executable_error=%v args=%v", err, os.Args[1:])
		return false, 0
	}

	if len(os.Args) > 1 && os.Args[1] == "upgrade" {
		launcherTracef("handle upgrade launcher=%s args=%v", exe, os.Args[2:])
		return true, runLauncherUpgrade(exe, os.Args[2:])
	}
	if shouldRunOperatorCommandInLauncher(os.Args[1:]) {
		launcherTracef("bypass operator_command launcher=%s args=%v", exe, os.Args[1:])
		return false, 0
	}

	enginePath, ok := resolveActiveEngine(exe)
	if !ok || samePath(enginePath, exe) {
		launcherTracef("bypass active_engine ok=%v launcher=%s active=%s args=%v", ok, exe, enginePath, os.Args[1:])
		return false, 0
	}

	launcherTracef("handle active_engine launcher=%s active=%s args=%v", exe, enginePath, os.Args[1:])
	return runEngineProcess(exe, enginePath, os.Args[1:])
}

func runEngineProcess(launcherPath, enginePath string, args []string) (bool, int) {
	if shouldRunOperatorCommandInLauncher(args) {
		return false, 0
	}
	if shouldSuperviseEngineProcess(args) {
		return true, runLauncherStdioSupervisor(launcherSupervisorConfig{
			LauncherPath:      launcherPath,
			InitialEnginePath: enginePath,
			Args:              args,
			PrepareStart:      prepareSupervisedEngineStart,
			Stdin:             os.Stdin,
			Stdout:            os.Stdout,
			Stderr:            os.Stderr,
			DormantLease:      launcherDormantLease(os.Getenv),
		})
	}

	process, fallbackToCaller, err := startEngineOrStableLauncher(launcherPath, enginePath, args, true, func(cmd *exec.Cmd) {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	})
	if fallbackToCaller {
		return false, 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-mux launcher: %v\n", err)
		return true, 1
	}
	if err := process.Wait(); err != nil {
		if code := process.ExitCode(); code >= 0 {
			return true, code
		}
		fmt.Fprintf(os.Stderr, "mcp-mux launcher: active engine %s exited with wait error: %v\n", enginePath, err)
		return true, 1
	}
	return true, 0
}

func shouldRunOperatorCommandInLauncher(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "status", "stop":
		return true
	default:
		return false
	}
}

func launcherTracef(format string, args ...any) {
	if os.Getenv(envLauncherTrace) != "1" {
		return
	}
	fmt.Fprintf(os.Stderr, "mcp-mux launcher trace: "+format+"\n", args...)
}

func startEngineOrStableLauncher(launcherPath, enginePath string, args []string, fallbackToCaller bool, configure func(*exec.Cmd)) (*procgroup.Process, bool, error) {
	return startEngineOrStableLauncherContext(context.Background(), launcherPath, enginePath, args, fallbackToCaller, false, configure)
}

func startEngineOrStableLauncherContext(ctx context.Context, launcherPath, enginePath string, args []string, fallbackToCaller, allowSurviveParentExit bool, configure func(*exec.Cmd)) (*procgroup.Process, bool, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, false, err
	}
	var process *procgroup.Process
	var startErr error
	if samePath(enginePath, launcherPath) || authorizeInstalledEnginePath(launcherPath, enginePath) {
		cmd := newLauncherEnvCommand(launcherPath, enginePath, args)
		configure(cmd)
		process, startErr = startLauncherEnvProcessWithParentExit(ctx, cmd, allowSurviveParentExit)
		if startErr == nil {
			return process, false, nil
		}
		if errors.Is(startErr, supervisor.ErrStartRollbackUnproven) {
			return nil, false, startErr
		}
	} else {
		startErr = errors.New("active engine path is not authorized")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, false, err
	}
	if fallbackToCaller {
		fmt.Fprintf(os.Stderr, "mcp-mux launcher: start active engine %s: %v\n", enginePath, startErr)
		fmt.Fprintln(os.Stderr, "mcp-mux launcher: falling back to stable launcher binary")
		return nil, true, nil
	}
	fmt.Fprintf(os.Stderr, "mcp-mux launcher: start active engine %s: %v\n", enginePath, startErr)
	fmt.Fprintln(os.Stderr, "mcp-mux launcher: starting daemon via stable launcher binary")
	cmd := newLauncherEnvCommand(launcherPath, launcherPath, args)
	configure(cmd)
	process, err := startLauncherEnvProcessWithParentExit(ctx, cmd, allowSurviveParentExit)
	if err != nil {
		return nil, false, fmt.Errorf("start active engine %s: %v; start stable launcher fallback %s: %w", enginePath, startErr, launcherPath, err)
	}
	return process, false, nil
}

func newLauncherEnvCommand(launcherPath, executablePath string, args []string) *exec.Cmd {
	cmd := exec.Command(executablePath, args...)
	cmd.Env = launcherEnv(launcherPath)
	return cmd
}

func startLauncherEnvProcess(ctx context.Context, cmd *exec.Cmd) (*procgroup.Process, error) {
	return startLauncherEnvProcessWithParentExit(ctx, cmd, false)
}

func startLauncherEnvProcessWithParentExit(ctx context.Context, cmd *exec.Cmd, allowSurviveParentExit bool) (*procgroup.Process, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	admission, err := prepareLauncherAttestation(cmd)
	if err != nil {
		launcherTracef("attestation unavailable")
		admission = nil
	}
	if err := context.Cause(ctx); err != nil {
		if admission != nil {
			_ = admission.Close()
		}
		return nil, err
	}
	args := []string(nil)
	if len(cmd.Args) > 1 {
		args = append(args, cmd.Args[1:]...)
	}
	process, err := procgroup.Spawn(procgroup.Options{
		Command:        cmd.Path,
		Args:           args,
		Dir:            cmd.Dir,
		Env:            append([]string(nil), cmd.Env...),
		Stdin:          cmd.Stdin,
		Stdout:         cmd.Stdout,
		Stderr:         cmd.Stderr,
		StartSuspended: true,
	})
	if err != nil {
		var closeErr error
		if admission != nil {
			closeErr = admission.Close()
		}
		if errors.Is(err, procgroup.ErrTreeRetirementUnproven) || closeErr != nil {
			return nil, errors.Join(supervisor.ErrStartRollbackUnproven, err, closeErr)
		}
		return nil, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, rollbackLauncherProcess(process, admission, err)
	}
	if admission != nil {
		if err := launcherAttestationBind(admission, process.PID()); err != nil {
			return nil, rollbackLauncherProcess(process, admission, fmt.Errorf("bind launcher attestation child: %w", err))
		}
	}
	if err := context.Cause(ctx); err != nil {
		return nil, rollbackLauncherProcess(process, admission, err)
	}
	if allowSurviveParentExit {
		if err := process.AllowSurviveParentExit(); err != nil {
			return nil, rollbackLauncherProcess(process, admission, fmt.Errorf("allow launcher child to survive parent exit: %w", err))
		}
	}
	if err := context.Cause(ctx); err != nil {
		return nil, rollbackLauncherProcess(process, admission, err)
	}
	return process, nil
}

func rollbackLauncherProcess(process *procgroup.Process, admission *attest.Parent, cause error) error {
	var proofErr error
	if process != nil {
		if err := process.Kill(); err != nil {
			proofErr = errors.Join(proofErr, err)
		}
		if err := process.Wait(); errors.Is(err, procgroup.ErrTreeRetirementUnproven) {
			proofErr = errors.Join(proofErr, err)
		}
	}
	var closeErr error
	if admission != nil {
		closeErr = admission.Close()
	}
	if proofErr != nil || closeErr != nil {
		return errors.Join(supervisor.ErrStartRollbackUnproven, cause, proofErr, closeErr)
	}
	return cause
}

func prepareLauncherAttestation(cmd *exec.Cmd) (*attest.Parent, error) {
	if !hasEnvKey(cmd.Env, envLauncherExe) {
		return nil, nil
	}
	admission, err := launcherAttestationStart()
	if err != nil {
		return nil, err
	}
	advertisement := admission.Advertisement()
	cmd.Env = setEnv(cmd.Env, envLauncherProtocol, advertisement.Version+":"+strconv.Itoa(advertisement.ParentPID))
	cmd.Env = setEnv(cmd.Env, envLauncherAttestation, advertisement.Endpoint)
	return admission, nil
}

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func launcherEnv(launcherPath string) []string {
	env := append([]string{}, os.Environ()...)
	env = setEnv(env, envEngineMode, "1")
	env = setEnv(env, envLauncherExe, launcherPath)
	env = setEnv(env, envActiveEngineFile, activeEngineFile(launcherPath))
	env = setEnv(env, envLauncherProtocol, "")
	env = setEnv(env, envLauncherAttestation, "")
	env = setEnv(env, envLauncherOwnsDaemon, "")
	return env
}

func runLauncherUpgrade(launcherPath string, args []string) int {
	upgradeFlags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	upgradeFlags.SetOutput(os.Stderr)
	restart := upgradeFlags.Bool("restart", false, "Safely restart daemon after active engine switch only when no live sessions are attached")
	forceDaemonRestart := upgradeFlags.Bool("force-daemon-restart", false, "Maintenance: restart daemon even with live sessions; existing old transports may close")
	updateLauncher := upgradeFlags.Bool("update-launcher", false, "Maintenance: replace the stable launcher binary from the staged update")
	restartActive := upgradeFlags.String("restart-active", "", "internal: restart daemon after active engine switch")
	if err := upgradeFlags.Parse(args); err != nil {
		return 2
	}
	if *restartActive != "" {
		enginePath, err := filepath.Abs(filepath.Clean(*restartActive))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: daemon restart incomplete: resolve active engine path: %v\n", err)
			fmt.Fprintln(os.Stderr, "Shims will start the active engine on next reconnect.")
			return 1
		}
		if err := restartDaemonAfterEngineSwitch(launcherPath, enginePath, *forceDaemonRestart); err != nil {
			fmt.Fprintf(os.Stderr, "warning: daemon restart incomplete: %v\n", err)
			fmt.Fprintln(os.Stderr, "Shims will start the active engine on next reconnect.")
			return 1
		}
		return 0
	}

	pendingPath := launcherPath + "~"
	enginePath, installed, launcherUpdated, err := installVersionedEngineWithOptions(launcherPath, pendingPath, versionedEngineInstallOptions{
		UpdateLauncher: *updateLauncher,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Build the pending engine binary first:")
		fmt.Fprintf(os.Stderr, "  go build -trimpath -o %s .\\cmd\\mcp-mux\n", pendingPath)
		return 1
	}

	if installed {
		fmt.Fprintf(os.Stderr, "Installed engine: %s\n", enginePath)
	} else {
		fmt.Fprintf(os.Stderr, "Engine already installed: %s\n", enginePath)
	}
	if launcherUpdated {
		fmt.Fprintf(os.Stderr, "Updated launcher: %s\n", launcherPath)
	} else if *updateLauncher {
		fmt.Fprintf(os.Stderr, "Launcher already current: %s\n", launcherPath)
	} else {
		fmt.Fprintf(os.Stderr, "Launcher preserved: %s\n", launcherPath)
	}
	fmt.Fprintf(os.Stderr, "Active engine: %s\n", enginePath)

	if *restart {
		if launcherUpdated {
			return launcherRunRestartActive(launcherPath, enginePath, *forceDaemonRestart)
		}
		if err := restartDaemonAfterEngineSwitch(launcherPath, enginePath, *forceDaemonRestart); err != nil {
			fmt.Fprintf(os.Stderr, "warning: daemon restart incomplete: %v\n", err)
			fmt.Fprintln(os.Stderr, "Shims will start the active engine on next reconnect.")
			return 1
		}
	}
	return 0
}

func runLauncherRestartActive(launcherPath, enginePath string, forceDaemonRestart bool) int {
	fmt.Fprintln(os.Stderr, "Re-executing updated launcher for daemon restart...")
	args := []string{"upgrade", "--restart-active", enginePath}
	if forceDaemonRestart {
		args = append(args, "--force-daemon-restart")
	}
	cmd := exec.Command(launcherPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "warning: updated launcher restart failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Shims will start the active engine on next reconnect.")
		return 1
	}
	return 0
}

type versionedEngineInstallOptions struct {
	UpdateLauncher bool
}

func installVersionedEngine(launcherPath, pendingPath string) (enginePath string, installed bool, launcherUpdated bool, err error) {
	return installVersionedEngineWithOptions(launcherPath, pendingPath, versionedEngineInstallOptions{})
}

func installVersionedEngineWithOptions(launcherPath, pendingPath string, opts versionedEngineInstallOptions) (enginePath string, installed bool, launcherUpdated bool, err error) {
	if _, err := os.Stat(pendingPath); err != nil {
		if os.IsNotExist(err) {
			return "", false, false, fmt.Errorf("no pending update found at %s", pendingPath)
		}
		return "", false, false, fmt.Errorf("stat pending update: %w", err)
	}

	hash, err := fileHash(pendingPath)
	if err != nil {
		return "", false, false, fmt.Errorf("hash pending update: %w", err)
	}
	versionID := hash
	if len(versionID) > 12 {
		versionID = versionID[:12]
	}
	enginePath = filepath.Join(versionStoreDir(launcherPath), versionID, engineFileName())

	if err := os.MkdirAll(filepath.Dir(enginePath), 0o755); err != nil {
		return "", false, false, fmt.Errorf("create version dir: %w", err)
	}

	if _, statErr := os.Stat(enginePath); statErr == nil {
		if !sameFile(enginePath, pendingPath) {
			return "", false, false, fmt.Errorf("version path exists with different content: %s", enginePath)
		}
		installed = false
	} else if os.IsNotExist(statErr) {
		if err := copyFile(pendingPath, enginePath, 0o755); err != nil {
			return "", false, false, fmt.Errorf("copy pending update into version store: %w", err)
		}
		installed = true
	} else {
		return "", false, false, fmt.Errorf("stat version path: %w", statErr)
	}

	if !opts.UpdateLauncher {
		_ = removeWithRetry(pendingPath, 10, 50*time.Millisecond)
	} else if sameFile(launcherPath, pendingPath) {
		_ = removeWithRetry(pendingPath, 10, 50*time.Millisecond)
	} else {
		oldPath, swapErr := upgrade.Swap(launcherPath, pendingPath)
		if swapErr != nil {
			return "", installed, false, fmt.Errorf("update launcher binary: %w", swapErr)
		}
		_ = os.Remove(oldPath)
		upgrade.CleanStale(launcherPath)
		launcherUpdated = true
	}

	if err := writeActiveEngine(launcherPath, enginePath); err != nil {
		return "", installed, launcherUpdated, err
	}
	return enginePath, installed, launcherUpdated, nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

func removeWithRetry(path string, attempts int, delay time.Duration) error {
	var last error
	for i := 0; i < attempts; i++ {
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			return nil
		} else {
			last = err
		}
		time.Sleep(delay)
	}
	return last
}

func restartDaemonAfterEngineSwitch(launcherPath, enginePath string, force bool) error {
	ctlPath := serverid.DaemonControlPath("", engineName)
	if !launcherIsDaemonRunning(ctlPath) {
		fmt.Fprintln(os.Stderr, "No daemon running; active engine will be used by the next shim reconnect.")
		return nil
	}

	if !force {
		liveSessions, err := launcherDaemonLiveSessionCount(ctlPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Daemon restart deferred: could not prove zero live sessions: %v\n", err)
			fmt.Fprintln(os.Stderr, "Active engine updated; existing host transports are preserved.")
			fmt.Fprintln(os.Stderr, "Run with --force-daemon-restart only during an explicit maintenance window.")
			return nil
		}
		if liveSessions > 0 {
			fmt.Fprintf(os.Stderr, "Daemon restart deferred: %d live session(s) attached; preserving host transports.\n", liveSessions)
			fmt.Fprintln(os.Stderr, "Active engine updated; the daemon can be restarted after sessions drain.")
			fmt.Fprintln(os.Stderr, "Run with --force-daemon-restart only during an explicit maintenance window.")
			return nil
		}
	}

	lockPath := serverid.DaemonLockPath("", engineName)
	lock, lockErr := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if lockErr == nil {
		if flockErr := lockFile(lock); flockErr == nil {
			defer func() {
				if err := unlockFile(lock); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not release daemon lock %s: %v\n", lockPath, err)
				}
				if err := lock.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not close daemon lock %s: %v\n", lockPath, err)
				}
			}()
			fmt.Fprintln(os.Stderr, "Acquired daemon lock (shims blocked from respawning).")
		} else {
			if err := lock.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not close daemon lock %s: %v\n", lockPath, err)
			}
			fmt.Fprintf(os.Stderr, "Warning: could not acquire daemon lock: %v (proceeding anyway)\n", flockErr)
		}
	} else {
		fmt.Fprintf(os.Stderr, "Warning: could not open daemon lock %s: %v (proceeding anyway)\n", lockPath, lockErr)
	}

	fmt.Fprintln(os.Stderr, "Graceful restart: serializing state...")
	resp, err := launcherControlSendWithTimeout(ctlPath, control.Request{
		Cmd:            "graceful-restart",
		DrainTimeoutMs: 30000,
		SuccessorExe:   enginePath,
	}, 60*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  graceful-restart not available: %v, falling back to shutdown\n", err)
		_, _ = control.Send(ctlPath, control.Request{Cmd: "shutdown"})
		launcherWaitForDaemonExit(ctlPath, "  Waiting for old daemon to exit...")
		return startDaemonAndWait(launcherPath, enginePath, ctlPath)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "  graceful-restart failed: %s, falling back to shutdown\n", resp.Message)
		_, _ = control.Send(ctlPath, control.Request{Cmd: "shutdown"})
		launcherWaitForDaemonExit(ctlPath, "  Waiting for old daemon to exit...")
		return startDaemonAndWait(launcherPath, enginePath, ctlPath)
	}

	launcherWaitForDaemonExit(ctlPath, "  snapshot written. Waiting for daemon to exit...")
	if err := launcherWaitForDaemon(ctlPath, 10*time.Second); err == nil {
		fmt.Fprintln(os.Stderr, "  New daemon ready. Releasing lock — shims will reconnect.")
		return nil
	}
	fmt.Fprintln(os.Stderr, "  Successor daemon not ready; starting active engine daemon...")
	if err := startDaemonAndWait(launcherPath, enginePath, ctlPath); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "  New daemon ready. Releasing lock — shims will reconnect.")
	return nil
}

func launcherDaemonLiveSessionCount(ctlPath string) (int, error) {
	resp, err := launcherControlSendWithTimeout(ctlPath, control.Request{Cmd: "status"}, 5*time.Second)
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, errors.New("nil status response")
	}
	if !resp.OK {
		return 0, fmt.Errorf("status: %s", resp.Message)
	}

	var payload struct {
		Servers []struct {
			SessionCount int `json:"session_count"`
		} `json:"servers"`
	}
	if len(resp.Data) == 0 {
		return 0, nil
	}
	if err := json.Unmarshal(resp.Data, &payload); err != nil {
		return 0, fmt.Errorf("decode daemon status: %w", err)
	}
	total := 0
	for _, srv := range payload.Servers {
		total += srv.SessionCount
	}
	return total, nil
}

func startDaemonAndWait(launcherPath, enginePath, ctlPath string) error {
	if err := launcherStartDaemonProcessFrom(launcherPath, enginePath); err != nil {
		return err
	}
	return launcherWaitForDaemon(ctlPath, 10*time.Second)
}

func startDaemonProcessFrom(launcherPath, enginePath string) error {
	return startDaemonProcessFromContext(context.Background(), launcherPath, enginePath)
}

func startDaemonProcessFromContext(ctx context.Context, launcherPath, enginePath string) error {
	_, _, err := startEngineOrStableLauncherContext(ctx, launcherPath, enginePath, []string{"daemon"}, false, true, func(cmd *exec.Cmd) {
		cmd.Stdout = nil
		cmd.Stderr = nil
		cmd.Stdin = nil
	})
	return err
}

var (
	launcherIsDaemonRunning        = isDaemonRunning
	launcherWaitForDaemon          = waitForDaemon
	launcherWaitForDaemonExit      = waitForDaemonExit
	launcherStartDaemonProcessFrom = startDaemonProcessFrom
	launcherControlSendWithTimeout = control.SendWithTimeout
	launcherRunRestartActive       = runLauncherRestartActive
)

func resolveActiveEngine(launcherPath string) (string, bool) {
	candidate, ok := resolveActiveEnginePointer(activeEngineFile(launcherPath))
	if !ok {
		return "", false
	}
	return resolveInstalledEnginePath(launcherPath, candidate)
}

func resolveInstalledEnginePath(launcherPath, candidate string) (string, bool) {
	storePath, err := filepath.Abs(versionStoreDir(launcherPath))
	if err != nil {
		return "", false
	}
	candidatePath, err := filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(storePath, candidatePath)
	if err != nil || !isVersionStoreEnginePath(storePath, candidatePath) {
		return "", false
	}

	storeInfo, err := os.Lstat(storePath)
	if err != nil || storeInfo.Mode()&os.ModeSymlink != 0 || !storeInfo.IsDir() {
		return "", false
	}
	versionPath := filepath.Join(storePath, filepath.Dir(relative))
	versionInfo, err := os.Lstat(versionPath)
	if err != nil || versionInfo.Mode()&os.ModeSymlink != 0 || !versionInfo.IsDir() {
		return "", false
	}
	engineInfo, err := os.Lstat(candidatePath)
	if err != nil || engineInfo.Mode()&os.ModeSymlink != 0 || !engineInfo.Mode().IsRegular() {
		return "", false
	}

	resolvedStore, err := filepath.EvalSymlinks(storePath)
	if err != nil {
		return "", false
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidatePath)
	if err != nil {
		return "", false
	}
	expectedCandidate := filepath.Join(resolvedStore, relative)
	if !samePath(resolvedCandidate, expectedCandidate) || !isVersionStoreEnginePath(resolvedStore, resolvedCandidate) {
		return "", false
	}
	return candidatePath, true
}

func authorizeInstalledEnginePath(launcherPath, candidate string) bool {
	resolved, ok := resolveInstalledEnginePath(launcherPath, candidate)
	if !ok || !samePath(resolved, candidate) {
		return false
	}
	versionID := filepath.Base(filepath.Dir(resolved))
	if len(versionID) != 12 {
		return false
	}
	for _, char := range versionID {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	hash, err := fileHash(resolved)
	return err == nil && strings.HasPrefix(hash, versionID)
}

func isVersionStoreEnginePath(storePath, candidatePath string) bool {
	relative, err := filepath.Rel(storePath, candidatePath)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	versionDir := filepath.Dir(relative)
	return filepath.Base(relative) == engineFileName() && versionDir != "." && filepath.Dir(versionDir) == "."
}

func resolveActiveEnginePointer(pointerPath string) (string, bool) {
	data, err := os.ReadFile(pointerPath)
	if err != nil {
		return "", false
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return "", false
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), true
	}
	return filepath.Clean(filepath.Join(filepath.Dir(pointerPath), raw)), true
}

func writeActiveEngine(launcherPath, enginePath string) error {
	activeFile := activeEngineFile(launcherPath)
	if err := os.MkdirAll(filepath.Dir(activeFile), 0o755); err != nil {
		return fmt.Errorf("create version store: %w", err)
	}
	rel, err := filepath.Rel(versionStoreDir(launcherPath), enginePath)
	if err != nil {
		rel = enginePath
	}
	tmp := fmt.Sprintf("%s.tmp.%d", activeFile, os.Getpid())
	if err := os.WriteFile(tmp, []byte(rel+"\n"), 0o644); err != nil {
		return fmt.Errorf("write active engine pointer: %w", err)
	}
	if err := replaceFile(tmp, activeFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace active engine pointer: %w", err)
	}
	return nil
}

func activeEngineFile(launcherPath string) string {
	return filepath.Join(versionStoreDir(launcherPath), "active.txt")
}

func versionStoreDir(launcherPath string) string {
	return filepath.Join(filepath.Dir(launcherPath), "mcp-mux.versions")
}

func engineFileName() string {
	if runtime.GOOS == "windows" {
		return "mcp-mux-engine.exe"
	}
	return "mcp-mux-engine"
}

func launcherFileName() string {
	engineName := engineFileName()
	ext := filepath.Ext(engineName)
	return strings.TrimSuffix(strings.TrimSuffix(engineName, ext), "-engine") + ext
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil {
		a = absA
	}
	if errB == nil {
		b = absB
	}
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
