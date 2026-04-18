package upstream

import (
	"bytes"
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

	time.Sleep(50 * time.Millisecond)

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
