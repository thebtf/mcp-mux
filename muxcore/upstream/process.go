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
// and any error from the kill escalation. A non-nil error signals that a forced
// kill occurred (either GracefulKill failed, kill succeeded after timeout, or the
// handler did not exit within timeout).
// Safe to call after Close() — returns (0, nil) if already closed or authority was transferred.
func (p *Process) SoftClose(timeout time.Duration) (int, error) {
	p.closeMu.Lock()
	defer p.closeMu.Unlock()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, nil
	}
	switch p.detach {
	case detachLegacy, detachCommitted:
		p.mu.Unlock()
		return 0, nil
	case detachPrepared, detachAborted:
		p.mu.Unlock()
		return 0, p.AbortDetach()
	case detachAttached:
		p.retiring = true
	}
	p.mu.Unlock()

	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	select {
	case <-p.Done:
		if err := p.finalizeOwnedTree(); err != nil {
			return softCloseExitCode(p.ExitErr), err
		}
		p.markClosed()
		return softCloseExitCode(p.ExitErr), nil
	case <-time.After(timeout):
	}

	if p.proc != nil || p.pid > 0 {
		if err := p.finalizeOwnedTree(); err != nil {
			return -1, err
		}
		select {
		case <-p.Done:
			p.markClosed()
		case <-time.After(5 * time.Second):
			return -1, fmt.Errorf("upstream: process tree did not exit after termination")
		}
		return softCloseExitCode(p.ExitErr), fmt.Errorf("upstream: forced kill after soft-close timeout")
	}
	return -1, fmt.Errorf("upstream: handler did not exit after %v", timeout)
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

// ErrDetachNotPrepared indicates CommitDetach or AbortDetach was called before
// a successful detach preparation.
var ErrDetachNotPrepared = errors.New("upstream: detach not prepared")

// ErrDetachCommitted indicates an abort arrived after ownership was committed
// to a successor.
var ErrDetachCommitted = errors.New("upstream: detach already committed")

type detachState uint8

const (
	detachAttached detachState = iota
	detachLegacy
	detachPrepared
	detachCommitted
	detachAborted
)

const processTreeRetirementTimeout = 10 * time.Second

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
	pid        int
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	stdinFile  *os.File // raw OS file for Detach; nil for handler-based processes
	stdoutFile *os.File // raw OS file for Detach; nil for handler-based processes
	stderrFile *os.File // raw OS file for transactional handoff
	// stdinFD / stdoutFD / stderrFD cache the OS handles captured once through
	// SyscallConn during PreStart, before reader/writer goroutines start. Calling
	// (*os.File).Fd() from Detach at runtime would switch the File to blocking
	// mode and race with active stream I/O.
	stdinFD  uintptr
	stdoutFD uintptr
	stderrFD uintptr
	// stdoutDrain/stderrDrain own the blocking read authorities. Transactional
	// detach quiesces and joins both before exposing their OS handles.
	stdoutDrain *streamReadControl
	stderrDrain *streamReadControl
	// authorityMu protects the transferable PGID/Job authority. Finalization
	// retains it on failure so a later owner-removal attempt can retry.
	authorityMu sync.Mutex
	spawnPgid   int
	jobHandle   uintptr

	lineBuf *lineBuffer

	mu            sync.Mutex
	closeMu       sync.Mutex
	finalizeMu    sync.Mutex
	closed        bool
	retiring      bool
	treeFinalized bool
	detach        detachState
	drainTimeout  time.Duration // from x-mux.drainTimeout; overrides default 5s stdin-close wait

	// Done is closed when the process exits.
	Done chan struct{}
	// ExitErr holds the error from Wait(), available after Done is closed.
	ExitErr error
}

// beginExitFinalization atomically decides whether this Process still owns
// leader-exit cleanup. A detach that reaches detachPrepared first owns the
// tree lease; an exit finalizer that reaches this method first marks the
// Process retiring so writes and later detach attempts are rejected while
// finalization remains retryable until the full tree is proven retired.
func (p *Process) beginExitFinalization() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.detach != detachAttached {
		return false
	}
	p.retiring = true
	return true
}

func (p *Process) markClosed() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
}

