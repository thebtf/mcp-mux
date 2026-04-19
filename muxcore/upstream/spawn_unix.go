//go:build unix

package upstream

import "syscall"

// applyUnixSpawnAttrs sets platform-specific attributes on the exec.Cmd
// before it starts, to isolate the child process from the daemon's
// process group. With Setpgid=true, the kernel puts the child in a NEW
// process group whose PGID equals the child's PID. SIGHUP and similar
// group-wide signals sent to the daemon do not cascade to the child.
// This is a precondition for FR-1 (upstream survives daemon restart).
func applyUnixSpawnAttrs(sysAttr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if sysAttr == nil {
		sysAttr = &syscall.SysProcAttr{}
	}
	sysAttr.Setpgid = true
	return sysAttr
}
