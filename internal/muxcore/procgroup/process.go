// Package procgroup manages child processes with full process-tree lifecycle control.
// On Unix, the process runs in its own process group (Setpgid). On Windows, it is
// placed in a Job Object with KillOnJobClose so the entire tree is reaped when the
// handle is released.
package procgroup

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"
)

// Options configures how a process is spawned.
type Options struct {
	Command string
	Args    []string
	Dir     string     // working directory (empty = inherit)
	Env     []string   // environment (nil = inherit)
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Logger  *log.Logger // optional logger
}

// Process manages a child process with its entire process tree.
type Process struct {
	cmd  *exec.Cmd
	pid  int
	done chan struct{}

	mu       sync.Mutex
	exitErr  error // set once the reaper goroutine calls cmd.Wait()
	exitCode int   // set alongside exitErr; -1 while running

	platform platformState // platform-specific fields
}

// Spawn creates and starts a new process in its own process group / job object.
func Spawn(opts Options) (*Process, error) {
	cmd := exec.Command(opts.Command, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdin = opts.Stdin
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr

	p := &Process{
		cmd:      cmd,
		done:     make(chan struct{}),
		exitCode: -1,
	}

	if err := configurePlatform(cmd, p); err != nil {
		return nil, fmt.Errorf("procgroup: configure platform: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("procgroup: start %q: %w", opts.Command, err)
	}

	p.pid = cmd.Process.Pid

	// postStart performs any platform work that requires the PID (e.g. assigning
	// the process to a Windows Job Object).
	if err := p.postStart(); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("procgroup: post-start platform setup: %w", err)
	}

	// Reaper goroutine: waits for the process and records the exit status.
	go func() {
		err := cmd.Wait()

		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}

		p.mu.Lock()
		p.exitErr = err
		p.exitCode = code
		p.mu.Unlock()

		if opts.Logger != nil {
			opts.Logger.Printf("procgroup: pid %d exited (code=%d): %v", p.pid, code, err)
		}

		close(p.done)
	}()

	return p, nil
}

// Wait blocks until the process exits and returns the exit error (nil = clean exit).
func (p *Process) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

// Alive returns true if the process is still running.
func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// PID returns the OS process ID of the leader process.
func (p *Process) PID() int {
	return p.pid
}

// Done returns a channel that is closed when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// GracefulKill sends a graceful signal, waits up to timeout, then force-kills.
//
// On Unix: SIGTERM to process group → wait → SIGKILL to process group.
// On Windows: CTRL_BREAK_EVENT to PID → wait → TerminateJobObject.
func (p *Process) GracefulKill(timeout time.Duration) error {
	if !p.Alive() {
		return nil
	}
	return p.gracefulKillPlatform(timeout)
}

// Kill immediately force-kills the entire process tree.
func (p *Process) Kill() error {
	if !p.Alive() {
		return nil
	}
	return p.killPlatform()
}

// ExitCode returns the process exit code, or -1 if the process has not exited yet.
func (p *Process) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}