func (p *Process) finalizeOwnedTree() error {
	p.finalizeMu.Lock()
	defer p.finalizeMu.Unlock()
	if p.treeFinalized {
		return nil
	}
	if err := terminateProcessTree(p); err != nil {
		return err
	}
	p.treeFinalized = true
	return nil
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
// startPostAuthorityHook is a narrow regression-test seam. Production leaves
// it as a no-op; tests use it to force a failure after process-tree authority
// exists and verify Start does not return before the full tree is retired.
var startPostAuthorityHook = func(*Process) error { return nil }

var afterSpawnPlatform = afterSpawnWindows

var finalizeFailedStartTree = func(p *Process) error { return p.finalizeOwnedTree() }

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
	// Stderr also uses an owned os.Pipe. procgroup starts cmd.Wait in its own
	// reaper goroutine immediately after spawn, so exec.Cmd.StderrPipe would be
	// invalid here: Cmd.Wait closes that pipe and can race the scanner, silently
	// dropping the last crash diagnostics. Owned pipe ends remain valid until
	// the drain goroutines close them explicitly.
	stdoutR, stdoutW, pipeErr := os.Pipe()
	if pipeErr != nil {
		return nil, fmt.Errorf("upstream: os.Pipe: %w", pipeErr)
	}
	stderrR, stderrW, pipeErr := os.Pipe()
	if pipeErr != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("upstream: stderr os.Pipe: %w", pipeErr)
	}

	// Capture pipe handles inside the PreStart callback so procgroup still owns
	// the exec.Cmd lifecycle (platform isolation, job object, tree kill).
	var captureErr error
	opts := procgroup.Options{
		Command: command,
		Args:    args,
		Dir:     cwd,
		Env:     merged,
		// stdout/stderr: use owned pipes (see rationale above). stdin still
		// uses exec.Cmd's built-in pipe because the caller drives writes.
		Stdout:      stdoutW,
		Stderr:      stderrW,
		DisableTree: true,
		PreStart: func(cmd *exec.Cmd) error {
			stdin, err := cmd.StdinPipe()
			if err != nil {
				captureErr = fmt.Errorf("upstream: stdin pipe: %w", err)
				return captureErr
			}
			p.stdin = stdin
			p.stdout = stdoutR
			p.stderr = stderrR
			// Capture raw *os.File references for Detach(). stdoutR is always
			// *os.File (from os.Pipe above). stdin from StdinPipe is also *os.File
			// on all supported platforms; the type assertion is defensive.
			if f, ok := stdin.(*os.File); ok {
				p.stdinFile = f
				p.stdinFD, err = rawFileHandle(f)
				if err != nil {
					captureErr = fmt.Errorf("upstream: stdin raw handle: %w", err)
					return captureErr
				}
			}
			p.stdoutFile = stdoutR
			p.stdoutFD, err = rawFileHandle(stdoutR)
			if err != nil {
				captureErr = fmt.Errorf("upstream: stdout raw handle: %w", err)
				return captureErr
			}
			p.stderrFile = stderrR
			p.stderrFD, err = rawFileHandle(stderrR)
			if err != nil {
				captureErr = fmt.Errorf("upstream: stderr raw handle: %w", err)
				return captureErr
			}
			cmd.SysProcAttr = applyUnixSpawnAttrs(cmd.SysProcAttr)
			return nil
		},
	}

	proc, err := procgroup.Spawn(opts)
	if err != nil {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, fmt.Errorf("upstream: spawn %s: %w", command, err)
	}

	p.proc = proc
	p.pid = proc.PID()
	// On Unix, Setpgid=true guarantees PGID == PID after spawn. We read
	// proc.PID() to stay within the procgroup abstraction. The Windows build
	// ignores spawnPgid and installs Job authority in afterSpawnWindows.
	if pid := proc.PID(); pid != 0 {
		p.authorityMu.Lock()
		p.spawnPgid = pid
		p.authorityMu.Unlock()
	}

	failAfterSpawn := func(startErr error) (*Process, error) {
		startErr = errors.Join(startErr, finalizeFailedStart(p, proc, stdoutR, stdoutW, stderrR, stderrW))
		if !p.RetirementProven() {
			return p, startErr
		}
		return nil, startErr
	}

	if captureErr != nil {
		// Should not happen — Spawn already propagates PreStart errors — but if
		// it does, retire the process before returning the setup error.
		return failAfterSpawn(captureErr)
	}

	// Upstream owns the single transferable tree authority. Procgroup tree
	// management is disabled for this spawn to avoid a second Job/PGID owner.
	if err := afterSpawnPlatform(p, proc.PID()); err != nil {
		return failAfterSpawn(fmt.Errorf("upstream: tree authority: %w", err))
	}
	if err := startPostAuthorityHook(p); err != nil {
		return failAfterSpawn(err)
	}

	// Close our copy of the write end — the child inherited its own copy via
	// exec. When the child exits, its write end closes; our drain goroutine's
	// Scanner then sees EOF naturally. Without this close, the pipe never
	// reaches EOF because one writer (us) remains open forever.
	_ = stdoutW.Close()
	_ = stderrW.Close()

	p.lineBuf = newLineBuffer()

	// Both readers publish cancellable read authority. Transactional handoff
	// joins them before duplicating the OS handles, so successor adoption never
	// races an outstanding predecessor ReadFile/read syscall.
	stdoutDrained := p.startStdoutDrain(stdoutR)
	stderrDrained := p.startStderrDrain(stderrR, logger, proc.PID())

	// Observe leader exit independently of output drains. A descendant may
	// inherit stdout/stderr, so waiting for EOF before tree finalization would
	// deadlock and leak that descendant.
	go func() {
		<-proc.Done()
		attached := p.beginExitFinalization()
		var finalizeErr error
		if attached {
			if p.stdin != nil {
				_ = p.stdin.Close()
			}
			finalizeErr = p.finalizeOwnedTree()
		}
		<-stdoutDrained
		<-stderrDrained
		p.ExitErr = errors.Join(proc.Wait(), finalizeErr)
		if finalizeErr == nil {
			p.markClosed()
		}
		close(p.Done)
	}()

	return p, nil
}

