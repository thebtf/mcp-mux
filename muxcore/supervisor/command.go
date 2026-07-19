package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/procgroup"
)

const defaultCommandGracefulTimeout = time.Second

var (
	// ErrStartRollbackUnproven reports that a failed StartFunc attempt may still
	// own a child process, process tree, or admission endpoint. Run treats this
	// as terminal unless returned Child authority proves full tree retirement;
	// admission authority is closed but is not process-retirement proof.
	ErrStartRollbackUnproven = errors.New("supervisor: failed start rollback unproven")

	// ErrProcessTreeUnproven reports that a command leader terminated without a
	// successful full process-group or Job Object retirement proof.
	ErrProcessTreeUnproven = errors.New("supervisor: process-tree retirement unproven")
)

// Command describes one subprocess child.
type Command struct {
	Path   string
	Args   []string
	Env    []string
	Dir    string
	Stderr io.Writer
}

// CommandChild is a Child backed by procgroup full process-tree authority.
type CommandChild struct {
	process *procgroup.Process
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	pid     int

	wait     chan Exit
	exitDone chan struct{}
	exitMu   sync.Mutex
	exit     Exit

	stopMu         sync.Mutex
	stdinCloseOnce sync.Once
}

// StartCommand starts a child with owned stdin/stdout pipes. On Windows the
// child remains suspended until procgroup installs its Job Object authority;
// Unix uses a dedicated process group.
func StartCommand(ctx context.Context, spec Command) (*CommandChild, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Path == "" {
		return nil, fmt.Errorf("supervisor: command path is empty")
	}

	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("supervisor: child stdout pipe: %w", err)
	}
	var stdin io.WriteCloser
	process, err := procgroup.Spawn(procgroup.Options{
		Command:        spec.Path,
		Args:           append([]string(nil), spec.Args...),
		Dir:            spec.Dir,
		Env:            cloneStrings(spec.Env),
		Stdout:         stdoutWriter,
		Stderr:         spec.Stderr,
		StartSuspended: true,
		PreStart: func(cmd *exec.Cmd) error {
			pipe, pipeErr := cmd.StdinPipe()
			if pipeErr != nil {
				return pipeErr
			}
			stdin = pipe
			return nil
		},
	})
	_ = stdoutWriter.Close()
	if err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		_ = stdout.Close()
		return nil, commandStartError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, rollbackStartedCommand(process, stdin, stdout, err)
	}
	if stdin == nil {
		return nil, rollbackStartedCommand(process, nil, stdout, errors.New("supervisor: child stdin pipe was not created"))
	}

	child := &CommandChild{
		process:  process,
		stdin:    stdin,
		stdout:   stdout,
		pid:      process.PID(),
		wait:     make(chan Exit, 1),
		exitDone: make(chan struct{}),
	}
	go child.reap()
	return child, nil
}

func commandStartError(err error) error {
	startErr := fmt.Errorf("supervisor: start command: %w", err)
	if errors.Is(err, procgroup.ErrTreeRetirementUnproven) {
		return errors.Join(ErrStartRollbackUnproven, startErr)
	}
	return startErr
}

func (child *CommandChild) Stdin() io.WriteCloser { return child.stdin }
func (child *CommandChild) Stdout() io.ReadCloser { return child.stdout }
func (child *CommandChild) Wait() <-chan Exit     { return child.wait }
func (child *CommandChild) PID() int              { return child.pid }

func (child *CommandChild) reap() {
	<-child.process.Done()
	waitErr := child.process.Wait()
	var exitError *exec.ExitError
	switch {
	case errors.Is(waitErr, procgroup.ErrTreeRetirementUnproven):
		waitErr = errors.Join(ErrProcessTreeUnproven, waitErr)
	case waitErr != nil && errors.As(waitErr, &exitError):
		waitErr = nil
	case waitErr != nil:
		waitErr = errors.Join(ErrProcessTreeUnproven, waitErr)
	}
	exit := Exit{Code: child.process.ExitCode(), Err: waitErr}
	child.exitMu.Lock()
	child.exit = exit
	child.exitMu.Unlock()
	close(child.exitDone)
	child.wait <- exit
	close(child.wait)
}

// Stop closes stdin, allows a bounded voluntary exit, then asks procgroup to
// retire and prove the complete process tree before returning nil.
func (child *CommandChild) Stop(ctx context.Context) error {
	child.stopMu.Lock()
	defer child.stopMu.Unlock()
	if exit, exited := child.exitResult(); exited {
		return commandExitProofError(exit)
	}
	child.stdinCloseOnce.Do(func() { _ = child.stdin.Close() })

	graceful := defaultCommandGracefulTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			graceful = 0
		} else if graceful > remaining/2 {
			graceful = remaining / 2
		}
	}
	if graceful > 0 {
		timer := time.NewTimer(graceful)
		select {
		case <-child.exitDone:
			timer.Stop()
			exit, _ := child.exitResult()
			return commandExitProofError(exit)
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}

	killDone := make(chan error, 1)
	go func() { killDone <- child.process.Kill() }()
	var killErr error
	select {
	case killErr = <-killDone:
	case <-ctx.Done():
		return fmt.Errorf("supervisor: command tree retirement: %w", ctx.Err())
	}
	select {
	case <-child.exitDone:
	case <-ctx.Done():
		return fmt.Errorf("supervisor: command terminal proof: %w", ctx.Err())
	}
	if killErr != nil {
		return killErr
	}
	exit, _ := child.exitResult()
	return commandExitProofError(exit)
}

func commandExitProofError(exit Exit) error {
	if errors.Is(exit.Err, ErrProcessTreeUnproven) {
		return exit.Err
	}
	return nil
}

func (child *CommandChild) exitResult() (Exit, bool) {
	select {
	case <-child.exitDone:
		child.exitMu.Lock()
		exit := child.exit
		child.exitMu.Unlock()
		return exit, true
	default:
		return Exit{}, false
	}
}

func rollbackStartedCommand(process *procgroup.Process, stdin io.WriteCloser, stdout io.ReadCloser, cause error) error {
	killErr := process.Kill()
	if stdin != nil {
		_ = stdin.Close()
	}
	_ = stdout.Close()
	if killErr != nil {
		return errors.Join(cause, ErrStartRollbackUnproven, fmt.Errorf("supervisor: partial command cleanup: %w", killErr))
	}
	return cause
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}
