package upstream

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
)

// AttachFromFDs creates a Process that wraps already-running upstream process
// file descriptors. The caller is responsible for transferring stdinFD and
// stdoutFD to this process (e.g. via SCM_RIGHTS on Unix or DuplicateHandle on
// Windows) before calling AttachFromFDs.
//
// pid is stored as spawnPgid for observability; on Unix it equals the upstream's
// process group ID (post-Setpgid guarantee). On Windows spawnPgid is not used
// for job management.
//
// command is stored for status/logging only and does not affect behavior.
//
// The returned Process is immediately ready to serve ReadLine/WriteLine traffic.
// Done is closed when the upstream's stdout reaches EOF (process exited or pipe
// closed). There is no proc.Wait() — AttachFromFDs does not own the process
// lifecycle and will not signal or wait for it.
//
// Returns an error if pid <= 0 (no handoff transfers PID 0 — Linux reserves it
// for "no process" and Windows' PID 0 is the System Idle Process), if stdinFD
// or stdoutFD is zero (the stdio contract never transfers fd 0/1/2), or if
// os.NewFile rejects the handle.
func AttachFromFDs(pid int, stdinFD, stdoutFD uintptr, command string) (*Process, error) {
	if pid <= 0 {
		return nil, errors.New("upstream: AttachFromFDs: pid must be > 0")
	}
	if stdinFD == 0 {
		return nil, errors.New("upstream: AttachFromFDs: stdinFD must be non-zero")
	}
	if stdoutFD == 0 {
		return nil, errors.New("upstream: AttachFromFDs: stdoutFD must be non-zero")
	}

	stdinFile := os.NewFile(stdinFD, fmt.Sprintf("upstream-stdin-%d", pid))
	stdoutFile := os.NewFile(stdoutFD, fmt.Sprintf("upstream-stdout-%d", pid))
	if stdinFile == nil || stdoutFile == nil {
		return nil, fmt.Errorf("upstream: AttachFromFDs: os.NewFile rejected handle (stdinFD=%d stdoutFD=%d)", stdinFD, stdoutFD)
	}

	p := &Process{
		stdin:      stdinFile,
		stdout:     stdoutFile,
		stdinFile:  stdinFile,
		stdoutFile: stdoutFile,
		stdinFD:    stdinFD,
		stdoutFD:   stdoutFD,
		spawnPgid:  pid,
		lineBuf:    newLineBuffer(),
		Done:       make(chan struct{}),
	}

	// Drain stdout into the line buffer. Same pattern as Start() — a
	// background goroutine feeds lines so ReadLine never blocks on I/O
	// directly. Done is closed once the scanner sees EOF, signalling that
	// the upstream has stopped writing (process exited or pipe closed).
	go func() {
		defer func() { _ = stdoutFile.Close() }()
		scanner := bufio.NewScanner(stdoutFile)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB — matches Start()
		for scanner.Scan() {
			line := scanner.Bytes()
			cp := make([]byte, len(line))
			copy(cp, line)
			p.lineBuf.push(cp)
		}
		err := scanner.Err()
		if errors.Is(err, os.ErrClosed) {
			err = nil // expected clean-close path
		}
		p.lineBuf.markDoneWithErr(err)
		// Signal upstream exit. ExitErr = io.EOF because we cannot call
		// proc.Wait() — we do not own the process. Callers that need the
		// real OS exit code must obtain it via the handoff protocol.
		p.ExitErr = io.EOF
		close(p.Done)
	}()

	return p, nil
}
