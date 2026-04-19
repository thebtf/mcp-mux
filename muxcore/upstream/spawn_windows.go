//go:build windows

package upstream

import "syscall"

// applyUnixSpawnAttrs is a no-op on Windows. Setpgid does not exist;
// Windows Job Object detachment is handled separately in T015.
func applyUnixSpawnAttrs(sysAttr *syscall.SysProcAttr) *syscall.SysProcAttr {
	return sysAttr
}
