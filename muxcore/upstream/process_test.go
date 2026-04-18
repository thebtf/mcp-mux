package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartAndClose(t *testing.T) {
	// Use a simple process that exits immediately
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "echo", "hello"}
	} else {
		cmd = "echo"
		args = []string{"hello"}
	}

	p, err := Start(cmd, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Close()

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() error: %v", err)
	}
	if !strings.Contains(string(line), "hello") {
		t.Errorf("ReadLine() = %q, want contains 'hello'", string(line))
	}
}

func TestWriteAndRead(t *testing.T) {
	// Use 'go run' with a simple cat-like program
	// For cross-platform, we use Go itself
	p, err := Start("go", []string{"run", "../../testdata/echo_pipe.go"}, nil, "", nil)
	if err != nil {
		t.Skipf("Skipping: cannot start echo_pipe: %v", err)
	}
	defer p.Close()

	// Write a line
	err = p.WriteLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("WriteLine() error: %v", err)
	}

	// Read it back
	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() error: %v", err)
	}
	if !strings.Contains(string(line), "ping") {
		t.Errorf("ReadLine() = %q, want contains 'ping'", string(line))
	}
}

func TestProcessDone(t *testing.T) {
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "echo", "done"}
	} else {
		cmd = "echo"
		args = []string{"done"}
	}

	p, err := Start(cmd, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Close()

	select {
	case <-p.Done:
		// Process exited as expected
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not exit within timeout")
	}
}

func TestProcessPID(t *testing.T) {
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "echo", "pid"}
	} else {
		cmd = "echo"
		args = []string{"pid"}
	}

	p, err := Start(cmd, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Close()

	if p.PID() == 0 {
		t.Error("PID() returned 0")
	}
}

func TestCloseTerminatesProcess(t *testing.T) {
	// Start a long-running process
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "timeout", "/t", "30"}
	} else {
		cmd = "sleep"
		args = []string{"30"}
	}

	p, err := Start(cmd, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	err = p.Close()
	if err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	select {
	case <-p.Done:
		// Good — process terminated
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not terminate after Close()")
	}
}

func TestReadLineAfterClose(t *testing.T) {
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "echo", "line1"}
	} else {
		cmd = "echo"
		args = []string{"line1"}
	}

	p, err := Start(cmd, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Read the first line
	_, _ = p.ReadLine()

	// Wait for process to finish
	<-p.Done

	// Subsequent reads should return EOF or a pipe-closed error
	_, err = p.ReadLine()
	if err == nil {
		t.Error("ReadLine() after close expected error, got nil")
	}
}

// TestStderrRoutedToLogger verifies upstream stderr lines are forwarded to the
// caller-supplied logger instead of os.Stderr. This is the diagnostic that
// lets us see why upstreams (e.g. cclsp) crash with exit status 1 when the
// daemon runs detached with os.Stderr discarded — before this fix the error
// output was silently dropped and crashes looked like mysterious "upstream
// exited: exit status 1" with no further context.
func TestStderrRoutedToLogger(t *testing.T) {
	var (
		buf bytes.Buffer
		mu  sync.Mutex
	)
	logger := log.New(&syncWriter{buf: &buf, mu: &mu}, "", 0)

	// Child process writes a unique sentinel to its stderr then exits.
	var cmd string
	var args []string
	const sentinel = "upstream-stderr-sentinel-x91z"
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "echo", sentinel, "1>&2"}
	} else {
		cmd = "sh"
		args = []string{"-c", "echo " + sentinel + " 1>&2"}
	}

	p, err := Start(cmd, args, nil, "", logger)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Close()

	// Wait for the process to exit so the stderr goroutine has flushed.
	select {
	case <-p.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit within timeout")
	}
	// Allow the forwarding goroutine a brief moment to drain after Done.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		captured := buf.String()
		mu.Unlock()
		if strings.Contains(captured, sentinel) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	final := buf.String()
	mu.Unlock()
	t.Fatalf("logger did not receive sentinel %q; captured=%q", sentinel, final)
}

// syncWriter serializes writes under a mutex so concurrent stderr drains and
// test assertions never observe a torn byte slice.
type syncWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestEnvironmentPassing(t *testing.T) {
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "echo", "%TEST_MUX_VAR%"}
	} else {
		cmd = "sh"
		args = []string{"-c", "echo $TEST_MUX_VAR"}
	}

	env := map[string]string{
		"TEST_MUX_VAR": "mux_value_123",
	}

	p, err := Start(cmd, args, env, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Close()

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() error: %v", err)
	}
	if !strings.Contains(string(line), "mux_value_123") {
		t.Errorf("ReadLine() = %q, want contains 'mux_value_123'", string(line))
	}
}

