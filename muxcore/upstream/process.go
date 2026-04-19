// Package upstream manages spawning and communicating with MCP server processes.
//
// An upstream process is a child process that speaks JSON-RPC 2.0 over stdio.
// The Process struct handles spawning, writing requests to stdin, reading
// responses from stdout, and detecting process exit.
package upstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/procgroup"
)

// SoftClose closes the upstream process gracefully via stdin close, waiting up
// to timeout for voluntary exit before escalating to GracefulKill.
//
// Graceful soft-close sequence (US3):
//  1. Close stdin — polite MCP shutdown signal (well-behaved servers exit on EOF).
//  2. Wait up to timeout for voluntary exit. Default 30s when called by the reaper.
//  3. If still alive after timeout: proc.GracefulKill(3s) — SIGTERM→wait→SIGKILL.
//
// Returns the upstream exit code (0 = clean voluntary exit, non-zero = forced)
// and any error from the kill escalation.
// Safe to call after Close() — returns (0, nil) if already closed or detached.
func (p *Process) SoftClose(timeout time.Duration) (int, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, nil
	}
	if p.detached {
		p.mu.Unlock()
		return 0, nil
	}
	p.closed = true
	p.mu.Unlock()

	// Release the Windows Job Object handle (no-op on non-Windows).
	releaseJobHandle(p)

	// Phase 1: close stdin — polite MCP shutdown signal.
	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	// Phase 2: wait for voluntary exit.
	select {
	case <-p.Done:
		// Exited cleanly on its own.
		return softCloseExitCode(p.ExitErr), nil
	case <-time.After(timeout):
	}

	// Phase 3: timeout — escalate to GracefulKill.
	if p.proc != nil {
		killErr := p.proc.GracefulKill(3 * time.Second)
		// Wait for the process to fully exit so ExitErr is populated.
		select {
		case <-p.Done:
		case <-time.After(5 * time.Second):
			return -1, fmt.Errorf("upstream: process did not exit after GracefulKill")
		}
		if killErr != nil {
			return softCloseExitCode(p.ExitErr), killErr
		}
		return softCloseExitCode(p.ExitErr), nil
	}

	return -1, nil
}

// softCloseExitCode extracts the process exit code from a Wait() error.
// Returns 0 for nil (clean exit), the numeric exit code for *exec.ExitError,
// and 1 for any other non-nil error.
func softCloseExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

// ErrAlreadyClosed is returned by Detach if the process has already been closed.
var ErrAlreadyClosed = errors.New("upstream: process already closed")

// ErrAlreadyDetached is returned by Detach if Detach has already been called.
var ErrAlreadyDetached = errors.New("upstream: process already detached")

// ErrDetachUnsupported is returned by Detach when the Process is not backed
// by an OS child process (for example, NewProcessFromHandler).
var ErrDetachUnsupported = errors.New("upstream: detach unsupported for handler-based process")

// lineBuffer is a mutex-protected deque of lines for buffering stdout.
// A drain goroutine pushes lines; ReadLine pops them.
type lineBuffer struct {
	mu    sync.Mutex
	cond  *sync.Cond
	lines [][]byte
	done  bool
	err   error // scanner error, if any; set once by markDoneWithErr
}

func newLineBuffer() *lineBuffer {
	lb := &lineBuffer{}
	lb.cond = sync.NewCond(&lb.mu)
	return lb
}

func (lb *lineBuffer) push(line []byte) {
	lb.mu.Lock()
	lb.lines = append(lb.lines, line)
	lb.mu.Unlock()
	lb.cond.Signal()
}

// markDoneWithErr marks the buffer as drained and records any scanner error.
// Callers should pass scanner.Err() so that ReadLine can surface the real
// failure instead of a misleading io.EOF when the scanner stopped due to an
// oversized line or other I/O error.
func (lb *lineBuffer) markDoneWithErr(err error) {
	lb.mu.Lock()
	lb.done = true
	lb.err = err
	lb.mu.Unlock()
	lb.cond.Broadcast()
}

