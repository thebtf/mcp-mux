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

// Process represents a running upstream MCP server process.
type Process struct {
	proc   *procgroup.Process
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	scanner *bufio.Scanner

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

	// Capture pipe handles inside the PreStart callback so procgroup still owns
	// the exec.Cmd lifecycle (platform isolation, job object, tree kill).
	var captureErr error
	opts := procgroup.Options{
		Command: command,
		Args:    args,
		Dir:     cwd,
		Env:     merged,
		// Stdin/Stdout/Stderr are left nil here; pipes are set up in PreStart.
		PreStart: func(cmd *exec.Cmd) error {
			stdin, err := cmd.StdinPipe()
			if err != nil {
				captureErr = fmt.Errorf("upstream: stdin pipe: %w", err)
				return captureErr
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				captureErr = fmt.Errorf("upstream: stdout pipe: %w", err)
				return captureErr
			}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				captureErr = fmt.Errorf("upstream: stderr pipe: %w", err)
				return captureErr
			}
			p.stdin = stdin
			p.stdout = stdout
			p.stderr = stderr
			return nil
		},
	}

	proc, err := procgroup.Spawn(opts)
	if err != nil {
		return nil, fmt.Errorf("upstream: spawn %s: %w", command, err)
	}
	if captureErr != nil {
		// Should not happen — Spawn already propagates PreStart errors — but be safe.
		_ = proc.Kill()
		return nil, captureErr
	}

	p.proc = proc
	p.scanner = bufio.NewScanner(p.stdout)
	p.scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer

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
// Returns io.EOF when the process closes stdout.
//
// Error normalisation: exec.Cmd.StdoutPipe is documented to close the pipe
// after cmd.Wait() returns, and we run Wait in a background goroutine (see
// Start's "bridge procgroup's done channel" block). A caller that dials
// ReadLine AFTER Wait has closed the pipe sees "read |0: file already
// closed" from os.ErrClosed. Semantically this is EOF: no more bytes will
// ever arrive. Map it so callers can short-circuit on io.EOF without
// stringy matches (upstream package and product code already treat EOF as
// clean end-of-stream — see muxcore/owner/owner.go readUpstream).
//
// This normalisation does NOT mask the deeper race (Wait concurrently
// closing the pipe with ReadLine); it just prevents a misleading error
// surface. Documented with full root-cause analysis in TECHNICAL_DEBT.md
// under "upstream.Start Wait-vs-ReadLine race".
func (p *Process) ReadLine() ([]byte, error) {
	if p.scanner.Scan() {
		// Return a copy to avoid scanner buffer reuse issues
		line := p.scanner.Bytes()
		result := make([]byte, len(line))
		copy(result, line)
		return result, nil
	}

	if err := p.scanner.Err(); err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("upstream: read: %w", err)
	}

	return nil, io.EOF
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
		stdin:  stdinW,
		stdout: stdoutR,
		Done:   make(chan struct{}),
	}
	p.scanner = bufio.NewScanner(stdoutR)
	p.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

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
