package upstream

import (
	"runtime"
	"strings"
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

	p, err := Start(cmd, args, nil)
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
	p, err := Start("go", []string{"run", "../../testdata/echo_pipe.go"}, nil)
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

	p, err := Start(cmd, args, nil)
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

	p, err := Start(cmd, args, nil)
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

	p, err := Start(cmd, args, nil)
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

	p, err := Start(cmd, args, nil)
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

	p, err := Start(cmd, args, env)
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
