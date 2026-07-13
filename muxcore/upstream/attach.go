package upstream

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

func AttachFromFDs(pid int, stdinFD, stdoutFD uintptr, command string) (*Process, error) {
	return attachFromFDs(pid, stdinFD, stdoutFD, 0, 0, command, false, nil)
}

// AttachFromFDsWithAuthority adopts transferred stdio plus the platform tree
// authority. Windows requires a Job handle containing pid; Unix requires zero
// because PID/PGID is the authority.
func AttachFromFDsWithAuthority(pid int, stdinFD, stdoutFD, stderrFD, authorityFD uintptr, command string, logger *log.Logger) (*Process, error) {
	return attachFromFDs(pid, stdinFD, stdoutFD, stderrFD, authorityFD, command, true, logger)
}

func closeTransferredFD(fd uintptr, name string) {
	if fd == 0 {
		return
	}
	if file := os.NewFile(fd, name); file != nil {
		_ = file.Close()
	}
}

func attachFromFDs(pid int, stdinFD, stdoutFD, stderrFD, authorityFD uintptr, command string, requireAuthority bool, logger *log.Logger) (_ *Process, err error) {
	var stdinFile, stdoutFile, stderrFile *os.File
	authorityAdopted := false
	defer func() {
		if err == nil {
			return
		}
		if stdinFile != nil {
			_ = stdinFile.Close()
		} else {
			closeTransferredFD(stdinFD, "rejected-upstream-stdin")
		}
		if stdoutFile != nil {
			_ = stdoutFile.Close()
		} else {
			closeTransferredFD(stdoutFD, "rejected-upstream-stdout")
		}
		if stderrFile != nil {
			_ = stderrFile.Close()
		} else {
			closeTransferredFD(stderrFD, "rejected-upstream-stderr")
		}
		if !authorityAdopted {
			closeHandoffAuthority(authorityFD)
		}
	}()

	if pid <= 0 {
		return nil, errors.New("upstream: AttachFromFDs: pid must be > 0")
	}
	if stdinFD == 0 {
		return nil, errors.New("upstream: AttachFromFDs: stdinFD must be non-zero")
	}
	if stdoutFD == 0 {
		return nil, errors.New("upstream: AttachFromFDs: stdoutFD must be non-zero")
	}
	if requireAuthority && stderrFD == 0 {
		return nil, errors.New("upstream: AttachFromFDs: stderrFD must be non-zero for transactional handoff")
	}

	stdinFile = os.NewFile(stdinFD, fmt.Sprintf("upstream-stdin-%d", pid))
	stdoutFile = os.NewFile(stdoutFD, fmt.Sprintf("upstream-stdout-%d", pid))
	if stderrFD != 0 {
		stderrFile = os.NewFile(stderrFD, fmt.Sprintf("upstream-stderr-%d", pid))
	}
	if stdinFile == nil || stdoutFile == nil || (requireAuthority && stderrFile == nil) {
		return nil, fmt.Errorf("upstream: AttachFromFDs: os.NewFile rejected handle (stdinFD=%d stdoutFD=%d stderrFD=%d)", stdinFD, stdoutFD, stderrFD)
	}

	p := &Process{
		pid:        pid,
		stdin:      stdinFile,
		stdout:     stdoutFile,
		stderr:     stderrFile,
		stdinFile:  stdinFile,
		stdoutFile: stdoutFile,
		stderrFile: stderrFile,
		stdinFD:    stdinFD,
		stdoutFD:   stdoutFD,
		stderrFD:   stderrFD,
		lineBuf:    newLineBuffer(),
		Done:       make(chan struct{}),
	}
	if err := attachHandoffAuthority(p, pid, authorityFD, requireAuthority); err != nil {
		return nil, err
	}
	leaderDone, err := watchAttachedLeader(p, pid)
	if err != nil {
		return nil, err
	}
	authorityAdopted = true

	stdoutDrained := p.startStdoutDrain(stdoutFile)
	stderrDrained := p.startStderrDrain(stderrFile, logger, pid)

	go func() {
		exitErr := io.EOF
		if err := <-leaderDone; err != nil {
			exitErr = err
		}
		attached := p.beginExitFinalization()
		if attached {
			if p.stdin != nil {
				_ = p.stdin.Close()
			}
			_ = terminateProcessTree(p)
			_ = stdoutFile.Close()
			if stderrFile != nil {
				_ = stderrFile.Close()
			}
		}
		<-stdoutDrained
		<-stderrDrained
		p.ExitErr = exitErr
		close(p.Done)
	}()

	return p, nil
}