// pop blocks until a line is available or the buffer is drained.
// Returns the scanner error (if any) in preference to io.EOF once the buffer
// is empty, so callers can distinguish a clean EOF from a scan failure.
func (lb *lineBuffer) pop() ([]byte, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	for len(lb.lines) == 0 && !lb.done {
		lb.cond.Wait()
	}
	if len(lb.lines) == 0 {
		if lb.err != nil {
			return nil, lb.err
		}
		return nil, io.EOF
	}

	line := lb.lines[0]
	lb.lines[0] = nil // clear reference so GC can reclaim the backing array slot
	lb.lines = lb.lines[1:]
	return line, nil
}

// Process represents a running upstream MCP server process.
type Process struct {
	proc       *procgroup.Process
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	stdinFile  *os.File // raw OS file for Detach; nil for handler-based processes
	stdoutFile *os.File // raw OS file for Detach; nil for handler-based processes
	// stdinFD / stdoutFD cache the OS file descriptors captured ONCE at
	// PreStart time — before any reader/writer goroutine touches the File
	// objects. Calling (*os.File).Fd() from Detach() at runtime would race
	// with the spawn goroutine's internal reads/writes on the same file
	// (observed on CI ubuntu -race: TestDetach_DoubleDetach). Caching at
	// spawn time eliminates the race.
	stdinFD  uintptr
	stdoutFD uintptr
	// spawnPgid is the process group ID of the spawned upstream. On Unix,
	// Setpgid=true causes the child's PGID to equal its PID (kernel guarantee).
	// Set after procgroup.Spawn returns successfully. Zero for handler-based processes.
	spawnPgid int
	// jobHandle is the Windows Job Object handle for this upstream. 0 on
	// non-Windows or if creation failed (graceful degradation per AC8).
	jobHandle uintptr

	lineBuf *lineBuffer

	mu           sync.Mutex
	closed       bool
	detached     bool
	drainTimeout time.Duration // from x-mux.drainTimeout; overrides default 5s stdin-close wait

	// Done is closed when the process exits.
	Done chan struct{}
	// ExitErr holds the error from Wait(), available after Done is closed.
	ExitErr error
}

// SetDrainTimeout sets the graceful shutdown wait time after stdin close.
// Called by the owner when x-mux.drainTimeout capability is parsed.
func (p *Process) SetDrainTimeout(d time.Duration) {
	p.mu.Lock()
	p.drainTimeout = d
	p.mu.Unlock()
}

