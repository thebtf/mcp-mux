// Package mcpserver implements a minimal MCP server for the control plane.
//
// It runs on stdio and exposes tools for managing mcp-mux instances:
// mux_list, mux_stop, mux_restart.
package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitswan-space/mcp-mux/internal/control"
	"github.com/bitswan-space/mcp-mux/internal/ipc"
)

// Server is a minimal MCP server that provides control plane tools.
type Server struct {
	reader *bufio.Scanner
	writer io.Writer
	logger *log.Logger
}

// NewServer creates a new MCP control server.
func NewServer(r io.Reader, w io.Writer, logger *log.Logger) *Server {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	return &Server{
		reader: scanner,
		writer: w,
		logger: logger,
	}
}

// Run processes MCP messages until EOF.
func (s *Server) Run() error {
	for s.reader.Scan() {
		line := s.reader.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method,omitempty"`
			Params  json.RawMessage `json:"params,omitempty"`
		}

		if err := json.Unmarshal(line, &msg); err != nil {
			s.logger.Printf("parse error: %v", err)
			continue
		}

		// Notification (no ID) — ignore
		if msg.ID == nil {
			continue
		}

		switch msg.Method {
		case "initialize":
			s.handleInitialize(msg.ID)
		case "tools/list":
			s.handleToolsList(msg.ID)
		case "tools/call":
			s.handleToolsCall(msg.ID, msg.Params)
		case "ping":
			s.sendResult(msg.ID, map[string]any{})
		default:
			s.sendError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method))
		}
	}

	return s.reader.Err()
}

func (s *Server) handleInitialize(id json.RawMessage) {
	result := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "mcp-mux-control",
			"version": "1.0.0",
		},
	}
	s.sendResult(id, result)
}

func (s *Server) handleToolsList(id json.RawMessage) {
	tools := []map[string]any{
		{
			"name":        "mux_list",
			"description": "List all running mcp-mux instances with their status, command, args, PID, sessions, and pending requests.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "mux_stop",
			"description": "Stop a specific mcp-mux instance by server_id. Use force=true for immediate shutdown without drain.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "Server ID from mux_list output",
					},
					"force": map[string]any{
						"type":        "boolean",
						"description": "Force immediate shutdown without waiting for pending requests",
						"default":     false,
					},
				},
				"required": []string{"server_id"},
			},
		},
		{
			"name":        "mux_restart",
			"description": "Restart a specific mcp-mux instance. Stops the server and spawns a new daemon owner. CC clients reconnect automatically on next tool call.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "Server ID from mux_list output",
					},
					"force": map[string]any{
						"type":        "boolean",
						"description": "Force stop before restart (no drain)",
						"default":     false,
					},
				},
				"required": []string{"server_id"},
			},
		},
	}

	s.sendResult(id, map[string]any{"tools": tools})
}

func (s *Server) handleToolsCall(id json.RawMessage, params json.RawMessage) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		s.sendError(id, -32602, fmt.Sprintf("invalid params: %v", err))
		return
	}

	switch call.Name {
	case "mux_list":
		s.toolMuxList(id)
	case "mux_stop":
		s.toolMuxStop(id, call.Arguments)
	case "mux_restart":
		s.toolMuxRestart(id, call.Arguments)
	default:
		s.sendToolError(id, fmt.Sprintf("unknown tool: %s", call.Name))
	}
}

// toolMuxList scans all .ctl.sock files and queries status from each.
func (s *Server) toolMuxList(id json.RawMessage) {
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("read temp dir: %v", err))
		return
	}

	var servers []map[string]any

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "mcp-mux-") || !strings.HasSuffix(name, ".ctl.sock") {
			continue
		}

		path := filepath.Join(tmpDir, name)
		serverID := strings.TrimPrefix(strings.TrimSuffix(name, ".ctl.sock"), "mcp-mux-")

		resp, err := control.Send(path, control.Request{Cmd: "status"})
		if err != nil {
			continue // skip unreachable
		}

		if resp.OK && resp.Data != nil {
			var data map[string]any
			if err := json.Unmarshal(resp.Data, &data); err == nil {
				data["server_id"] = serverID
				servers = append(servers, data)
			}
		}
	}

	result, _ := json.MarshalIndent(servers, "", "  ")
	s.sendToolResult(id, string(result))
}

// toolMuxStop stops a specific server.
func (s *Server) toolMuxStop(id json.RawMessage, args json.RawMessage) {
	var params struct {
		ServerID string `json:"server_id"`
		Force    bool   `json:"force"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		s.sendToolError(id, fmt.Sprintf("invalid arguments: %v", err))
		return
	}

	ctlPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.ctl.sock", params.ServerID))

	drainMs := 30000
	timeout := 35 * time.Second
	if params.Force {
		drainMs = 0
		timeout = 5 * time.Second
	}

	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:            "shutdown",
		DrainTimeoutMs: drainMs,
	}, timeout)
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("failed to stop %s: %v", params.ServerID, err))
		return
	}

	s.sendToolResult(id, resp.Message)
}

// toolMuxRestart stops a server and spawns a new daemon owner.
func (s *Server) toolMuxRestart(id json.RawMessage, args json.RawMessage) {
	var params struct {
		ServerID string `json:"server_id"`
		Force    bool   `json:"force"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		s.sendToolError(id, fmt.Sprintf("invalid arguments: %v", err))
		return
	}

	ctlPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.ctl.sock", params.ServerID))

	// Get current status to learn command + args
	statusResp, err := control.Send(ctlPath, control.Request{Cmd: "status"})
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("server %s unreachable: %v", params.ServerID, err))
		return
	}

	var status struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(statusResp.Data, &status); err != nil || status.Command == "" {
		s.sendToolError(id, fmt.Sprintf("server %s has no command info", params.ServerID))
		return
	}

	// Stop the server
	drainMs := 30000
	timeout := 35 * time.Second
	if params.Force {
		drainMs = 0
		timeout = 5 * time.Second
	}

	stopResp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:            "shutdown",
		DrainTimeoutMs: drainMs,
	}, timeout)
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("failed to stop %s: %v", params.ServerID, err))
		return
	}

	// Wait briefly for shutdown to complete
	time.Sleep(500 * time.Millisecond)

	// Verify the old owner is gone
	if ipc.IsAvailable(filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.sock", params.ServerID))) {
		// Still alive — drain might be in progress, wait more
		time.Sleep(2 * time.Second)
	}

	// Spawn new daemon owner
	exe, err := os.Executable()
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("cannot find mcp-mux binary: %v", err))
		return
	}

	daemonArgs := []string{"--daemon"}
	daemonArgs = append(daemonArgs, status.Command)
	daemonArgs = append(daemonArgs, status.Args...)

	cmd := exec.Command(exe, daemonArgs...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		s.sendToolError(id, fmt.Sprintf("failed to spawn daemon: %v", err))
		return
	}

	// Detach — don't wait for the daemon
	go cmd.Wait()

	s.sendToolResult(id, fmt.Sprintf("restarted: stopped (%s), new daemon PID %d", stopResp.Message, cmd.Process.Pid))
}

// --- JSON-RPC response helpers ---

func (s *Server) sendResult(id json.RawMessage, result any) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	s.writer.Write(data)
}

func (s *Server) sendError(id json.RawMessage, code int, message string) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	s.writer.Write(data)
}

func (s *Server) sendToolResult(id json.RawMessage, text string) {
	s.sendResult(id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
}

func (s *Server) sendToolError(id json.RawMessage, text string) {
	s.sendResult(id, map[string]any{
		"isError": true,
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
}
