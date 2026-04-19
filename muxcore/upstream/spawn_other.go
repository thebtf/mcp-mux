//go:build !windows

package upstream

// afterSpawnWindows is a no-op on non-Windows platforms.
// On Windows, this opens a process handle, creates a per-upstream Job Object
// and assigns the process to it (see spawn_windows.go).
func afterSpawnWindows(p *Process, pid int) {}

// releaseJobHandle is a no-op on non-Windows platforms.
// On Windows, this closes the per-upstream Job Object handle (see spawn_windows.go).
func releaseJobHandle(p *Process) {}
