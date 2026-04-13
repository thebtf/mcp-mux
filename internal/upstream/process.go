// Package upstream manages spawning and communicating with MCP server processes.
//
// An upstream process is a child process that speaks JSON-RPC 2.0 over stdio.
// The Process struct handles spawning, writing requests to stdin, reading
// responses from stdout, and detecting process exit.
package upstream

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/procgroup"
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
// The env map is merged with the current process environment.
// If cwd is non-empty, the child process runs in that directory.
func Start(command string, args []string, env map[string]string, cwd string) (*Process, error) {
	// Build merged environment: start from current process env, then overlay caller-supplied vars.
	merged := os.Environ()
	for k, v := range env {
		merged = append(merged, fmt.Sprintf("%s=%s", k, v))
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
	go func() {
		scanner := bufio.NewScanner(p.stderr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024) // 64KB — handles any build output line
		for scanner.Scan() {
			fmt.Fprintf(os.Stderr, "[upstream:%d] %s\n", proc.PID(), scanner.Text())
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
func (p *Process) ReadLine() ([]byte, error) {
	if p.scanner.Scan() {
		// Return a copy to avoid scanner buffer reuse issues
		line := p.scanner.Bytes()
		result := make([]byte, len(line))
		copy(result, line)
		return result, nil
	}

	if err := p.scanner.Err(); err != nil {
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