func observeFailedStartLeader(p *Process, proc *procgroup.Process) {
	if proc == nil {
		close(p.Done)
		return
	}
	go func() {
		p.ExitErr = proc.Wait()
		close(p.Done)
	}()
}

func finalizeFailedStart(p *Process, proc *procgroup.Process, pipes ...*os.File) error {
	p.mu.Lock()
	p.retiring = true
	p.mu.Unlock()
	observeFailedStartLeader(p, proc)

	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	finalizeErr := finalizeFailedStartTree(p)
	if finalizeErr != nil && proc != nil {
		finalizeErr = errors.Join(finalizeErr, proc.Kill())
	}
	if finalizeErr == nil {
		<-p.Done
		p.markClosed()
	}
	for _, pipe := range pipes {
		if pipe != nil {
			_ = pipe.Close()
		}
	}
	return finalizeErr
}

// WriteLine sends a line of data to the upstream process stdin, followed by newline.
func (p *Process) WriteLine(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.retiring {
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
	p.closeMu.Lock()
	defer p.closeMu.Unlock()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	switch p.detach {
	case detachLegacy, detachCommitted:
		p.mu.Unlock()
		return nil
	case detachPrepared, detachAborted:
		p.mu.Unlock()
		return p.AbortDetach()
	case detachAttached:
		p.retiring = true
	}
	stdinWait := p.drainTimeout
	p.mu.Unlock()

	if stdinWait <= 0 {
		stdinWait = 5 * time.Second
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	select {
	case <-p.Done:
		if err := p.finalizeOwnedTree(); err != nil {
			return err
		}
		p.markClosed()
		return nil
	case <-time.After(stdinWait):
	}
	if err := p.finalizeOwnedTree(); err != nil {
		return err
	}
	if p.proc == nil && p.pid <= 0 {
		p.markClosed()
		return nil
	}
	select {
	case <-p.Done:
		p.markClosed()
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("upstream: process tree finalization not yet proven")
	}
}

// PID returns the process ID of the upstream process.
func (p *Process) PID() int {
	return p.pid
}

// RetirementProven reports whether this Process no longer owns a live process
// tree authority. Attached OS processes require both Process.Done and retired
// process-group/Job authority. A committed detach is also terminal for this
// owner because authority has transferred to the successor.
func (p *Process) RetirementProven() bool {
	p.mu.Lock()
	detach := p.detach
	closed := p.closed
	hasOSProcess := p.proc != nil || p.pid > 0
	p.mu.Unlock()

	if detach == detachCommitted {
		return true
	}
	if !hasOSProcess {
		return closed
	}

	p.finalizeMu.Lock()
	finalized := p.treeFinalized
	p.finalizeMu.Unlock()
	if !finalized {
		return false
	}
	select {
	case <-p.Done:
		return true
	default:
		return false
	}
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

	if p.closed || p.retiring {
		return 0, 0, 0, ErrAlreadyClosed
	}
	if p.detach != detachAttached {
		return 0, 0, 0, ErrAlreadyDetached
	}
	if p.pid <= 0 {
		return 0, 0, 0, ErrDetachUnsupported
	}
	pid = p.pid
	if err := prepareLegacyDetach(p, pid); err != nil {
		return 0, 0, 0, err
	}
	p.detach = detachLegacy
	if p.stdinFile != nil {
		stdinFD = p.stdinFD
	}
	if p.stdoutFile != nil {
		stdoutFD = p.stdoutFD
	}
	return
}

// DetachWithAuthority prepares a handoff while retaining the predecessor's
// authority until the successor receives a duplicated handle.
func (p *Process) DetachWithAuthority() (pid int, stdinFD, stdoutFD, stderrFD, authorityFD uintptr, err error) {
	p.mu.Lock()
	if p.closed || p.retiring {
		p.mu.Unlock()
		return 0, 0, 0, 0, 0, ErrAlreadyClosed
	}
	if p.detach != detachAttached {
		p.mu.Unlock()
		return 0, 0, 0, 0, 0, ErrAlreadyDetached
	}
	if p.pid <= 0 {
		p.mu.Unlock()
		return 0, 0, 0, 0, 0, ErrDetachUnsupported
	}
	pid = p.pid
	authorityFD, err = handoffAuthority(p)
	if err != nil {
		p.mu.Unlock()
		return 0, 0, 0, 0, 0, err
	}
	// Preparation is intentionally one-way: if stream quiescence fails, this
	// lease is aborted and the tree is terminated so no half-detached process
	// can survive without a reader or owner.
	p.detach = detachPrepared
	p.mu.Unlock()

	if err = p.quiesceStreamsForHandoff(); err != nil {
		abortErr := p.AbortDetach()
		return 0, 0, 0, 0, 0, errors.Join(fmt.Errorf("upstream: quiesce handoff streams: %w", err), abortErr)
	}

	if p.stdinFile != nil {
		stdinFD = p.stdinFD
	}
	if p.stdoutFile != nil {
		stdoutFD = p.stdoutFD
	}
	if p.stderrFile != nil {
		stderrFD = p.stderrFD
	}
	return
}

// CommitDetach commits a prepared transfer after the successor has adopted
// stdio and tree authority. It is idempotent.
func (p *Process) CommitDetach() error {
	p.mu.Lock()
	switch p.detach {
	case detachCommitted:
		p.mu.Unlock()
		return nil
	case detachPrepared:
		p.detach = detachCommitted
	case detachAborted, detachAttached, detachLegacy:
		p.mu.Unlock()
		return ErrDetachNotPrepared
	}
	p.mu.Unlock()

	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.stdout != nil {
		_ = p.stdout.Close()
	}
	if p.stderr != nil {
		_ = p.stderr.Close()
	}
	return releaseTreeAuthority(p)
}

// AbortDetach terminates a tree whose prepared transfer did not commit. It is
// idempotent and leaves no authority for a later finalizer to reuse.
func (p *Process) AbortDetach() error {
	p.mu.Lock()
	switch p.detach {
	case detachCommitted:
		p.mu.Unlock()
		return ErrDetachCommitted
	case detachAttached, detachLegacy:
		p.mu.Unlock()
		return ErrDetachNotPrepared
	case detachPrepared:
		p.detach = detachAborted
		p.retiring = true
	case detachAborted:
		p.retiring = true
	}
	p.mu.Unlock()

	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	err := p.finalizeOwnedTree()
	if err == nil {
		p.markClosed()
	}
	if p.stdout != nil {
		_ = p.stdout.Close()
	}
	if p.stderr != nil {
		_ = p.stderr.Close()
	}
	if p.proc != nil {
		select {
		case <-p.Done:
		case <-time.After(5 * time.Second):
			return errors.Join(err, errors.New("upstream: aborted detach process did not exit"))
		}
	}
	return err
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
