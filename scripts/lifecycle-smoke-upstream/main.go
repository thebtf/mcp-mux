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

var version = "1"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Arguments struct {
			DelayMS int    `json:"delay_ms"`
			Marker  string `json:"marker"`
		} `json:"arguments"`
	} `json:"params"`
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
		"version":        version,
		"executable":     exe,
	}
	data, _ := json.Marshal(record)
	recordPath := filepath.Join(root, "generation-"+strconv.Itoa(os.Getpid())+".json")
	if err := os.WriteFile(recordPath, data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	sharing := "isolated"
	idleTimeout := 20
	if os.Getenv("MCP_MUX_LIFECYCLE_FIXTURE_SHARED") == "1" {
		sharing = "shared"
		idleTimeout = 300
	}
	var trace *json.Encoder
	if os.Getenv("MCP_MUX_LIFECYCLE_FIXTURE_TRACE") == "1" {
		file, err := os.OpenFile(filepath.Join(root, "frames-"+strconv.Itoa(os.Getpid())+".ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer file.Close()
		trace = json.NewEncoder(file)
	}
	recordFrame := func(kind string, frame json.RawMessage) {
		if trace != nil {
			if err := trace.Encode(map[string]any{"kind": kind, "utc": time.Now().UTC().Format(time.RFC3339Nano), "leader_pid": os.Getpid(), "version": version, "frame": frame}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
	}
	enc := json.NewEncoder(os.Stdout)
	writeResponse := func(response any) {
		if err := enc.Encode(response); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		recordFrame("received", json.RawMessage(scanner.Bytes()))
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
					"x-mux": map[string]any{"sharing": sharing, "idleTimeout": idleTimeout},
				},
				"serverInfo": map[string]any{"name": "lifecycle-smoke", "version": version},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "lifecycle_probe",
				"description": "Return the real upstream process identity.",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			if req.Params.Arguments.DelayMS < 0 || req.Params.Arguments.DelayMS > 10000 {
				writeResponse(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "delay_ms must be between 0 and 10000"}})
				continue
			}
			if req.Params.Arguments.DelayMS > 0 {
				time.Sleep(time.Duration(req.Params.Arguments.DelayMS) * time.Millisecond)
			}
			payload := map[string]any{"leader_pid": os.Getpid(), "descendant_pid": descendantPID, "version": version, "marker": req.Params.Arguments.Marker}
			text, _ := json.Marshal(payload)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}
		default:
			writeResponse(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
			continue
		}
		writeResponse(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
		recordFrame("completed", json.RawMessage(scanner.Bytes()))
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
