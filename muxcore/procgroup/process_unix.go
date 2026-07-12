//go:build !windows

package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type platformState struct {
	mu        sync.Mutex
	pgid      int
	state     uint8
	finalized chan struct{}
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
	case authorityFinalizing:
		return 0, p.platform.finalized, false
	case authorityFinalized, authorityIdle:
		return 0, nil, false
	default:
		return 0, nil, false
	}
}

func (p *Process) finishGroupAuthority() {
	p.platform.mu.Lock()
	if p.platform.state == authorityFinalizing {
		p.platform.state = authorityFinalized
		close(p.platform.finalized)
	}
	p.platform.mu.Unlock()
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

func waitAuthority(wait <-chan struct{}) {
	if wait != nil {
		<-wait
	}
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
		waitAuthority(wait)
		return nil
	}
	termErr := signalProcessGroup(pgid, syscall.SIGTERM)
	select {
	case <-p.leaderDone:
	case <-time.After(timeout):
	}
	killErr := signalProcessGroup(pgid, syscall.SIGKILL)
	p.finishGroupAuthority()
	<-p.done
	if termErr != nil {
		return termErr
	}
	return killErr
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
		waitAuthority(wait)
		return nil
	}
	err := signalProcessGroup(pgid, syscall.SIGKILL)
	p.finishGroupAuthority()
	if p.Alive() {
		<-p.done
	}
	return err
}

func (p *Process) cleanupPlatform() {
	if p.disableTree {
		return
	}
	p.platform.mu.Lock()
	if p.platform.state == authorityIdle {
		p.platform.state = authorityFinalized
		close(p.platform.finalized)
	}
	p.platform.mu.Unlock()
}

func (p *Process) reapPlatform() {
	if p.disableTree {
		return
	}
	pgid, wait, owner := p.takeGroupAuthority()
	if owner {
		_ = signalProcessGroup(pgid, syscall.SIGKILL)
		p.finishGroupAuthority()
		return
	}
	waitAuthority(wait)
}
