// Package procgroup manages child processes with full process-tree lifecycle control.
// On Unix, the process runs in its own process group (Setpgid). On Windows, it
// is placed in a Job Object that defaults to KillOnJobClose so the entire tree
// is reaped when the owning process exits.
package procgroup

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

const processTreeWaitTimeout = 5 * time.Second

// ErrTreeRetirementUnproven reports that the leader exited but procgroup could
// not prove that every process in its group or Job Object was retired.
var ErrTreeRetirementUnproven = errors.New("procgroup: process-tree retirement unproven")

// Options configures how a process is spawned.
type Options struct {
	Command string
	Args    []string
	Dir     string   // working directory (empty = inherit)
	Env     []string // environment (nil = inherit)
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Logger  *log.Logger // optional logger

	// DisableTree lets a caller that owns its own transferable tree authority
	// use procgroup only for exec/reaping. The zero value preserves full tree
	// management for all existing callers.
	DisableTree bool

	// StartSuspended keeps a Windows child suspended until procgroup installs
	// its Job Object authority, then resumes it. Other platforms ignore this
	// option. The zero value preserves existing spawn behavior.
	StartSuspended bool

	// PreStart is an optional callback invoked after the exec.Cmd is built and
	// platform-configured but before cmd.Start() is called. Callers can use
	// this to call cmd.StdinPipe / cmd.StdoutPipe / cmd.StderrPipe and capture
	// the resulting io.ReadCloser / io.WriteCloser handles.
	// If PreStart returns a non-nil error, Spawn aborts and propagates the error.
	PreStart func(cmd *exec.Cmd) error
}

// Process manages a child process with its entire process tree.
type Process struct {
	cmd        *exec.Cmd
	pid        int
	leaderDone chan struct{}
	done       chan struct{}

	mu       sync.Mutex
	exitErr  error // set once the reaper goroutine calls cmd.Wait()
	exitCode int   // set alongside exitErr; -1 while running

	disableTree    bool
	startSuspended bool
	platform       platformState // platform-specific fields
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
		cmd:            cmd,
		leaderDone:     make(chan struct{}),
		done:           make(chan struct{}),
		exitCode:       -1,
		disableTree:    opts.DisableTree,
		startSuspended: opts.StartSuspended,
	}

	if err := configurePlatform(cmd, p); err != nil {
		return nil, fmt.Errorf("procgroup: configure platform: %w", err)
	}

	if opts.PreStart != nil {
		if err := opts.PreStart(cmd); err != nil {
			preStartErr := fmt.Errorf("procgroup: pre-start: %w", err)
			if cleanupErr := p.cleanupPlatform(); cleanupErr != nil {
				return nil, errors.Join(preStartErr, fmt.Errorf("procgroup: platform cleanup: %w", cleanupErr))
			}
			return nil, preStartErr
		}
	}

	if err := cmd.Start(); err != nil {
		startErr := fmt.Errorf("procgroup: start %q: %w", opts.Command, err)
		if cleanupErr := p.cleanupPlatform(); cleanupErr != nil {
			return nil, errors.Join(startErr, fmt.Errorf("procgroup: platform cleanup: %w", cleanupErr))
		}
		return nil, startErr
	}

	p.pid = cmd.Process.Pid
	if err := p.postStart(); err != nil {
		killErr := cmd.Process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		_, waitErr := cmd.Process.Wait()
		if errors.Is(waitErr, os.ErrProcessDone) {
			waitErr = nil
		}
		close(p.leaderDone)
		cleanupErr := p.cleanupPlatform()
		return nil, postStartFailure(err, errors.Join(killErr, waitErr, cleanupErr))
	}

	go func() {
		leaderErr := cmd.Wait()

		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		close(p.leaderDone)

		authorityErr := p.reapPlatform()
		if authorityErr != nil {
			authorityErr = fmt.Errorf("%w: %v", ErrTreeRetirementUnproven, authorityErr)
		}
		p.mu.Lock()
		p.exitErr = errors.Join(leaderErr, authorityErr)
		p.exitCode = code
		p.mu.Unlock()

		if opts.Logger != nil {
			opts.Logger.Printf("procgroup: pid %d exited (code=%d): %v", p.pid, code, p.exitErr)
		}
		close(p.done)
	}()

	return p, nil
}

func postStartFailure(setupErr, rollbackErr error) error {
	err := fmt.Errorf("procgroup: post-start platform setup: %w", setupErr)
	if rollbackErr == nil {
		return err
	}
	return errors.Join(err, fmt.Errorf("%w: %w", ErrTreeRetirementUnproven, rollbackErr))
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

// AllowSurviveParentExit disables only the platform policy that would kill
// this process tree when the spawning process exits. Explicit Kill and Wait
// authority remains available until the caller commits the detached start.
// On Unix, parent exit does not implicitly terminate a process group, so this
// operation is a no-op.
func (p *Process) AllowSurviveParentExit() error {
	if p == nil {
		return errors.New("procgroup: nil process")
	}
	if p.disableTree {
		return errors.New("procgroup: tree management is disabled")
	}
	return p.allowSurviveParentExitPlatform()
}

// GracefulKill sends a graceful signal, waits up to timeout, then force-kills.
//
// On Unix: SIGTERM to process group → wait → SIGKILL to process group.
// On Windows: CTRL_BREAK_EVENT to PID → wait → TerminateJobObject.
func (p *Process) GracefulKill(timeout time.Duration) error {
	if !p.Alive() {
		return p.killPlatform()
	}
	return p.gracefulKillPlatform(timeout)
}

// Kill immediately force-kills the entire process tree.
func (p *Process) Kill() error {
	return p.killPlatform()
}

// ExitCode returns the process exit code, or -1 if the process has not exited yet.
func (p *Process) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}
