//go:build !windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const signalShutdownAuthorityRole = "MCPMUX_SIGNAL_SHUTDOWN_AUTHORITY_ROLE"

func TestRunGlobalDaemonSignalShutdownAuthority(t *testing.T) {
	for _, scenario := range []string{"held", "unfenced"} {
		t.Run(scenario, func(t *testing.T) {
			scratch := os.Getenv("GOTMPDIR")
			canonical := strings.ToLower(filepath.ToSlash(filepath.Clean(scratch)))
			if !filepath.IsAbs(scratch) || !strings.Contains(canonical, "/.agent/") || strings.Contains(canonical, "/.agent/worktrees/") {
				t.Skip("requires parent-supplied GOTMPDIR beneath primary .agent")
			}
			root, err := os.MkdirTemp(scratch, "sc-")
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "config")
			if err := os.Mkdir(config, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Logf("actual signal caller fixture: %s", root)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunGlobalDaemonSignalShutdownAuthorityHelper$", "--", "daemon")
			command.Env = append(os.Environ(), signalShutdownAuthorityRole+"=daemon",
				"MCPMUX_SIGNAL_SHUTDOWN_AUTHORITY_CASE="+scenario,
				"MCPMUX_SIGNAL_SHUTDOWN_AUTHORITY_ROOT="+root,
				"MCPMUX_ENGINE=1", "MCPMUX_DISABLE_LAUNCHER=1", "MCP_MUX_TEST_MAIN=0",
				"MCP_MUX_IDLE_TIMEOUT=1m", "MCP_MUX_OWNER_IDLE=1m",
				"APPDATA="+config, "XDG_CONFIG_HOME="+config, "HOME="+config, "USERPROFILE="+config,
				"TMP="+root, "TEMP="+root, "TMPDIR="+root)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("actual main signal caller %s: %v\n%s", scenario, err, output)
			}
			t.Logf("%s", output)
		})
	}
}

func TestRunGlobalDaemonSignalShutdownAuthorityHelper(t *testing.T) {
	switch os.Getenv(signalShutdownAuthorityRole) {
	case "upstream":
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
				os.Exit(2)
			}
			if request.ID == nil {
				continue
			}
			result := map[string]any{}
			switch request.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "signal-authority-fixture", "version": "1"}}
			case "tools/list":
				result["tools"] = []any{}
			}
			response, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
			if err != nil {
				os.Exit(3)
			}
			fmt.Fprintln(os.Stdout, string(response))
		}
		os.Exit(0)
	case "daemon":
	default:
		return
	}

	root := os.Getenv("MCPMUX_SIGNAL_SHUTDOWN_AUTHORITY_ROOT")
	scenario := os.Getenv("MCPMUX_SIGNAL_SHUTDOWN_AUTHORITY_CASE")
	executable := os.Args[0]
	os.Args = modernAdmissionMainArgs(t)
	// The ignored-to-notified transition proves production signal registration
	// before delivery, without adding a production seam or racing startup.
	signal.Ignore(syscall.SIGTERM)
	returned := make(chan struct{})
	go func() {
		main()
		close(returned)
	}()
	path := serverid.DaemonControlPath("", engineName)
	if err := waitForDaemon(path, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for signal.Ignored(syscall.SIGTERM) {
		if time.Now().After(deadline) {
			t.Fatal("actual daemon caller did not register SIGTERM")
		}
		time.Sleep(time.Millisecond)
	}
	var hold control.MaintenanceResult
	if scenario == "held" {
		spawn, err := control.SendWithTimeout(path, control.Request{
			Cmd: "spawn", Command: executable, Args: []string{"-test.run=^TestRunGlobalDaemonSignalShutdownAuthorityHelper$"},
			Mode: "global", Cwd: root, Env: map[string]string{signalShutdownAuthorityRole: "upstream"},
		}, 5*time.Second)
		if err != nil || !spawn.OK {
			t.Fatalf("spawn real owned fixture: %+v %v", spawn, err)
		}
		response, err := control.SendWithTimeout(path, control.Request{Cmd: "hold", ServerID: spawn.ServerID}, 5*time.Second)
		if err != nil || !response.OK || response.Maintenance == nil {
			t.Fatalf("hold real fixture: %+v %v", response, err)
		}
		hold = *response.Maintenance
		if hold.State != control.MaintenanceHeld || !hold.TreesRetired {
			t.Fatalf("fixture trees did not retire before HELD: %+v", hold)
		}
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if scenario == "held" {
		logPath := filepath.Join(root, "mcp-muxd-debug.log")
		deadline = time.Now().Add(5 * time.Second)
		for {
			data, err := os.ReadFile(logPath)
			if err == nil && strings.Contains(string(data), "daemon shutdown refused:") {
				break
			}
			select {
			case <-returned:
				t.Fatal("signal caller returned through main with held authority")
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("actual signal did not reach the refused Shutdown caller")
			}
			time.Sleep(time.Millisecond)
		}
		select {
		case <-returned:
			t.Fatal("signal caller returned through main after refused shutdown")
		case <-time.After(100 * time.Millisecond):
		}
		for _, cmd := range []string{"ping", "status"} {
			response, err := control.SendWithTimeout(path, control.Request{Cmd: cmd}, time.Second)
			if err != nil || !response.OK {
				t.Fatalf("signal refusal stopped real control serving (%s): %+v %v", cmd, response, err)
			}
		}
		response, err := control.SendWithTimeout(path, control.Request{Cmd: "shutdown"}, time.Second)
		if err != nil || response.OK || response.ErrorCode != control.ErrMaintenanceHeld.Code {
			t.Fatalf("signal refusal lost terminal lifecycle admission: %+v %v", response, err)
		}
		response, err = control.SendWithTimeout(path, control.Request{Cmd: "resume", HoldID: hold.HoldID}, time.Second)
		if err != nil || !response.OK {
			t.Fatalf("safe exact-lease release: %+v %v", response, err)
		}
		response, err = control.SendWithTimeout(path, control.Request{Cmd: "shutdown"}, time.Second)
		if err != nil || !response.OK {
			t.Fatalf("explicit admitted shutdown: %+v %v", response, err)
		}
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("actual main signal caller did not finish legitimate daemon shutdown")
	}
	fmt.Fprintln(os.Stderr, "verified actual SIGTERM caller:", scenario)
}