// Start spawns an upstream process with the given command, args, environment, and working directory.
// When env is non-empty, it is used as the complete process environment (converted from map to slice).
// When env is nil/empty, the current process environment (os.Environ()) is used as fallback.
// If cwd is non-empty, the child process runs in that directory.
//
// If logger is non-nil, upstream stderr is forwarded to it (one log.Printf per line,
// prefix "[upstream:PID] "). If logger is nil, stderr is forwarded to os.Stderr of the
// calling process. In daemon mode os.Stderr is typically discarded (daemon is detached
// with cmd.Stderr=nil), so upstream crash diagnostics get lost — callers running under
// a daemon MUST pass a logger to preserve stderr.
func Start(command string, args []string, env map[string]string, cwd string, logger *log.Logger) (*Process, error) {
	var merged []string
	if len(env) > 0 {
		// Full session env provided — use it directly, no os.Environ() merge.
		merged = make([]string, 0, len(env))
		for k, v := range env {
			merged = append(merged, fmt.Sprintf("%s=%s", k, v))
		}
	} else {
		// No session env (nil map) — fallback to daemon process env.
		// This covers owners created without spawn request (tests, direct construction).
		merged = os.Environ()
	}

	p := &Process{
		Done: make(chan struct{}),
	}

	// Use os.Pipe() for stdout instead of cmd.StdoutPipe(). StdoutPipe's docs
	// forbid reading concurrently with cmd.Wait — Wait closes the pipe's read
	// side and any unread data in the pipe buffer is lost on close. With
	// os.Pipe we own both ends: the child process writes to stdoutW (cmd.Stdout),
	// we read from stdoutR, and the pipe stays open until we close it
	// ourselves in the drain goroutine. Wait no longer races with reads.
	//
	// Stdin can still use StdinPipe: the caller drives writes and the pipe
	// close is an intentional shutdown signal, not a race.
	//
	// Stderr uses StderrPipe but is synchronized the same way as stdout:
	// a stderrDrained channel ensures proc.Wait() is only called after the
	// stderr scanner goroutine has finished reading. Without that ordering,
	// Cmd.Wait() closes the StderrPipe while the scanner may still be reading,
	// which can silently drop the last lines of crash diagnostics.
	stdoutR, stdoutW, pipeErr := os.Pipe()
	if pipeErr != nil {
		return nil, fmt.Errorf("upstream: os.Pipe: %w", pipeErr)
	}

	// Capture pipe handles inside the PreStart callback so procgroup still owns
	// the exec.Cmd lifecycle (platform isolation, job object, tree kill).
	var captureErr error
	opts := procgroup.Options{
		Command: command,
		Args:    args,
		Dir:     cwd,
		Env:     merged,
		// stdout: use our own pipe (see rationale above). stdin / stderr
		// still use exec.Cmd's built-in pipes.
		Stdout: stdoutW,
		PreStart: func(cmd *exec.Cmd) error {
			stdin, err := cmd.StdinPipe()
			if err != nil {
				captureErr = fmt.Errorf("upstream: stdin pipe: %w", err)
				return captureErr
			}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				captureErr = fmt.Errorf("upstream: stderr pipe: %w", err)
				return captureErr
			}
			p.stdin = stdin
			p.stdout = stdoutR
			p.stderr = stderr
			// Capture raw *os.File references for Detach(). stdoutR is always
			// *os.File (from os.Pipe above). stdin from StdinPipe is also *os.File
			// on all supported platforms; the type assertion is defensive.
			if f, ok := stdin.(*os.File); ok {
				p.stdinFile = f
				p.stdinFD = f.Fd() // cache pre-goroutine (NFR-5, race-safe)
			}
			p.stdoutFile = stdoutR
			p.stdoutFD = stdoutR.Fd() // cache pre-goroutine (race-safe)
			cmd.SysProcAttr = applyUnixSpawnAttrs(cmd.SysProcAttr)
			return nil
		},
	}

	proc, err := procgroup.Spawn(opts)
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("upstream: spawn %s: %w", command, err)
	}
	if captureErr != nil {
		// Should not happen — Spawn already propagates PreStart errors — but be safe.
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = proc.Kill()
		return nil, captureErr
	}

	// Close our copy of the write end — the child inherited its own copy via
	// exec. When the child exits, its write end closes; our drain goroutine's
	// Scanner then sees EOF naturally. Without this close, the pipe never
	// reaches EOF because one writer (us) remains open forever.
	_ = stdoutW.Close()

	// Windows: assign upstream to its own Job Object for per-process kill
	// semantics (C1). No-op on non-Windows (see spawn_other.go).
	afterSpawnWindows(p, proc.PID())

	p.proc = proc
	// On Unix, Setpgid=true guarantees PGID == PID after spawn.
	// We read proc.PID() to stay within the procgroup abstraction.
	// spawnPgid is zero for handler-based processes.
	if pid := proc.PID(); pid != 0 {
		p.spawnPgid = pid
	}
	p.lineBuf = newLineBuffer()

	// stdoutDrained and stderrDrained are closed by their respective drain
	// goroutines once the scanner has finished and markDoneWithErr has been
	// called. The Wait goroutine blocks on both before calling proc.Wait(),
	// guaranteeing that all output is consumed before the OS reclaims the
	// pipes. Without this ordering, Cmd.Wait() closes the pipes (stdout via
	// our os.Pipe close, stderr via StderrPipe's internal close) while the
	// scanners may still be reading, causing data loss or spurious errors.
	stdoutDrained := make(chan struct{})
	stderrDrained := make(chan struct{})

	// Drain stdout into an internal buffer before the OS pipe is closed.
	// ReadLine reads from memory, not directly from cmd.StdoutPipe, so
	// proc.Wait() no longer races with external readers.
	go func() {
		defer close(stdoutDrained)
		defer func() { _ = p.stdout.Close() }()
		scanner := bufio.NewScanner(p.stdout)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer
		for scanner.Scan() {
			line := scanner.Bytes()
			cp := make([]byte, len(line))
			copy(cp, line)
			p.lineBuf.push(cp)
		}
		// Propagate real scanner errors (e.g., bufio.ErrTooLong) so ReadLine
		// surfaces them instead of io.EOF. But cmd.StdoutPipe closes on
		// process exit and the scanner then returns os.ErrClosed ("read |N:
		// file already closed") — that is the expected clean-close path, not
		// a failure, so normalise it to nil (pop() will return io.EOF).
		err := scanner.Err()
		if errors.Is(err, os.ErrClosed) {
			err = nil
		}
		p.lineBuf.markDoneWithErr(err)
	}()

	// Forward stderr to logger (prefix with [upstream]) so diagnostics are visible.
	// Buffer must be large enough for long lines (MSBuild paths, NuGet restore logs).
	// If scanner stops reading (line too long), stderr pipe fills → upstream blocks.
	//
	// When the caller supplies a logger, route stderr there — the daemon runs
	// detached with os.Stderr discarded, so writing to os.Stderr would drop
	// every crash diagnostic (the exact failure mode we are fixing here).
	//
	// close(stderrDrained) signals the Wait goroutine below that all stderr
	// output has been consumed; proc.Wait() is only called after both drain
	// goroutines have finished (see stdoutDrained/stderrDrained ordering).
	go func() {
		defer close(stderrDrained)
		scanner := bufio.NewScanner(p.stderr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024) // 64KB — handles any build output line
		for scanner.Scan() {
			if logger != nil {
				logger.Printf("[upstream:%d] %s", proc.PID(), scanner.Text())
			} else {
				fmt.Fprintf(os.Stderr, "[upstream:%d] %s\n", proc.PID(), scanner.Text())
			}
		}
	}()

	// Bridge procgroup's done channel into upstream's Done channel and capture
	// the exit error. Wait for both drain goroutines to finish before calling
	// proc.Wait() — this ensures stdout and stderr are fully consumed before
	// the OS reclaims the pipes, preventing data loss and spurious errors.
	go func() {
		<-stdoutDrained
		<-stderrDrained
		p.ExitErr = proc.Wait()
		close(p.Done)
	}()

	return p, nil
}

