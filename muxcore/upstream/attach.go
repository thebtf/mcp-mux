package upstream

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
)

func AttachFromFDs(pid int, stdinFD, stdoutFD uintptr, command string) (*Process, error) {
	return attachFromFDs(pid, stdinFD, stdoutFD, 0, command, false)
}

// AttachFromFDsWithAuthority adopts transferred stdio plus the platform tree
// authority. Windows requires a Job handle containing pid; Unix requires zero
// because PID/PGID is the authority.
func AttachFromFDsWithAuthority(pid int, stdinFD, stdoutFD, authorityFD uintptr, command string) (*Process, error) {
	return attachFromFDs(pid, stdinFD, stdoutFD, authorityFD, command, true)
}

func closeTransferredFD(fd uintptr, name string) {
	if fd == 0 {
		return
	}
	if file := os.NewFile(fd, name); file != nil {
		_ = file.Close()
	}
}

func attachFromFDs(pid int, stdinFD, stdoutFD, authorityFD uintptr, command string, requireAuthority bool) (_ *Process, err error) {
	var stdinFile, stdoutFile *os.File
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

	stdinFile = os.NewFile(stdinFD, fmt.Sprintf("upstream-stdin-%d", pid))
	stdoutFile = os.NewFile(stdoutFD, fmt.Sprintf("upstream-stdout-%d", pid))
	if stdinFile == nil || stdoutFile == nil {
		return nil, fmt.Errorf("upstream: AttachFromFDs: os.NewFile rejected handle (stdinFD=%d stdoutFD=%d)", stdinFD, stdoutFD)
	}

	p := &Process{
		pid:        pid,
		stdin:      stdinFile,
		stdout:     stdoutFile,
		stdinFile:  stdinFile,
		stdoutFile: stdoutFile,
		stdinFD:    stdinFD,
		stdoutFD:   stdoutFD,
		lineBuf:    newLineBuffer(),
		Done:       make(chan struct{}),
	}
	if err := attachHandoffAuthority(p, pid, authorityFD, requireAuthority); err != nil {
		return nil, err
	}
	authorityAdopted = true

	go func() {
		defer func() { _ = stdoutFile.Close() }()
		scanner := bufio.NewScanner(stdoutFile)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			cp := make([]byte, len(line))
			copy(cp, line)
			p.lineBuf.push(cp)
		}
		scanErr := scanner.Err()
		if errors.Is(scanErr, os.ErrClosed) {
			scanErr = nil
		}
		p.lineBuf.markDoneWithErr(scanErr)
		p.mu.Lock()
		attached := p.detach == detachAttached
		p.mu.Unlock()
		if attached {
			_ = terminateProcessTree(p)
		}
		p.ExitErr = io.EOF
		close(p.Done)
	}()

	return p, nil
}
