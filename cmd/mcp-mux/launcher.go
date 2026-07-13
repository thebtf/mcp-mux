package main

import (
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
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/upgrade"
)

const (
	envEngineMode       = "MCPMUX_ENGINE"
	envDisableLauncher  = "MCPMUX_DISABLE_LAUNCHER"
	envLauncherExe      = "MCPMUX_LAUNCHER_EXE"
	envActiveEngineFile = "MCPMUX_ACTIVE_ENGINE_FILE"
	envLauncherProtocol = "MCPMUX_LAUNCHER_PROTOCOL"
	envLauncherTrace    = "MCPMUX_LAUNCHER_TRACE"
)

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
			Stdin:             os.Stdin,
			Stdout:            os.Stdout,
			Stderr:            os.Stderr,
			DormantLease:      launcherDormantLease(os.Getenv),
		})
	}

	cmd, fallbackToCaller, err := startEngineOrStableLauncher(launcherPath, enginePath, args, true, func(cmd *exec.Cmd) {
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
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return true, exitErr.ExitCode()
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

func startEngineOrStableLauncher(launcherPath, enginePath string, args []string, fallbackToCaller bool, configure func(*exec.Cmd)) (*exec.Cmd, bool, error) {
	cmd := newLauncherEnvCommand(launcherPath, enginePath, args)
	configure(cmd)
	if err := cmd.Start(); err == nil {
		return cmd, false, nil
	} else if fallbackToCaller {
		fmt.Fprintf(os.Stderr, "mcp-mux launcher: start active engine %s: %v\n", enginePath, err)
		fmt.Fprintln(os.Stderr, "mcp-mux launcher: falling back to stable launcher binary")
		return nil, true, nil
	} else {
		fmt.Fprintf(os.Stderr, "mcp-mux launcher: start active engine %s: %v\n", enginePath, err)
		fmt.Fprintln(os.Stderr, "mcp-mux launcher: starting daemon via stable launcher binary")
		startErr := err
		cmd = newLauncherEnvCommand(launcherPath, launcherPath, args)
		configure(cmd)
		if err := cmd.Start(); err != nil {
			return nil, false, fmt.Errorf("start active engine %s: %v; start stable launcher fallback %s: %w", enginePath, startErr, launcherPath, err)
		}
		return cmd, false, nil
	}
}

func newLauncherEnvCommand(launcherPath, executablePath string, args []string) *exec.Cmd {
	cmd := exec.Command(executablePath, args...)
	cmd.Env = launcherEnv(launcherPath)
	return cmd
}

func launcherEnv(launcherPath string) []string {
	env := append([]string{}, os.Environ()...)
	env = setEnv(env, envEngineMode, "1")
	env = setEnv(env, envLauncherExe, launcherPath)
	env = setEnv(env, envActiveEngineFile, activeEngineFile(launcherPath))
	// This is only an advertisement. The engine additionally proves that its
	// direct parent is this launcher and that it is the active engine before it
	// sends launcher-private lifecycle frames.
	env = setEnv(env, envLauncherProtocol, "1:"+strconv.Itoa(os.Getpid()))
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

	if err := os.MkdirAll(filepath.Dir(enginePath), 0755); err != nil {
		return "", false, false, fmt.Errorf("create version dir: %w", err)
	}

	if _, statErr := os.Stat(enginePath); statErr == nil {
		if !sameFile(enginePath, pendingPath) {
			return "", false, false, fmt.Errorf("version path exists with different content: %s", enginePath)
		}
		installed = false
	} else if os.IsNotExist(statErr) {
		if err := copyFile(pendingPath, enginePath, 0755); err != nil {
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
	lock, lockErr := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
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
	cmd, _, err := startEngineOrStableLauncher(launcherPath, enginePath, []string{"daemon"}, false, func(cmd *exec.Cmd) {
		cmd.Stdout = nil
		cmd.Stderr = nil
		cmd.Stdin = nil
		setSysProcAttr(cmd)
	})
	if err != nil {
		return fmt.Errorf("start daemon process: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process: %w", err)
	}
	return nil
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
	return resolveActiveEnginePointer(activeEngineFile(launcherPath))
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
	if err := os.MkdirAll(filepath.Dir(activeFile), 0755); err != nil {
		return fmt.Errorf("create version store: %w", err)
	}
	rel, err := filepath.Rel(versionStoreDir(launcherPath), enginePath)
	if err != nil {
		rel = enginePath
	}
	tmp := fmt.Sprintf("%s.tmp.%d", activeFile, os.Getpid())
	if err := os.WriteFile(tmp, []byte(rel+"\n"), 0644); err != nil {
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
