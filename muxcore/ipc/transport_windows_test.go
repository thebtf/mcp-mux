//go:build windows

package ipc

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func TestPipeConfigUsesCurrentUserSecurityDescriptor(t *testing.T) {
	cfg, err := pipeConfig()
	if err != nil {
		t.Fatalf("pipeConfig() error: %v", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() error: %v", err)
	}
	if cfg.SecurityDescriptor == "" {
		t.Fatal("SecurityDescriptor is empty")
	}
	if !strings.Contains(cfg.SecurityDescriptor, user.User.Sid.String()) {
		t.Fatalf("SecurityDescriptor = %q, want current user SID %s", cfg.SecurityDescriptor, user.User.Sid.String())
	}
	if cfg.InputBufferSize != pipeBufferSize {
		t.Fatalf("InputBufferSize = %d, want %d", cfg.InputBufferSize, pipeBufferSize)
	}
	if cfg.OutputBufferSize != pipeBufferSize {
		t.Fatalf("OutputBufferSize = %d, want %d", cfg.OutputBufferSize, pipeBufferSize)
	}
	if cfg.MessageMode {
		t.Fatal("MessageMode = true, want byte-stream mode")
	}
}

func TestDialMayStartBeforeAccept(t *testing.T) {
	path := socketPath(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer ln.Close()

	dialed := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, err := DialTimeout(path, 5*time.Second)
		dialed <- struct {
			conn net.Conn
			err  error
		}{conn: conn, err: err}
	}()

	time.Sleep(50 * time.Millisecond)

	accepted := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- struct {
			conn net.Conn
			err  error
		}{conn: conn, err: err}
	}()

	d := <-dialed
	if d.err != nil {
		t.Fatalf("DialTimeout() error: %v", d.err)
	}
	defer d.conn.Close()

	a := <-accepted
	if a.err != nil {
		t.Fatalf("Accept() error: %v", a.err)
	}
	defer a.conn.Close()
}

func TestDetachedProcessListenerAcceptsParentDial(t *testing.T) {
	path := socketPath(t)
	readyPath := path + ".ready"
	resultPath := path + ".result"

	cmd := exec.Command(os.Args[0], "-test.run=TestDetachedProcessListenerHelper")
	cmd.Env = append(os.Environ(),
		"MUXCORE_IPC_TEST_HELPER=1",
		"MUXCORE_IPC_TEST_PATH="+path,
		"MUXCORE_IPC_TEST_READY="+readyPath,
		"MUXCORE_IPC_TEST_RESULT="+resultPath,
	)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	if err := cmd.Process.Release(); err != nil {
		t.Fatalf("release helper: %v", err)
	}

	waitForFile(t, readyPath, 5*time.Second)

	conn, err := DialTimeout(path, 5*time.Second)
	if err != nil {
		t.Fatalf("DialTimeout() error: %v", err)
	}
	conn.Close()

	waitForFile(t, resultPath, 5*time.Second)
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if got := strings.TrimSpace(string(result)); got != "ok" {
		t.Fatalf("helper result = %q, want ok", got)
	}
}

func TestPipeListenerCloseTimesOutWhenUnderlyingCloseBlocks(t *testing.T) {
	oldTimeout := pipeListenerCloseTimeout
	pipeListenerCloseTimeout = 25 * time.Millisecond
	defer func() { pipeListenerCloseTimeout = oldTimeout }()

	inner := &blockingCloseListener{
		closeStarted: make(chan struct{}),
		unblockClose: make(chan struct{}),
	}
	defer close(inner.unblockClose)

	ln := &pipeListener{
		Listener: inner,
		path:     socketPath(t),
	}

	start := time.Now()
	err := ln.Close()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Close() error = nil, want timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Close() error = %v, want timeout", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Close() took %s, want bounded timeout", elapsed)
	}

	select {
	case <-inner.closeStarted:
	default:
		t.Fatal("underlying listener Close was not called")
	}
}

func TestListenRetriesTransientPipeAccessDenied(t *testing.T) {
	oldListen := listenNamedPipe
	oldSleep := sleepBeforeListenRetry
	oldAttempts := pipeListenRetryAttempts
	oldDelay := pipeListenRetryDelay
	t.Cleanup(func() {
		listenNamedPipe = oldListen
		sleepBeforeListenRetry = oldSleep
		pipeListenRetryAttempts = oldAttempts
		pipeListenRetryDelay = oldDelay
	})

	calls := 0
	listenNamedPipe = func(string, *winio.PipeConfig) (net.Listener, error) {
		calls++
		if calls < 3 {
			return nil, &os.PathError{
				Op:   "open",
				Path: `\\.\pipe\mcp-mux-test`,
				Err:  windows.ERROR_ACCESS_DENIED,
			}
		}
		return stubListener{}, nil
	}
	sleepBeforeListenRetry = func(time.Duration) {}
	pipeListenRetryAttempts = 5
	pipeListenRetryDelay = time.Millisecond

	ln, err := Listen(socketPath(t))
	if err != nil {
		t.Fatalf("Listen() error = %v, want retry success", err)
	}
	ln.Close()

	if calls != 3 {
		t.Fatalf("listen calls = %d, want 3", calls)
	}
}

func TestDetachedProcessListenerHelper(t *testing.T) {
	if os.Getenv("MUXCORE_IPC_TEST_HELPER") != "1" {
		return
	}
	path := os.Getenv("MUXCORE_IPC_TEST_PATH")
	readyPath := os.Getenv("MUXCORE_IPC_TEST_READY")
	resultPath := os.Getenv("MUXCORE_IPC_TEST_RESULT")
	if path == "" || readyPath == "" || resultPath == "" {
		os.Exit(2)
	}

	ln, err := Listen(path)
	if err != nil {
		_ = os.WriteFile(resultPath, []byte("listen: "+err.Error()), 0600)
		os.Exit(0)
	}
	defer ln.Close()

	if err := os.WriteFile(readyPath, []byte("ready"), 0600); err != nil {
		_ = os.WriteFile(resultPath, []byte("ready: "+err.Error()), 0600)
		os.Exit(0)
	}

	conn, err := ln.Accept()
	if err != nil {
		_ = os.WriteFile(resultPath, []byte("accept: "+err.Error()), 0600)
		os.Exit(0)
	}
	conn.Close()
	_ = os.WriteFile(resultPath, []byte("ok"), 0600)
	os.Exit(0)
}

type blockingCloseListener struct {
	closeStarted chan struct{}
	unblockClose chan struct{}
}

func (l *blockingCloseListener) Accept() (net.Conn, error) {
	return nil, net.ErrClosed
}

func (l *blockingCloseListener) Close() error {
	close(l.closeStarted)
	<-l.unblockClose
	return nil
}

func (l *blockingCloseListener) Addr() net.Addr {
	return testAddr("blocking-close-listener")
}

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

type stubListener struct{}

func (stubListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (stubListener) Close() error              { return nil }
func (stubListener) Addr() net.Addr            { return testAddr("stub-listener") }

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", path)
}
