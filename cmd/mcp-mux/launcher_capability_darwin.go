//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func directParentExecutable() (string, error) {
	pid := os.Getppid()
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("inspect direct parent %d: %w", pid, err)
	}
	if int(proc.Proc.P_pid) != pid {
		return "", fmt.Errorf("inspect direct parent %d: pid mismatch", pid)
	}
	// kern.procargs2 begins with argc followed by the NUL-terminated executable
	// path. Unlike ps output this preserves executable paths containing spaces.
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", fmt.Errorf("read direct parent executable: %w", err)
	}
	if len(raw) <= 4 {
		return "", fmt.Errorf("direct parent %d has no executable path", pid)
	}
	if end := bytes.IndexByte(raw[4:], 0); end > 0 {
		return string(raw[4 : 4+end]), nil
	}
	return "", fmt.Errorf("direct parent %d has malformed executable path", pid)
}
