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
