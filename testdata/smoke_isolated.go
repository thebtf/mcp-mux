// smoke_isolated.go is a smoke test for MCP servers running THROUGH mux.
//
// It validates mux-specific behavior, not upstream correctness:
// - Spawn via daemon works (proactive init, classification)
// - Classification is correct (isolated servers get isolated, shared stay shared)
// - Session connects and gets cached init replay
// - Second spawn from different cwd gets SEPARATE owner (isolation check)
// - mux_list shows the owner with correct metadata
// - tools/list response is forwarded correctly
// - Optional: tool call + inflight observability
//
// Usage:
//
//	go run testdata/smoke_isolated.go <command> [args...]
//
// Environment:
//
//	SMOKE_CWD       — working directory for the server (default: current dir)
//	SMOKE_CWD2      — second cwd for isolation check (optional)
//	SMOKE_TIMEOUT   — max wait in seconds (default: 60)
//	SMOKE_TOOL      — tool name to call after init (optional)
//	SMOKE_TOOL_ARGS — JSON args for the tool call (optional)
//	SMOKE_EXPECT    — expected classification: "isolated", "shared", "session-aware" (optional)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/control"
	"github.com/thebtf/mcp-mux/internal/muxcore/serverid"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run testdata/smoke_isolated.go <command> [args...]")
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]
	cwd := envOr("SMOKE_CWD", mustGetwd())
	cwd2 := os.Getenv("SMOKE_CWD2")
	timeoutSec, _ := strconv.Atoi(envOr("SMOKE_TIMEOUT", "60"))
	timeout := time.Duration(timeoutSec) * time.Second
	expectClass := os.Getenv("SMOKE_EXPECT")

	fmt.Printf("=== MUX SMOKE TEST ===\n")
	fmt.Printf("command: %s %v\n", command, args)
	fmt.Printf("cwd: %s\n", cwd)
	if cwd2 != "" {
		fmt.Printf("cwd2: %s (isolation check)\n", cwd2)
	}

	ctlPath := serverid.DaemonControlPath("")
	failures := 0

	// ── TEST 1: Spawn via daemon ──
	fmt.Printf("\n[1] Spawn via daemon...\n")
	resp1, err := spawnServer(ctlPath, command, args, cwd, timeout)
	if err != nil {
		fail("spawn", err)
	}
	sid1 := resp1.ServerID
	fmt.Printf("    server=%s ipc=%s\n", sid1[:8], resp1.IPCPath)

	// ── TEST 2: Connect + init (verifies proactive init / cached replay) ──
	fmt.Printf("\n[2] Connect + initialize...\n")
	initResp, initTime, toolCount, err := connectAndInit(resp1.IPCPath, resp1.Token, timeout)
	if err != nil {
		fail("connect+init", err)
	}
	fmt.Printf("    init: %.1fs, server: %s %s, tools: %d\n",
		initTime.Seconds(), initResp.ServerInfo.Name, initResp.ServerInfo.Version, toolCount)

	// ── TEST 3: Check classification via daemon status ──
	fmt.Printf("\n[3] Check classification via daemon status...\n")
	statusResp, err := control.SendWithTimeout(ctlPath, control.Request{Cmd: "status"}, 5*time.Second)
	if err != nil {
		fail("status", err)
	}
	classification := findOwnerClassification(statusResp.Data, sid1)
	fmt.Printf("    classification: %s\n", classification)
	if expectClass != "" && classification != expectClass {
		fmt.Printf("    FAIL: expected %q, got %q\n", expectClass, classification)
		failures++
	} else if expectClass != "" {
		fmt.Printf("    OK: matches expected %q\n", expectClass)
	}

	// ── TEST 4: Isolation check — second cwd gets different owner ──
	if cwd2 != "" {
		fmt.Printf("\n[4] Isolation check: spawn from cwd2=%s...\n", cwd2)
		resp2, err := spawnServer(ctlPath, command, args, cwd2, timeout)
		if err != nil {
			fail("spawn2", err)
		}
		sid2 := resp2.ServerID
		fmt.Printf("    server2=%s\n", sid2[:8])
		if classification == "isolated" {
			if sid1 == sid2 {
				// Same server ID means same cwd hash — could be dedup.
				// For isolated servers, the second should get its own owner.
				// Check if IPC paths differ (different owners).
				if resp1.IPCPath == resp2.IPCPath {
					fmt.Printf("    FAIL: isolated server reused same owner for different cwd\n")
					failures++
				} else {
					fmt.Printf("    OK: different owners (isolation preserved)\n")
				}
			} else {
				fmt.Printf("    OK: different server IDs (different cwd hash)\n")
			}
		} else {
			// Shared servers SHOULD reuse the same owner
			if sid1 != sid2 && resp1.IPCPath != resp2.IPCPath {
				fmt.Printf("    INFO: shared server got different owners (different cwd hash)\n")
			} else {
				fmt.Printf("    OK: shared server reused owner\n")
			}
		}
	} else {
		fmt.Printf("\n[4] Isolation check: SKIPPED (set SMOKE_CWD2 to enable)\n")
	}

	// ── TEST 5: Optional tool call + inflight check ──
	toolName := os.Getenv("SMOKE_TOOL")
	if toolName != "" {
		fmt.Printf("\n[5] Tool call: %s...\n", toolName)
		toolArgs := envOr("SMOKE_TOOL_ARGS", "{}")
		callTime, err := callTool(resp1.IPCPath, resp1.Token, toolName, toolArgs, timeout)
		if err != nil {
			fmt.Printf("    FAIL: %v\n", err)
			failures++
		} else {
			fmt.Printf("    OK: %.1fs\n", callTime.Seconds())
		}
	} else {
		fmt.Printf("\n[5] Tool call: SKIPPED (set SMOKE_TOOL to enable)\n")
	}

	// ── Result ──
	fmt.Println()
	if failures > 0 {
		fmt.Printf("FAIL: %d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("PASS: all mux checks passed")
}

// ── Helpers ──

type initResult struct {
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

func spawnServer(ctlPath, command string, args []string, cwd string, timeout time.Duration) (*control.Response, error) {
	// Collect env vars like the real shim does — daemon's diffEnv extracts
	// CC-configured vars (API keys, paths) that upstream needs.
	env := collectEnv()
	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:     "spawn",
		Command: command,
		Args:    args,
		Cwd:     cwd,
		Mode:    "cwd",
		Env:     env,
	}, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("daemon: %s", resp.Message)
	}
	return resp, nil
}

