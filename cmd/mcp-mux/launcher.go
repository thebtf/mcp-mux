package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const (
	envEngineMode       = "MCPMUX_ENGINE"
	envDisableLauncher  = "MCPMUX_DISABLE_LAUNCHER"
	envLauncherExe      = "MCPMUX_LAUNCHER_EXE"
	envActiveEngineFile = "MCPMUX_ACTIVE_ENGINE_FILE"
)

func maybeRunLauncher() (bool, int) {
	if os.Getenv(envEngineMode) == "1" || os.Getenv(envDisableLauncher) == "1" {
		return false, 0
	}

	exe, err := os.Executable()
	if err != nil {
		return false, 0
	}

	if len(os.Args) > 1 && os.Args[1] == "upgrade" {
		return true, runLauncherUpgrade(exe, os.Args[2:])
	}

	enginePath, ok := resolveActiveEngine(exe)
	if !ok || samePath(enginePath, exe) {
		return false, 0
	}

	return true, runEngineProcess(exe, enginePath, os.Args[1:])
}

func runEngineProcess(launcherPath, enginePath string, args []string) int {
	cmd := exec.Command(enginePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = launcherEnv(launcherPath)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "mcp-mux launcher: start engine %s: %v\n", enginePath, err)
		return 1
	}
	return 0
}

func launcherEnv(launcherPath string) []string {
	env := append([]string{}, os.Environ()...)
	env = setEnv(env, envEngineMode, "1")
	env = setEnv(env, envLauncherExe, launcherPath)
	env = setEnv(env, envActiveEngineFile, activeEngineFile(launcherPath))
	return env
}

func runLauncherUpgrade(launcherPath string, args []string) int {
	upgradeFlags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	upgradeFlags.SetOutput(os.Stderr)
	restart := upgradeFlags.Bool("restart", false, "Restart daemon after active engine switch")
	if err := upgradeFlags.Parse(args); err != nil {
		return 2
	}

	pendingPath := launcherPath + "~"
	enginePath, installed, err := installVersionedEngine(launcherPath, pendingPath)
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
	fmt.Fprintf(os.Stderr, "Active engine: %s\n", enginePath)

	if *restart {
		if err := restartDaemonAfterEngineSwitch(launcherPath, enginePath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: daemon restart incomplete: %v\n", err)
			fmt.Fprintln(os.Stderr, "Shims will start the active engine on next reconnect.")
			return 1
		}
	}
	return 0
}

func installVersionedEngine(launcherPath, pendingPath string) (enginePath string, installed bool, err error) {
	if _, err := os.Stat(pendingPath); err != nil {
		if os.IsNotExist(err) {
			return "", false, fmt.Errorf("no pending update found at %s", pendingPath)
		}
		return "", false, fmt.Errorf("stat pending update: %w", err)
	}

	hash, err := fileHash(pendingPath)
	if err != nil {
		return "", false, fmt.Errorf("hash pending update: %w", err)
	}
	versionID := hash
	if len(versionID) > 12 {
		versionID = versionID[:12]
	}
	enginePath = filepath.Join(versionStoreDir(launcherPath), versionID, engineFileName())

	if current, ok := resolveActiveEngine(launcherPath); ok && samePath(current, enginePath) {
		if sameFile(current, pendingPath) {
			_ = removeWithRetry(pendingPath, 10, 50*time.Millisecond)
			return enginePath, false, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(enginePath), 0755); err != nil {
		return "", false, fmt.Errorf("create version dir: %w", err)
	}

	if _, statErr := os.Stat(enginePath); statErr == nil {
		if !sameFile(enginePath, pendingPath) {
			return "", false, fmt.Errorf("version path exists with different content: %s", enginePath)
		}
		_ = removeWithRetry(pendingPath, 10, 50*time.Millisecond)
		installed = false
	} else if os.IsNotExist(statErr) {
		if err := movePendingEngine(pendingPath, enginePath); err != nil {
			return "", false, fmt.Errorf("move pending update into version store: %w", err)
		}
		installed = true
	} else {
		return "", false, fmt.Errorf("stat version path: %w", statErr)
	}

	if err := writeActiveEngine(launcherPath, enginePath); err != nil {
		return "", installed, err
	}
	return enginePath, installed, nil
}

func movePendingEngine(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst, 0755); err != nil {
		return err
	}
	if !sameFile(src, dst) {
		_ = os.Remove(dst)
		return fmt.Errorf("copied engine hash mismatch")
	}
	_ = removeWithRetry(src, 10, 50*time.Millisecond)
	return nil
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

func restartDaemonAfterEngineSwitch(launcherPath, enginePath string) error {
	ctlPath := serverid.DaemonControlPath("", engineName)
	if !isDaemonRunning(ctlPath) {
		if err := startDaemonProcessFrom(launcherPath, enginePath); err != nil {
			return err
		}
		return waitForDaemon(ctlPath, 10*time.Second)
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
	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:            "graceful-restart",
		DrainTimeoutMs: 30000,
	}, 60*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  graceful-restart not available: %v, falling back to shutdown\n", err)
		_, _ = control.Send(ctlPath, control.Request{Cmd: "shutdown"})
		waitForDaemonExit(ctlPath, "  Waiting for old daemon to exit...")
		return startDaemonAndWait(launcherPath, enginePath, ctlPath)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "  graceful-restart failed: %s, falling back to shutdown\n", resp.Message)
		_, _ = control.Send(ctlPath, control.Request{Cmd: "shutdown"})
		waitForDaemonExit(ctlPath, "  Waiting for old daemon to exit...")
		return startDaemonAndWait(launcherPath, enginePath, ctlPath)
	}

	waitForDaemonExit(ctlPath, "  snapshot written. Waiting for daemon to exit...")
	if err := waitForDaemon(ctlPath, 10*time.Second); err == nil {
		fmt.Fprintln(os.Stderr, "  New daemon ready. Releasing lock — shims will reconnect.")
		return nil
	}
	fmt.Fprintln(os.Stderr, "  Successor daemon not ready; starting active engine daemon...")
	return startDaemonAndWait(launcherPath, enginePath, ctlPath)
}

func startDaemonAndWait(launcherPath, enginePath, ctlPath string) error {
	if err := startDaemonProcessFrom(launcherPath, enginePath); err != nil {
		return err
	}
	return waitForDaemon(ctlPath, 10*time.Second)
}

func startDaemonProcessFrom(launcherPath, enginePath string) error {
	cmd := exec.Command(enginePath, "daemon")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.Env = launcherEnv(launcherPath)
	setSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon process: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process: %w", err)
	}
	return nil
}

func resolveActiveEngine(launcherPath string) (string, bool) {
	data, err := os.ReadFile(activeEngineFile(launcherPath))
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
	return filepath.Clean(filepath.Join(versionStoreDir(launcherPath), raw)), true
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
