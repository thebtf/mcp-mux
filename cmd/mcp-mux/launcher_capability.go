package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thebtf/mcp-mux/muxcore/upgrade"
)

var (
	launcherCurrentExecutable = os.Executable
	launcherParentExecutable  = directParentExecutable
	launcherActivePointer     = resolveActiveEnginePointer
)

// launcherLifecycleCapable proves that a current engine was directly launched
// by a current stable launcher. An inherited environment alone is not enough:
// old launchers must never receive private dormant frames on host stdout.
func launcherLifecycleCapable() bool {
	parts := strings.Split(strings.TrimSpace(os.Getenv(envLauncherProtocol)), ":")
	if len(parts) != 2 || parts[0] != "1" {
		return false
	}
	launcherPID, err := strconv.Atoi(parts[1])
	if err != nil || launcherPID <= 0 || launcherPID != os.Getppid() {
		return false
	}
	// The PID proves the capability belongs to this direct parent instance; the
	// executable identity proves that parent is the installed stable launcher,
	// not an arbitrary parent that copied the environment; the active pointer
	// proves this child is the engine that launcher selected.
	parentPath, err := launcherParentExecutable()
	return err == nil && samePath(parentPath, os.Getenv(envLauncherExe)) && launcherActiveEngineMatchesSelf()
}

// launcherBootstrapEligible is deliberately stricter than an env check. It
// permits one active engine child of an old launcher to update the stable
// launcher for future invocations, but it does not let a copied/inherited env
// replace an arbitrary executable.
func launcherBootstrapEligible() bool {
	launcherPath := strings.TrimSpace(os.Getenv(envLauncherExe))
	activeFile := strings.TrimSpace(os.Getenv(envActiveEngineFile))
	if launcherPath == "" || activeFile == "" {
		return false
	}
	if !launcherActiveEngineMatchesSelf() {
		return false
	}
	parentPath, err := launcherParentExecutable()
	return err == nil && samePath(parentPath, launcherPath)
}

func launcherActiveEngineMatchesSelf() bool {
	activeFile := strings.TrimSpace(os.Getenv(envActiveEngineFile))
	if activeFile == "" {
		return false
	}
	enginePath, err := launcherCurrentExecutable()
	if err != nil {
		return false
	}
	activePath, ok := launcherActivePointer(activeFile)
	return ok && samePath(activePath, enginePath)
}

// bootstrapStableLauncher copies the verified active engine beside the running
// stable launcher and uses upgrade.Swap's two-rename rollback contract. It
// never touches the current launcher process in place, and a concurrent child
// simply leaves the winner's result intact.
func bootstrapStableLauncher() (bool, error) {
	if !launcherBootstrapEligible() {
		return false, fmt.Errorf("launcher bootstrap identity proof failed")
	}
	launcherPath := filepath.Clean(os.Getenv(envLauncherExe))
	enginePath, err := launcherCurrentExecutable()
	if err != nil {
		return false, fmt.Errorf("resolve active engine: %w", err)
	}
	if sameFile(launcherPath, enginePath) {
		return false, nil
	}

	lock, err := os.OpenFile(launcherPath+".bootstrap.lock", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return false, fmt.Errorf("open launcher bootstrap lock: %w", err)
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		// Another verified child is already performing the same idempotent update.
		return false, nil
	}
	defer unlockFile(lock)
	if sameFile(launcherPath, enginePath) {
		return false, nil
	}

	staged := fmt.Sprintf("%s.bootstrap.%d", launcherPath, os.Getpid())
	if err := copyFile(enginePath, staged, 0755); err != nil {
		return false, fmt.Errorf("stage stable launcher: %w", err)
	}
	oldPath, err := upgrade.Swap(launcherPath, staged)
	if err != nil {
		_ = os.Remove(staged)
		return false, fmt.Errorf("swap stable launcher: %w", err)
	}
	// The old file can still be mapped by the direct parent on Windows. Do not
	// delete it here; a future non-live maintenance operation may clean it.
	_ = oldPath
	return true, nil
}
