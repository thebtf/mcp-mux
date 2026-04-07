// smoke_isolated.go is a smoke test for isolated MCP servers through mux.
//
// It spawns an MCP server via the daemon, connects a session, sends
// initialize + tools/list, and verifies classification and response.
// Used to validate serena, netcoredbg, and other isolated servers
// without mocking — real upstream processes.
//
// Usage:
//
//	go run testdata/smoke_isolated.go <command> [args...]
//	go run testdata/smoke_isolated.go uvx --from git+https://github.com/oraios/serena serena start-mcp-server --project-from-cwd
//	go run testdata/smoke_isolated.go uv run --project D:\Dev\netcoredbg-mcp netcoredbg-mcp --project-from-cwd
//
// Environment:
//
//	SMOKE_CWD      — working directory for the server (default: current dir)
//	SMOKE_TIMEOUT  — max wait for init response in seconds (default: 60)
//	SMOKE_TOOL     — tool name to call after init (optional, e.g. "activate_project")
//	SMOKE_TOOL_ARGS — JSON args for the tool call (optional)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/thebtf/mcp-mux/internal/control"
	"github.com/thebtf/mcp-mux/internal/serverid"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run testdata/smoke_isolated.go <command> [args...]")
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]

	cwd := os.Getenv("SMOKE_CWD")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	timeoutSec := 60
	if t := os.Getenv("SMOKE_TIMEOUT"); t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			timeoutSec = v
		}
	}
	timeout := time.Duration(timeoutSec) * time.Second

	fmt.Printf("SMOKE TEST: %s %v\n", command, args)
	fmt.Printf("  cwd: %s\n", cwd)
	fmt.Printf("  timeout: %ds\n", timeoutSec)

	// Step 1: Spawn via daemon
	ctlPath := serverid.DaemonControlPath()
	fmt.Printf("  daemon: %s\n", ctlPath)

	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:     "spawn",
		Command: command,
		Args:    args,
		Cwd:     cwd,
		Mode:    "cwd",
	}, timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: spawn: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "FAIL: spawn: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Printf("  spawned: server=%s ipc=%s\n", resp.ServerID[:8], resp.IPCPath)

	// Step 2: Connect to IPC
	conn, err := net.DialTimeout("unix", resp.IPCPath, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Send token
	if resp.Token != "" {
		fmt.Fprintf(conn, "%s\n", resp.Token)
	}
	fmt.Println("  connected to IPC")

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Step 3: Send initialize
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"smoke-test","version":"1.0.0"}}}`
	fmt.Fprintf(conn, "%s\n", initReq)

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(timeout))
	if !scanner.Scan() {
		fmt.Fprintf(os.Stderr, "FAIL: no init response within %ds\n", timeoutSec)
		os.Exit(1)
	}
	initElapsed := time.Since(start)
	fmt.Printf("  init response: %.1fs (%d bytes)\n", initElapsed.Seconds(), len(scanner.Bytes()))

	var initResp struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
			Capabilities map[string]any `json:"capabilities"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &initResp); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: parse init: %v\n", err)
		os.Exit(1)
	}
	if initResp.Error != nil {
		fmt.Fprintf(os.Stderr, "FAIL: init error: [%d] %s\n", initResp.Error.Code, initResp.Error.Message)
		os.Exit(1)
	}
	fmt.Printf("  server: %s %s\n", initResp.Result.ServerInfo.Name, initResp.Result.ServerInfo.Version)

	// Step 4: Send notifications/initialized
	fmt.Fprintf(conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")

	// Step 5: Send tools/list
	fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`+"\n")

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if !scanner.Scan() {
		fmt.Fprintf(os.Stderr, "FAIL: no tools/list response\n")
		os.Exit(1)
	}

	var toolsResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &toolsResp); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: parse tools: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  tools: %d\n", len(toolsResp.Result.Tools))
	for i, t := range toolsResp.Result.Tools {
		if i < 5 {
			fmt.Printf("    - %s\n", t.Name)
		}
	}
	if len(toolsResp.Result.Tools) > 5 {
		fmt.Printf("    ... and %d more\n", len(toolsResp.Result.Tools)-5)
	}

	// Step 6: Optional tool call
	toolName := os.Getenv("SMOKE_TOOL")
	toolArgs := os.Getenv("SMOKE_TOOL_ARGS")
	if toolName != "" {
		if toolArgs == "" {
			toolArgs = "{}"
		}
		callReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"%s","arguments":%s}}`, toolName, toolArgs)
		fmt.Printf("  calling tool: %s\n", toolName)
		fmt.Fprintf(conn, "%s\n", callReq)

		conn.SetReadDeadline(time.Now().Add(timeout))
		callStart := time.Now()
		if !scanner.Scan() {
			fmt.Fprintf(os.Stderr, "FAIL: no tool response within %ds\n", timeoutSec)
			os.Exit(1)
		}
		callElapsed := time.Since(callStart)
		fmt.Printf("  tool response: %.1fs (%d bytes)\n", callElapsed.Seconds(), len(scanner.Bytes()))

		// Check for error
		var callResp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &callResp); err == nil && callResp.Error != nil {
			fmt.Fprintf(os.Stderr, "FAIL: tool error: [%d] %s\n", callResp.Error.Code, callResp.Error.Message)
			os.Exit(1)
		}
	}

	fmt.Println("\nPASS")
}
