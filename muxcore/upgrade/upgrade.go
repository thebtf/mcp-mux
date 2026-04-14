// Package upgrade provides utilities for atomic binary replacement.
// It is designed for zero-downtime self-upgrade of running executables.
//
// On Windows a running executable can be renamed but not overwritten.
// Swap exploits this by: rename current → .old.{pid}, rename new → current.
// The old binary stays on disk until the OS releases all file handles (process exit),
// after which CleanStale removes it.
package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Swap atomically replaces currentExe with newExe.
//
// The current binary is renamed to currentExe.old.{pid} (not deleted — may be
// locked on Windows while the process is running). The caller is responsible for
// calling CleanStale once the old binary is no longer needed.
//
// Returns oldPath (the renamed current binary) so the caller can attempt
// deferred cleanup. Returns an error if newExe does not exist or any rename fails.
// On failure the function attempts to restore currentExe from oldPath.
func Swap(currentExe, newExe string) (oldPath string, err error) {
	if _, err := os.Stat(newExe); err != nil {
		return "", fmt.Errorf("new binary not found: %w", err)
	}

	pid := os.Getpid()
	oldPath = fmt.Sprintf("%s.old.%d", currentExe, pid)

	// Rename current → old (Windows allows renaming a running exe)
	if err := os.Rename(currentExe, oldPath); err != nil {
		return "", fmt.Errorf("rename current to old: %w", err)
	}

	// Rename new → current
	if err := os.Rename(newExe, currentExe); err != nil {
		// Best-effort rollback — restore the original binary
		_ = os.Rename(oldPath, currentExe)
		return "", fmt.Errorf("rename new to current: %w", err)
	}

	return oldPath, nil
}

// CleanStale removes stale old-binary artefacts left by previous upgrades.
// It matches files in the same directory as exePath whose names follow the
// patterns: base.old.*, base.bak, base~~, base~
//
// The current binary (base name matches exePath) is never removed.
// Returns the number of files successfully removed.
func CleanStale(exePath string) int {
	dir := filepath.Dir(exePath)
	base := filepath.Base(exePath)
	cleaned := 0

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	for _, e := range entries {
		name := e.Name()
		if name == base {
			continue // never remove the live binary
		}
		if strings.HasPrefix(name, base+".old.") ||
			name == base+".bak" ||
			name == base+"~~" ||
			name == base+"~" {
			if err := os.Remove(filepath.Join(dir, name)); err == nil {
				cleaned++
			}
		}
	}

	return cleaned
}
