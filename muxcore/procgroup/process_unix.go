//go:build !windows

package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type platformState struct {
	mu          sync.Mutex
	pgid        int
	state       uint8
	finalized   chan struct{}
	finalizeErr error
}

const (
	authorityIdle uint8 = iota
	authorityActive
	authorityFinalizing
	authorityFinalized
)

func configurePlatform(cmd *exec.Cmd, p *Process) error {
	if p.disableTree {
		return nil
	}
	if err := prepareProcessTreeAuthority(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.platform.finalized = make(chan struct{})
	return nil
}

func (p *Process) postStart() error {
	if !p.disableTree {
		p.platform.mu.Lock()
		p.platform.pgid = p.pid
		p.platform.state = authorityActive
		p.platform.mu.Unlock()
	}
	return nil
}

func (p *Process) takeGroupAuthority() (int, <-chan struct{}, bool) {
	p.platform.mu.Lock()
	defer p.platform.mu.Unlock()
	switch p.platform.state {
	case authorityActive:
		pgid := p.platform.pgid
		p.platform.pgid = 0
		p.platform.state = authorityFinalizing
		return pgid, p.platform.finalized, true
	case authorityFinalizing, authorityFinalized:
		return 0, p.platform.finalized, false
	case authorityIdle:
		return 0, nil, false
	default:
		return 0, nil, false
	}
}

func (p *Process) finishGroupAuthority(finalizeErr error) error {
	p.platform.mu.Lock()
	if p.platform.state == authorityFinalizing {
		p.platform.finalizeErr = finalizeErr
		p.platform.state = authorityFinalized
		close(p.platform.finalized)
	}
	result := p.platform.finalizeErr
	p.platform.mu.Unlock()
	return result
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	if pgid <= 0 {
		return nil
	}
	err := syscall.Kill(-pgid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func waitProcessGroupGone(pgid int, leaderDone <-chan struct{}) error {
	if pgid <= 0 {
		return nil
	}
	deadline := time.Now().Add(processTreeWaitTimeout)
	if leaderDone != nil {
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-leaderDone:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			return fmt.Errorf("process group %d leader remained unreaped after %s", pgid, processTreeWaitTimeout)
		}
	}
	for {
		if err := drainWaitableGroupChildren(pgid); err != nil {
			return fmt.Errorf("reap adopted process group %d children: %w", pgid, err)
		}
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("probe process group %d: %w", pgid, err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process group %d remained alive after %s", pgid, processTreeWaitTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processGroupRetirementError(waitErr, termErr, killErr error) error {
	if waitErr == nil {
		return nil
	}
	return errors.Join(termErr, killErr, waitErr)
}

func (p *Process) waitGroupAuthority(wait <-chan struct{}) error {
	if wait == nil {
		return nil
	}
	<-wait
	p.platform.mu.Lock()
	defer p.platform.mu.Unlock()
	return p.platform.finalizeErr
}

func (p *Process) gracefulKillPlatform(timeout time.Duration) error {
	if p.disableTree {
		if p.cmd.Process == nil {
			return nil
		}
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.leaderDone:
		case <-time.After(timeout):
			if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
		}
		<-p.done
		return nil
	}

	pgid, wait, owner := p.takeGroupAuthority()
	if !owner {
		authorityErr := p.waitGroupAuthority(wait)
		<-p.done
		return authorityErr
	}
	termErr := signalProcessGroup(pgid, syscall.SIGTERM)
	select {
	case <-p.leaderDone:
	case <-time.After(timeout):
	}
	killErr := signalProcessGroup(pgid, syscall.SIGKILL)
	waitErr := waitProcessGroupGone(pgid, p.leaderDone)
	authorityErr := p.finishGroupAuthority(processGroupRetirementError(waitErr, termErr, killErr))
	<-p.done
	return authorityErr
}

func (p *Process) killPlatform() error {
	if p.disableTree {
		if !p.Alive() || p.cmd.Process == nil {
			return nil
		}
		err := p.cmd.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		if err == nil {
			<-p.done
		}
		return err
	}

	pgid, wait, owner := p.takeGroupAuthority()
	if !owner {
		authorityErr := p.waitGroupAuthority(wait)
		<-p.done
		return authorityErr
	}
	killErr := signalProcessGroup(pgid, syscall.SIGKILL)
	waitErr := waitProcessGroupGone(pgid, p.leaderDone)
	authorityErr := p.finishGroupAuthority(processGroupRetirementError(waitErr, nil, killErr))
	if p.Alive() {
		<-p.done
	}
	return authorityErr
}

func (p *Process) cleanupPlatform() error {
	if p.disableTree {
		return nil
	}
	p.platform.mu.Lock()
	if p.platform.state == authorityIdle {
		p.platform.state = authorityFinalized
		close(p.platform.finalized)
		result := p.platform.finalizeErr
		p.platform.mu.Unlock()
		return result
	}
	p.platform.mu.Unlock()
	pgid, wait, owner := p.takeGroupAuthority()
	if owner {
		killErr := signalProcessGroup(pgid, syscall.SIGKILL)
		waitErr := waitProcessGroupGone(pgid, p.leaderDone)
		return p.finishGroupAuthority(processGroupRetirementError(waitErr, nil, killErr))
	}
	return p.waitGroupAuthority(wait)
}

func (p *Process) reapPlatform() error {
	if p.disableTree {
		return nil
	}
	pgid, wait, owner := p.takeGroupAuthority()
	if owner {
		killErr := signalProcessGroup(pgid, syscall.SIGKILL)
		waitErr := waitProcessGroupGone(pgid, p.leaderDone)
		return p.finishGroupAuthority(processGroupRetirementError(waitErr, nil, killErr))
	}
	return p.waitGroupAuthority(wait)
}
