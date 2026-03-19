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
)

// Process represents a running upstream MCP server process.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	scanner *bufio.Scanner

	mu     sync.Mutex
	closed bool

	// Done is closed when the process exits.
	Done chan struct{}
	// ExitErr holds the error from Wait(), available after Done is closed.
	ExitErr error
}

// Start spawns an upstream process with the given command, args, environment, and working directory.
// The env map is merged with the current process environment.
// If cwd is non-empty, the child process runs in that directory.
func Start(command string, args []string, env map[string]string, cwd string) (*Process, error) {
	cmd := exec.Command(command, args...)

	// Set working directory if specified
	if cwd != "" {
		cmd.Dir = cwd
	}

	// Merge environment
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream: stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream: stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("upstream: start %s: %w", command, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer

	p := &Process{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		scanner: scanner,
		Done:    make(chan struct{}),
	}

	// Monitor process exit in background
	go func() {
		p.ExitErr = cmd.Wait()
		close(p.Done)
	}()

	// Forward stderr to logger (prefix with [upstream]) so diagnostics are visible.
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 4096), 4096)
		for scanner.Scan() {
			fmt.Fprintf(os.Stderr, "[upstream:%d] %s\n", p.cmd.Process.Pid, scanner.Text())
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

// Close terminates the upstream process.
func (p *Process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true

	// Close stdin to signal the process
	_ = p.stdin.Close()

	// Kill if still running
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}

	return nil
}

// PID returns the process ID of the upstream process.
func (p *Process) PID() int {
	if p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

