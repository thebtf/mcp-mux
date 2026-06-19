//go:build windows

package daemon

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func TestDetachedDevNullDaemonAcceptsParentStatus(t *testing.T) {
	ctlPath := shortSocketPath(t, "detached-daemon.ctl.sock")
	readyPath := ctlPath + ".ready"
	resultPath := ctlPath + ".result"

	cmd := exec.Command(os.Args[0], "-test.run=TestDetachedDaemonControlHelper")
	cmd.Env = append(os.Environ(),
		"MUXCORE_DAEMON_TEST_HELPER=1",
		"MUXCORE_DAEMON_TEST_CONTROL="+ctlPath,
		"MUXCORE_DAEMON_TEST_READY="+readyPath,
		"MUXCORE_DAEMON_TEST_RESULT="+resultPath,
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

	waitForDaemonFile(t, readyPath, 5*time.Second)
	resp, err := control.Send(ctlPath, control.Request{Cmd: "status"})
	if err != nil {
		t.Fatalf("control.Send(status) error: %v", err)
	}
	if !resp.OK {
		t.Fatalf("control.Send(status) OK=false: %+v", resp)
	}
	waitForDaemonFile(t, resultPath, 5*time.Second)
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "ok" {
		t.Fatalf("helper result = %q, want ok", got)
	}
}

func TestDetachedDaemonControlHelper(t *testing.T) {
	if os.Getenv("MUXCORE_DAEMON_TEST_HELPER") != "1" {
		return
	}
	ctlPath := os.Getenv("MUXCORE_DAEMON_TEST_CONTROL")
	readyPath := os.Getenv("MUXCORE_DAEMON_TEST_READY")
	resultPath := os.Getenv("MUXCORE_DAEMON_TEST_RESULT")
	if ctlPath == "" || readyPath == "" || resultPath == "" {
		os.Exit(2)
	}
	d, err := New(Config{
		Name:         "detached-daemon-test",
		Namespace:    "detached-daemon-test",
		ControlPath:  ctlPath,
		IdleTimeout:  5 * time.Second,
		SkipSnapshot: true,
		Logger:       log.New(os.Stderr, "[daemon-helper] ", log.LstdFlags),
	})
	if err != nil {
		_ = os.WriteFile(resultPath, []byte("daemon: "+err.Error()), 0600)
		os.Exit(0)
	}
	defer d.Shutdown()
	if err := os.WriteFile(readyPath, []byte("ready"), 0600); err != nil {
		_ = os.WriteFile(resultPath, []byte("ready: "+err.Error()), 0600)
		os.Exit(0)
	}
	time.Sleep(500 * time.Millisecond)
	_ = os.WriteFile(resultPath, []byte("ok"), 0600)
	os.Exit(0)
}

func waitForDaemonFile(t *testing.T, path string, timeout time.Duration) {
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
