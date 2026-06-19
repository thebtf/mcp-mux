//go:build windows

package control

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDetachedDevNullControlServerAcceptsParentPing(t *testing.T) {
	socketPath := testSocketPath(t)
	readyPath := socketPath + ".ready"
	resultPath := socketPath + ".result"

	cmd := exec.Command(os.Args[0], "-test.run=TestDetachedControlServerHelper")
	cmd.Env = append(os.Environ(),
		"MUXCORE_CONTROL_TEST_HELPER=1",
		"MUXCORE_CONTROL_TEST_SOCKET="+socketPath,
		"MUXCORE_CONTROL_TEST_READY="+readyPath,
		"MUXCORE_CONTROL_TEST_RESULT="+resultPath,
	)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
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

	waitForControlFile(t, readyPath, 5*time.Second)
	resp, err := Send(socketPath, Request{Cmd: "ping"})
	if err != nil {
		t.Fatalf("Send(ping) error: %v", err)
	}
	if !resp.OK {
		t.Fatalf("Send(ping) OK=false: %+v", resp)
	}
	waitForControlFile(t, resultPath, 5*time.Second)
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "ok" {
		t.Fatalf("helper result = %q, want ok", got)
	}
}

func TestDetachedControlServerHelper(t *testing.T) {
	if os.Getenv("MUXCORE_CONTROL_TEST_HELPER") != "1" {
		return
	}
	socketPath := os.Getenv("MUXCORE_CONTROL_TEST_SOCKET")
	readyPath := os.Getenv("MUXCORE_CONTROL_TEST_READY")
	resultPath := os.Getenv("MUXCORE_CONTROL_TEST_RESULT")
	if socketPath == "" || readyPath == "" || resultPath == "" {
		os.Exit(2)
	}
	srv, err := NewServer(socketPath, &mockHandler{}, log.New(os.Stderr, "[control-helper] ", log.LstdFlags))
	if err != nil {
		_ = os.WriteFile(resultPath, []byte("server: "+err.Error()), 0600)
		os.Exit(0)
	}
	defer srv.Close()
	if err := os.WriteFile(readyPath, []byte("ready"), 0600); err != nil {
		_ = os.WriteFile(resultPath, []byte("ready: "+err.Error()), 0600)
		os.Exit(0)
	}
	time.Sleep(500 * time.Millisecond)
	_ = os.WriteFile(resultPath, []byte("ok"), 0600)
	os.Exit(0)
}

func waitForControlFile(t *testing.T, path string, timeout time.Duration) {
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