// WriteLine sends a line of data to the upstream process stdin, followed by newline.
func (p *Process) WriteLine(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return fmt.Errorf("upstream: process closed")
	}

	_, err := p.stdin.Write(data)
	if err != nil {
		return fmt.Errorf("upstream: write: %w", err)
	}

	_, err = p.stdin.Write([]byte("\n"))
	if err != nil {
		return fmt.Errorf("upstream: write newline: %w", err)
	}

	return nil
}

// ReadLine reads the next line from the upstream process stdout.
// Returns io.EOF when the process has exited and all output has been consumed.
// All lines written before process exit are available even after Done is closed.
func (p *Process) ReadLine() ([]byte, error) {
	return p.lineBuf.pop()
}

// Close terminates the upstream process gracefully.
//
// Graceful shutdown sequence:
//  1. Close stdin — the MCP-standard way to signal "session ended". Well-behaved
//     servers (Python MCP SDK, Node SDK) exit on stdin close.
//  2. Wait for the process to exit on its own. Duration is drainTimeout from
//     x-mux.drainTimeout capability (default: 5s).
//  3. If still alive: proc.GracefulKill() — SIGTERM→wait→SIGKILL on Unix,
//     CTRL_BREAK_EVENT→wait→TerminateJobObject on Windows. Kills the whole tree.
func (p *Process) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	if p.detached {
		// Ownership transferred via Detach — do not signal the process.
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	stdinWait := p.drainTimeout
	p.mu.Unlock()

	if stdinWait <= 0 {
		stdinWait = 5 * time.Second // default
	}

	// Release the Windows Job Object handle (no-op on non-Windows).
	// Done before stdin close so the handle is freed regardless of whether
	// the process exits cleanly or requires a forced kill below.
	releaseJobHandle(p)

	// Phase 1: close stdin — the polite MCP shutdown signal.
	// Nil-safe for test-constructed Process values (no real process attached).
	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	// Phase 2: wait for voluntary exit (drainTimeout or default 5s).
	select {
	case <-p.Done:
		return nil // process exited cleanly
	case <-time.After(stdinWait):
	}

	// Phase 3: delegate tree kill to procgroup — SIGTERM→wait→SIGKILL on Unix,
	// CTRL_BREAK_EVENT→wait→TerminateJobObject on Windows.
	if p.proc != nil {
		return p.proc.GracefulKill(3 * time.Second)
	}

	return nil
}

