//go:build !windows

package upstream

import (
	"errors"
	"fmt"
	"syscall"
)

func afterSpawnWindows(p *Process, pid int) error { return nil }

func (p *Process) takeProcessGroupAuthority() int {
	p.authorityMu.Lock()
	pgid := p.spawnPgid
	p.spawnPgid = 0
	p.authorityMu.Unlock()
	return pgid
}

func terminateProcessTree(p *Process) error {
	pgid := p.takeProcessGroupAuthority()
	if pgid <= 0 {
		if p.proc == nil {
			return nil
		}
		return p.proc.Kill()
	}
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func releaseTreeAuthority(p *Process) error {
	_ = p.takeProcessGroupAuthority()
	return nil
}

func handoffAuthority(p *Process) (uintptr, error) {
	p.authorityMu.Lock()
	defer p.authorityMu.Unlock()
	if p.spawnPgid <= 0 {
		return 0, errors.New("upstream: process-group authority unavailable")
	}
	return 0, nil
}

func prepareLegacyDetach(p *Process, pid int) error {
	p.authorityMu.Lock()
	defer p.authorityMu.Unlock()
	if p.spawnPgid != pid {
		return fmt.Errorf("upstream: process-group authority %d does not match pid %d", p.spawnPgid, pid)
	}
	return nil
}

func attachHandoffAuthority(p *Process, pid int, authority uintptr, requireAuthority bool) error {
	if authority != 0 {
		return fmt.Errorf("upstream: unexpected authority fd %d on Unix", authority)
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("upstream: validate process group %d: %w", pid, err)
	}
	p.authorityMu.Lock()
	p.spawnPgid = pid
	p.authorityMu.Unlock()
	return nil
}

func closeHandoffAuthority(authority uintptr) {
	if authority != 0 {
		_ = syscall.Close(int(authority))
	}
}
