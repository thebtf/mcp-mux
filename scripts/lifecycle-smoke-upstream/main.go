package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--descendant" {
		for {
			time.Sleep(time.Hour)
		}
	}

	root := os.Getenv("MCP_MUX_LIFECYCLE_FIXTURE_ROOT")
	if root == "" {
		fmt.Fprintln(os.Stderr, "MCP_MUX_LIFECYCLE_FIXTURE_ROOT is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	descendant := exec.Command(exe, "--descendant")
	if err := descendant.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	descendantPID := descendant.Process.Pid
	if err := descendant.Process.Release(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	record := map[string]any{
		"leader_pid":     os.Getpid(),
		"descendant_pid": descendantPID,
		"started_utc":    time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, _ := json.Marshal(record)
	recordPath := filepath.Join(root, "generation-"+strconv.Itoa(os.Getpid())+".json")
	if err := os.WriteFile(recordPath, data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil ||
			req.JSONRPC != "2.0" || len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities": map[string]any{
					"tools": map[string]any{},
					// 20s covers the measured ~5.1s response window plus two 3.9-4.8s
					// Windows process-metadata snapshots used by the smoke's parent proof.
					"x-mux": map[string]any{"sharing": "isolated", "idleTimeout": 20},
				},
				"serverInfo": map[string]any{"name": "lifecycle-smoke", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "lifecycle_probe",
				"description": "Return the real upstream process identity.",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			payload := map[string]any{"leader_pid": os.Getpid(), "descendant_pid": descendantPID}
			text, _ := json.Marshal(payload)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}
		default:
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
			continue
		}
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
	}
}