// PID returns the process ID of the upstream process.
func (p *Process) PID() int {
	if p.proc != nil {
		return p.proc.PID()
	}
	return 0
}

// Detach releases ownership of the upstream process without terminating it.
// It returns the process PID and the raw OS file descriptors for stdin and
// stdout, so that a successor daemon can reattach to the running process via
// FD passing (Unix SCM_RIGHTS) or DuplicateHandle (Windows).
//
// After Detach returns successfully, the caller owns the returned file
// descriptors and is responsible for transferring or closing them.
// Subsequent calls to Close() are no-ops — the process is not signaled.
//
// Returns ErrAlreadyClosed if the process has been closed.
// Returns ErrAlreadyDetached if Detach has already been called.
func (p *Process) Detach() (pid int, stdinFD uintptr, stdoutFD uintptr, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return 0, 0, 0, ErrAlreadyClosed
	}
	if p.detached {
		return 0, 0, 0, ErrAlreadyDetached
	}
	if p.proc == nil {
		return 0, 0, 0, ErrDetachUnsupported
	}
	p.detached = true

	pid = p.proc.PID()

	// Return cached FDs (captured in PreStart, race-safe). Calling
	// (*os.File).Fd() here would race with the spawn goroutines' Read/Write
	// on the same file objects — observed on ubuntu -race as a data race
	// between os.(*File).fd() and os.(*File).Fd() in TestDetach_DoubleDetach.
	if p.stdinFile != nil {
		stdinFD = p.stdinFD
	}
	if p.stdoutFile != nil {
		stdoutFD = p.stdoutFD
	}

	// Release our copy of the Windows Job Object handle. On the planned-handoff
	// path (T020), the caller must DuplicateHandle the job into the successor
	// process BEFORE calling Detach — by this point our copy is redundant and
	// must be freed to avoid a kernel handle leak on every spawned upstream.
	// No-op on non-Windows (see spawn_other.go).
	releaseJobHandle(p)

	return pid, stdinFD, stdoutFD, nil
}

// NewProcessFromHandler creates a Process that runs an in-process handler via
// io.Pipe instead of spawning a subprocess. The handler receives the write end
// of the stdin pipe and the read end of the stdout pipe. Done is closed when
// the handler goroutine returns.
//
// The returned Process has PID() == 0 and no procgroup backing; Close() closes
// the stdin pipe (EOF signal) and waits for the handler to exit.
func NewProcessFromHandler(ctx context.Context, handler func(ctx context.Context, stdin io.Reader, stdout io.Writer) error) *Process {
	// stdinR → handler reads its "stdin" from here
	// stdinW → process.WriteLine writes here
	stdinR, stdinW := io.Pipe()

	// stdoutR → process.ReadLine reads from here
	// stdoutW → handler writes its responses here
	stdoutR, stdoutW := io.Pipe()

	p := &Process{
		stdin:   stdinW,
		stdout:  stdoutR,
		Done:    make(chan struct{}),
		lineBuf: newLineBuffer(),
	}

	go func() {
		scanner := bufio.NewScanner(stdoutR)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			cp := make([]byte, len(line))
			copy(cp, line)
			p.lineBuf.push(cp)
		}
		// Propagate real scanner errors (e.g., bufio.ErrTooLong) but normalise
		// the expected clean-close path — io.Pipe.CloseWithError produces
		// io.ErrClosedPipe when the handler exits; os.ErrClosed when stdin
		// closure cascades — neither is a real failure.
		err := scanner.Err()
		if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			err = nil
		}
		p.lineBuf.markDoneWithErr(err)
	}()

	go func() {
		// handler runs until it returns or ctx is cancelled.
		err := handler(ctx, stdinR, stdoutW)
		// Signal EOF on stdout so ReadLine returns io.EOF.
		stdoutW.CloseWithError(err)
		// Drain stdin pipe (in case handler exited before reading all input).
		stdinR.CloseWithError(err)
		p.ExitErr = err
		close(p.Done)
	}()

	return p
}