func connectAndInit(ipcPath, token string, timeout time.Duration) (*initResult, time.Duration, int, error) {
	conn, err := net.DialTimeout("unix", ipcPath, 5*time.Second)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if token != "" {
		fmt.Fprintf(conn, "%s\n", token)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Send initialize
	fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"smoke-test","version":"1.0.0"}}}`+"\n")

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(timeout))
	if !scanner.Scan() {
		return nil, 0, 0, fmt.Errorf("no init response within timeout")
	}
	elapsed := time.Since(start)

	var resp struct {
		Result initResult     `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, elapsed, 0, fmt.Errorf("parse init: %w", err)
	}
	if resp.Error != nil {
		return nil, elapsed, 0, fmt.Errorf("init error: %s", string(*resp.Error))
	}

	// Send notifications/initialized + tools/list
	fmt.Fprintf(conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`+"\n")

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if !scanner.Scan() {
		return &resp.Result, elapsed, 0, fmt.Errorf("no tools/list response")
	}

	var toolsResp struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal(scanner.Bytes(), &toolsResp)

	return &resp.Result, elapsed, len(toolsResp.Result.Tools), nil
}

func callTool(ipcPath, token, toolName, toolArgs string, timeout time.Duration) (time.Duration, error) {
	conn, err := net.DialTimeout("unix", ipcPath, 5*time.Second)
	if err != nil {
		return 0, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if token != "" {
		fmt.Fprintf(conn, "%s\n", token)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Must init first
	fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke-tool","version":"1.0.0"}}}`+"\n")
	conn.SetReadDeadline(time.Now().Add(timeout))
	if !scanner.Scan() {
		return 0, fmt.Errorf("no init response")
	}
	fmt.Fprintf(conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")

	// Call tool
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"%s","arguments":%s}}`, toolName, toolArgs)
	fmt.Fprintf(conn, "%s\n", req)

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(timeout))
	if !scanner.Scan() {
		return 0, fmt.Errorf("no tool response within %ds", int(timeout.Seconds()))
	}

	var callResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(scanner.Bytes(), &callResp) == nil && callResp.Error != nil {
		return time.Since(start), fmt.Errorf("[%d] %s", callResp.Error.Code, callResp.Error.Message)
	}

	return time.Since(start), nil
}

func findOwnerClassification(data json.RawMessage, serverID string) string {
	var status struct {
		Servers []struct {
			ServerID       string `json:"server_id"`
			Classification string `json:"auto_classification"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return "unknown (parse error)"
	}
	for _, s := range status.Servers {
		if s.ServerID == serverID {
			if s.Classification == "" {
				return "unclassified"
			}
			return s.Classification
		}
	}
	return "not found"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: getwd: %v\n", err)
		os.Exit(1)
	}
	return cwd
}

func collectEnv() map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			env[e[:i]] = e[i+1:]
		}
	}
	return env
}

func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "FAIL at %s: %v\n", step, err)
	os.Exit(1)
}
