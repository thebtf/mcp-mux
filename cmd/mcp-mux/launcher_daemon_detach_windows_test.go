//go:build windows

package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const detachedDaemonParentHelper = "MCPMUX_DETACHED_DAEMON_PARENT_HELPER"

func TestStartDaemonProcessFromSurvivesParentExit(t *testing.T) {
	fixture := buildDetachedDaemonFixture(t)
	for _, testCase := range []struct {
		name       string
		enginePath func(*testing.T) string
	}{
		{name: "active engine", enginePath: func(*testing.T) string { return fixture }},
		{name: "stable launcher fallback", enginePath: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "missing-engine.exe")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			readyFile := filepath.Join(t.TempDir(), "ready.txt")
			token := strconv.FormatInt(time.Now().UnixNano(), 36)
			parent := exec.Command(os.Args[0], "-test.run=^TestStartDaemonProcessFromParentHelper$")
			parent.Env = append(os.Environ(),
				detachedDaemonParentHelper+"=1",
				"MCPMUX_DETACHED_DAEMON_FIXTURE="+fixture,
				"MCPMUX_DETACHED_DAEMON_ENGINE="+testCase.enginePath(t),
				"MCPMUX_DETACHED_DAEMON_READY_FILE="+readyFile,
				"MCPMUX_DETACHED_DAEMON_TOKEN="+token,
			)
			if output, err := parent.CombinedOutput(); err != nil {
				t.Fatalf("short-lived launcher parent failed: %v\n%s", err, output)
			}

			pid, address := waitDetachedDaemonReady(t, readyFile)
			handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
			if err != nil {
				t.Fatalf("open detached daemon pid %d: %v", pid, err)
			}
			stopped := false
			t.Cleanup(func() {
				if !stopped {
					_, _ = detachedDaemonCommand(address, token, "shutdown")
					_ = windows.TerminateProcess(handle, 1)
					_, _ = windows.WaitForSingleObject(handle, 5_000)
				}
				_ = windows.CloseHandle(handle)
			})

			if response, err := detachedDaemonCommand(address, token, "status"); err != nil || response != "ok" {
				t.Fatalf("detached daemon status after parent exit = %q, %v", response, err)
			}
			if response, err := detachedDaemonCommand(address, token, "shutdown"); err != nil || response != "bye" {
				t.Fatalf("detached daemon shutdown = %q, %v", response, err)
			}
			status, err := windows.WaitForSingleObject(handle, 5_000)
			if err != nil {
				t.Fatal(err)
			}
			if status != uint32(windows.WAIT_OBJECT_0) {
				t.Fatalf("detached daemon did not exit after shutdown: wait status %d", status)
			}
			stopped = true
		})
	}
}

func TestStartDaemonProcessFromParentHelper(t *testing.T) {
	if os.Getenv(detachedDaemonParentHelper) != "1" {
		return
	}
	launcher := os.Getenv("MCPMUX_DETACHED_DAEMON_FIXTURE")
	engine := os.Getenv("MCPMUX_DETACHED_DAEMON_ENGINE")
	if err := startDaemonProcessFrom(launcher, engine); err != nil {
		t.Fatal(err)
	}
}

func buildDetachedDaemonFixture(t *testing.T) string {
	t.Helper()
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "detached-daemon.exe")
	command := exec.Command("go", "build", "-o", output, "./cmd/mcp-mux/testdata/detached-daemon")
	command.Dir = moduleRoot
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build detached daemon fixture: %v\n%s", err, result)
	}
	return output
}

func waitDetachedDaemonReady(t *testing.T, path string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Fields(string(data))
			if len(lines) == 2 {
				pid, convErr := strconv.Atoi(lines[0])
				if convErr == nil && pid > 0 {
					return pid, lines[1]
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("detached daemon did not publish readiness: %s", path)
	return 0, ""
}

func detachedDaemonCommand(address, token, command string) (string, error) {
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := fmt.Fprintf(conn, "%s %s\n", token, command); err != nil {
		return "", err
	}
	response, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(response), err
}