// TestStart_FastExitPreservesStdout verifies all stdout lines from a
// fast-exiting upstream are readable via ReadLine even after Done is closed.
// Regression test for the Wait-vs-ReadLine race (TECHNICAL_DEBT.md).
func TestStart_FastExitPreservesStdout(t *testing.T) {
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "echo line1 && echo line2"}
	} else {
		cmd = "sh"
		args = []string{"-c", "printf 'line1\nline2\n'"}
	}

	p, err := Start(cmd, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Close()

	select {
	case <-p.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit within timeout")
	}

	line1, err := p.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() line1 error: %v (race? process already exited)", err)
	}
	if !strings.Contains(string(line1), "line1") {
		t.Errorf("line1 = %q, want contains 'line1'", string(line1))
	}

	line2, err := p.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() line2 error: %v", err)
	}
	if !strings.Contains(string(line2), "line2") {
		t.Errorf("line2 = %q, want contains 'line2'", string(line2))
	}

	_, err = p.ReadLine()
	if err != io.EOF {
		t.Errorf("ReadLine() after all lines = %v, want io.EOF", err)
	}
}

// startLongRunningProcess starts a process that runs for 30 seconds.
// Used by Detach tests to verify process survival post-detach.
func startLongRunningProcess(t *testing.T) *Process {
	t.Helper()
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "timeout", "/t", "30", "/nobreak"}
	} else {
		cmd = "sleep"
		args = []string{"30"}
	}
	p, err := Start(cmd, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	return p
}

// TestDetach_HappyPath verifies that Detach returns a valid PID and file
// descriptors without signaling the process, and that a subsequent Close()
// is a no-op.
func TestDetach_HappyPath(t *testing.T) {
	p := startLongRunningProcess(t)
	defer p.proc.Kill() // cleanup the detached process

	pid, stdinFD, stdoutFD, err := p.Detach()
	if err != nil {
		t.Fatalf("Detach() error: %v", err)
	}

	// PID must be valid (> 0).
	if pid <= 0 {
		t.Errorf("Detach() pid = %d, want > 0", pid)
	}

	// stdoutFD is always available (os.Pipe returns *os.File).
	if stdoutFD == 0 {
		t.Errorf("Detach() stdoutFD = %d, want > 0", stdoutFD)
	}

	// stdinFD: available when cmd.StdinPipe returns *os.File (Unix).
	// On Windows, StdinPipe may not be *os.File; skip the check there.
	if runtime.GOOS != "windows" && stdinFD == 0 {
		t.Errorf("Detach() stdinFD = %d, want > 0 on non-Windows", stdinFD)
	}

	// Process must still be alive after Detach (no signal sent).
	select {
	case <-p.Done:
		t.Error("process exited after Detach — Detach must NOT kill the process")
	default:
		// Good: process is still running.
	}

	// Close() after Detach() must be a no-op: no error, no signal.
	if closeErr := p.Close(); closeErr != nil {
		t.Errorf("Close() after Detach() returned error: %v", closeErr)
	}

	// Process must still be alive even after the no-op Close().
	select {
	case <-p.Done:
		t.Error("process exited after Close() following Detach — Close must be a no-op after Detach")
	default:
		// Good.
	}
}

// TestDetach_DoubleDetach verifies that calling Detach twice returns
// ErrAlreadyDetached on the second call.
func TestDetach_DoubleDetach(t *testing.T) {
	p := startLongRunningProcess(t)
	defer p.proc.Kill()

	// First Detach must succeed.
	if _, _, _, err := p.Detach(); err != nil {
		t.Fatalf("first Detach() error: %v", err)
	}

	// Second Detach must return ErrAlreadyDetached.
	_, _, _, err := p.Detach()
	if !errors.Is(err, ErrAlreadyDetached) {
		t.Errorf("second Detach() error = %v, want ErrAlreadyDetached", err)
	}
}

// TestDetach_AfterClose verifies that calling Detach after Close returns
// ErrAlreadyClosed.
func TestDetach_AfterClose(t *testing.T) {
	p := startLongRunningProcess(t)

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// Detach after Close must return ErrAlreadyClosed.
	_, _, _, err := p.Detach()
	if !errors.Is(err, ErrAlreadyClosed) {
		t.Errorf("Detach() after Close() error = %v, want ErrAlreadyClosed", err)
	}
}

// TestDetach_HandlerBasedProcessUnsupported verifies Detach returns a stable
// error (not a panic) for handler-based processes created with
// NewProcessFromHandler.
func TestDetach_HandlerBasedProcessUnsupported(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewProcessFromHandler(ctx, func(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	})

	_, _, _, err := p.Detach()
	if !errors.Is(err, ErrDetachUnsupported) {
		t.Fatalf("Detach() on handler-based process error = %v, want ErrDetachUnsupported", err)
	}

	cancel()
	select {
	case <-p.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler-based process did not exit after context cancellation")
	}
}
