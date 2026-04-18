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
	proc   *procgroup.Process
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	lineBuf *lineBuffer

	mu           sync.Mutex
	closed       bool
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
	// Stdin and stderr can still use StdinPipe / StderrPipe because:
	// - stdin: caller drives writes; pipe close is intentional shutdown signal
	// - stderr: the drain goroutine's lifetime is bound by stderr EOF, not
	//   Wait — the scanner pattern is race-free on stderr because it ends
	//   when the child exits (EOF from kernel), not when Wait runs.
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

	p.proc = proc
	p.lineBuf = newLineBuffer()

	// Drain stdout into an internal buffer before the OS pipe is closed.
	// ReadLine reads from memory, not directly from cmd.StdoutPipe, so
	// proc.Wait() no longer races with external readers.
	go func() {
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

	// Bridge procgroup's done channel into upstream's Done channel and capture
	// the exit error.
	go func() {
		p.ExitErr = proc.Wait()
		close(p.Done)
	}()

	// Forward stderr to logger (prefix with [upstream]) so diagnostics are visible.
	// Buffer must be large enough for long lines (MSBuild paths, NuGet restore logs).
	// If scanner stops reading (line too long), stderr pipe fills → upstream blocks.
	//
	// When the caller supplies a logger, route stderr there — the daemon runs
	// detached with os.Stderr discarded, so writing to os.Stderr would drop
	// every crash diagnostic (the exact failure mode we are fixing here).
	go func() {
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
	p.closed = true
	stdinWait := p.drainTimeout
	p.mu.Unlock()

	if stdinWait <= 0 {
		stdinWait = 5 * time.Second // default
	}

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
