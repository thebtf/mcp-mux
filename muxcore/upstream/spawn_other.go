//go:build !windows

package upstream

import "os/exec"

// applyUnixSpawnAttrs sets Unix process group attributes for signal isolation.
// On Windows this is a no-op (see spawn_windows.go).
// On Unix (T008 / Phase 2), this function will be replaced by a real
// implementation that configures Setpgid via cmd.SysProcAttr.
func applyUnixSpawnAttrs(cmd *exec.Cmd) {}

// afterSpawnWindows is a no-op on non-Windows platforms.
func afterSpawnWindows(p *Process, pid int) {}
