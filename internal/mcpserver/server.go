// Package mcpserver implements a minimal MCP server for the control plane.
//
// It runs on stdio and exposes tools for managing mcp-mux instances
// (mux_list, mux_stop, mux_restart) and a built-in prompt ("mux-guide")
// that teaches connecting agents what mcp-mux is and how to use it.
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

	"github.com/thebtf/mcp-mux/internal/control"
	"github.com/thebtf/mcp-mux/internal/ipc"
)

// instructions is injected into the initialize response so the connecting agent
// immediately understands what this server is and how to interact with it.
const instructions = `You are connected to mcp-mux, a transparent stdio multiplexer for MCP servers.

mcp-mux allows multiple Claude Code sessions to share a single upstream MCP server process,
reducing memory usage by ~3x. This control-plane server lets you monitor and manage all
running mcp-mux instances.

Available tools:
- mux_list: Show all running MCP server instances (PID, sessions, classification, caches).
- mux_stop: Gracefully stop an instance by server_id (with optional drain or force).
- mux_restart: Restart an instance — stops the old one, spawns a new daemon, clients auto-reconnect.

Available prompts:
- mux-guide: Full reference on mcp-mux architecture, classification, and management.

Quick start: call mux_list to see what's running, then use server_id from the output for stop/restart.`

// Server is a minimal MCP server that provides control plane tools and prompts.
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
		case "prompts/list":
			s.handlePromptsList(msg.ID)
		case "prompts/get":
			s.handlePromptsGet(msg.ID, msg.Params)
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
		"protocolVersion": "2025-11-25",
		"capabilities": map[string]any{
			"tools":   map[string]any{},
			"prompts": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "mcp-mux",
			"version": "2.0.0",
		},
		"instructions": instructions,
	}
	s.sendResult(id, result)
}

// --- Prompts ---

func (s *Server) handlePromptsList(id json.RawMessage) {
	prompts := []map[string]any{
		{
			"name":        "mux-guide",
			"description": "Full reference guide for mcp-mux: architecture, sharing modes, auto-classification, daemon management, and troubleshooting.",
		},
		{
			"name":        "mux-status-summary",
			"description": "Get a human-readable summary of all running mcp-mux instances. Calls mux_list internally and formats the output.",
		},
	}
	s.sendResult(id, map[string]any{"prompts": prompts})
}

func (s *Server) handlePromptsGet(id json.RawMessage, params json.RawMessage) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		s.sendError(id, -32602, fmt.Sprintf("invalid params: %v", err))
		return
	}

	switch req.Name {
	case "mux-guide":
		s.sendResult(id, map[string]any{
			"description": "Full reference guide for mcp-mux",
			"messages": []map[string]any{
				{
					"role": "user",
					"content": map[string]any{
						"type": "text",
						"text": muxGuidePrompt,
					},
				},
			},
		})
	case "mux-status-summary":
		s.sendResult(id, map[string]any{
			"description": "Summarize running mcp-mux instances",
			"messages": []map[string]any{
				{
					"role": "user",
					"content": map[string]any{
						"type": "text",
						"text": "Call the mux_list tool and provide a concise human-readable summary of all running MCP server instances. Group them by classification (shared/isolated/session-aware). For each, show: server name (from command+args), PID, session count, and whether caches are warm. Highlight any issues (zero sessions, high pending requests, stale instances).",
					},
				},
			},
		})
	default:
		s.sendError(id, -32602, fmt.Sprintf("unknown prompt: %s", req.Name))
	}
}

// muxGuidePrompt is the full reference guide returned by the "mux-guide" prompt.
const muxGuidePrompt = `# mcp-mux Reference Guide

## What is mcp-mux?

mcp-mux is a transparent stdio multiplexer for MCP (Model Context Protocol) servers.
It allows multiple Claude Code sessions to share a single instance of each MCP server,
reducing process count and memory by ~3x.

## How It Works

When you configure an MCP server with mcp-mux as a wrapper:

` + "```" + `json
{ "command": "mcp-mux", "args": ["uvx", "engram-mcp-server"] }
` + "```" + `

The first invocation becomes the "owner" — it spawns the real upstream server and listens
for IPC connections. Subsequent invocations connect as clients through the same upstream.

` + "```" + `
CC Session 1 ──stdio──> mcp-mux (client) ──IPC──┐
CC Session 2 ──stdio──> mcp-mux (client) ──IPC──┤──> mcp-mux (owner) ──stdio──> upstream
CC Session 3 ──stdio──> mcp-mux (client) ──IPC──┘
` + "```" + `

## Sharing Modes

| Mode | When | Behavior |
|------|------|----------|
| **shared** (default) | Stateless servers (engram, tavily, context7) | One upstream, all sessions share it |
| **isolated** | Stateful servers (playwright, desktop-commander) | Each session gets its own upstream |
| **session-aware** | Servers declaring x-mux.sharing: "session-aware" | One upstream, sessions identified via _meta.muxSessionId |

## Auto-Classification

mcp-mux automatically classifies servers by two methods (priority order):

1. **x-mux capability** (highest priority): Server declares ` + "`" + `x-mux.sharing` + "`" + ` in its initialize response capabilities.
2. **Tool-name heuristics**: Tools matching patterns like ` + "`" + `browser_*` + "`" + `, ` + "`" + `session_*` + "`" + `, ` + "`" + `editor_*` + "`" + ` trigger isolation.

## Response Caching

mcp-mux caches these responses from the first session and replays them instantly to later sessions:
- ` + "`" + `initialize` + "`" + ` (with protocolVersion fingerprint matching)
- ` + "`" + `tools/list` + "`" + `
- ` + "`" + `prompts/list` + "`" + `
- ` + "`" + `resources/list` + "`" + `
- ` + "`" + `resources/templates/list` + "`" + `

Caches auto-invalidate on ` + "`" + `notifications/**/list_changed` + "`" + `.

## Global Daemon (experimental)

Set ` + "`" + `MCP_MUX_GLOBAL_DAEMON=1` + "`" + ` to enable a single daemon process that manages ALL upstreams:

- Upstreams survive CC session disconnects (30s grace period by default)
- Persistent servers (x-mux.persistent: true) survive indefinitely
- Auto-respawn of crashed persistent servers
- Daemon auto-exits after 5min idle (no owners, no sessions)

Control: ` + "`" + `mcp-mux daemon` + "`" + ` (start), ` + "`" + `mcp-mux stop` + "`" + ` (stop all), ` + "`" + `mcp-mux status` + "`" + ` (inspect).

## Management Tools

Use the tools exposed by this control-plane server:

### mux_list
Returns JSON array of all running instances with:
- ` + "`" + `server_id` + "`" + `: 16-char hex ID (use first 8 chars as shorthand)
- ` + "`" + `command` + "`" + ` + ` + "`" + `args` + "`" + `: what upstream is running
- ` + "`" + `upstream_pid` + "`" + `: OS process ID of the upstream
- ` + "`" + `session_count` + "`" + `: how many CC sessions are connected
- ` + "`" + `pending_requests` + "`" + `: in-flight requests to upstream
- ` + "`" + `auto_classification` + "`" + `: shared/isolated/session-aware
- ` + "`" + `cached_init` + "`" + `/` + "`" + `cached_tools` + "`" + `: whether caches are warm

### mux_stop
Gracefully drain and stop an instance:
- ` + "`" + `server_id` + "`" + ` (required): from mux_list output
- ` + "`" + `force` + "`" + ` (optional): skip drain, kill immediately

### mux_restart
Stop + re-spawn as daemon. Existing CC clients reconnect on next tool call:
- ` + "`" + `server_id` + "`" + ` (required): from mux_list output
- ` + "`" + `force` + "`" + ` (optional): force-stop before restart

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| MCP_MUX_ISOLATED | 0 | Force isolated mode for this server |
| MCP_MUX_STATELESS | 0 | Ignore cwd in server identity hash |
| MCP_MUX_GLOBAL_DAEMON | 0 | Enable global daemon mode |
| MCP_MUX_GRACE | 30s | Grace period before reaping idle owners (daemon mode) |
| MCP_MUX_IDLE_TIMEOUT | 5m | Daemon auto-exit after this idle period |

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| "stale socket" in status | Crashed owner left socket file | ` + "`" + `mcp-mux stop` + "`" + ` cleans stale sockets |
| Server classified as isolated unexpectedly | Tool names match isolation patterns | Add x-mux capability to server |
| High pending_requests | Upstream is slow or stuck | Check upstream logs, consider restart |
| Session count = 0 but server alive | All CC sessions disconnected | Will be reaped after grace period (daemon) or stays alive (legacy) |
`

// --- Tools ---

func (s *Server) handleToolsList(id json.RawMessage) {
	tools := []map[string]any{
		{
			"name": "mux_list",
			"description": "List all running mcp-mux managed MCP server instances. " +
				"Returns compact summary by default: server name, sessions, classification, version. " +
				"Set verbose=true for full details (PID, IPC path, cache status, classification reason). " +
				"By default shows only servers belonging to this CC session's project. " +
				"Set all=true to see servers from all projects/sessions. " +
				"Use server_id or name from the output to target mux_stop or mux_restart.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"verbose": map[string]any{
						"type":        "boolean",
						"description": "Return full status details for each server (default: compact summary).",
						"default":     false,
					},
					"all": map[string]any{
						"type":        "boolean",
						"description": "Show servers from all projects/sessions, not just this one (default: false).",
						"default":     false,
					},
				},
			},
		},
		{
			"name": "mux_stop",
			"description": "Gracefully stop a running MCP server instance. " +
				"Identify the target by server_id (hex hash from mux_list) or by name " +
				"(substring match against command and args, e.g. 'tavily', 'aimux', 'serena'). " +
				"Drains pending requests (up to 30s) before shutdown. Set force=true to kill immediately. " +
				"The upstream process is terminated and the IPC socket cleaned up. " +
				"Connected sessions will reconnect on next tool call (daemon auto-respawns the server).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "Hex server ID from mux_list (e.g. '03017faad92416e6'). Provide this OR name.",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Substring to match against command+args (e.g. 'tavily', 'aimux', 'engram'). Case-insensitive. Fails if multiple servers match.",
					},
					"force": map[string]any{
						"type":        "boolean",
						"description": "Skip drain and kill immediately.",
						"default":     false,
					},
				},
			},
		},
		{
			"name": "mux_restart",
			"description": "Restart an MCP server: stop the current upstream process and spawn a fresh one " +
				"with the same command and args. All connected CC sessions share the new upstream — " +
				"the next request from any session goes to the new process. " +
				"Use after updating server code (git pull, npm install, pip upgrade) or when a server is stuck. " +
				"Identify by server_id or name (substring match, e.g. 'aimux', 'tavily').",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "Hex server ID from mux_list. Provide this OR name.",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Substring to match against command+args (e.g. 'tavily', 'aimux'). Case-insensitive.",
					},
					"force": map[string]any{
						"type":        "boolean",
						"description": "Force-stop before restart (no drain).",
						"default":     false,
					},
				},
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
		s.toolMuxList(id, call.Arguments)
	case "mux_stop":
		s.toolMuxStop(id, call.Arguments)
	case "mux_restart":
		s.toolMuxRestart(id, call.Arguments)
	default:
		s.sendToolError(id, fmt.Sprintf("unknown tool: %s", call.Name))
	}
}

// toolMuxList scans all .ctl.sock files and queries status from each.
// By default filters to servers belonging to this CC session's project (by cwd).
// Set all=true to see all servers across all projects.
func (s *Server) toolMuxList(id json.RawMessage, args json.RawMessage) {
	var params struct {
		Verbose bool `json:"verbose"`
		All     bool `json:"all"`
	}
	if args != nil {
		_ = json.Unmarshal(args, &params)
	}

	// Determine this session's cwd for filtering
	myCwd, _ := os.Getwd()
	myCwd = strings.ToLower(filepath.Clean(myCwd))

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
				// Filter by cwd unless all=true
				if !params.All && myCwd != "" {
					if !s.ownerHasCwd(data, myCwd) {
						continue
					}
				}

				if params.Verbose {
					data["server_id"] = serverID
					servers = append(servers, data)
				} else {
					// Compact: only key fields
					compact := map[string]any{
						"server_id": serverID,
						"command":   data["command"],
						"args":      data["args"],
						"sessions":  data["session_count"],
						"pending":   data["pending_requests"],
						"class":     data["auto_classification"],
						"version":   data["mux_version"],
					}
					servers = append(servers, compact)
				}
			}
		}
	}

	result, _ := json.MarshalIndent(servers, "", "  ")
	s.sendToolResult(id, string(result))
}

// ownerHasCwd checks if an owner's status data contains a given cwd in its cwdSet or primary cwd.
func (s *Server) ownerHasCwd(data map[string]any, cwd string) bool {
	// Check primary cwd
	if primary, ok := data["cwd"].(string); ok {
		if strings.ToLower(filepath.Clean(primary)) == cwd {
			return true
		}
	}
	// Check cwdSet (array of strings in status response)
	if cwdSet, ok := data["cwd_set"].([]any); ok {
		for _, c := range cwdSet {
			if cs, ok := c.(string); ok {
				if strings.ToLower(filepath.Clean(cs)) == cwd {
					return true
				}
			}
		}
	}
	return false
}

// toolMuxStop stops a specific server.
func (s *Server) toolMuxStop(id json.RawMessage, args json.RawMessage) {
	var params struct {
		ServerID string `json:"server_id"`
		Name     string `json:"name"`
		Force    bool   `json:"force"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		s.sendToolError(id, fmt.Sprintf("invalid arguments: %v", err))
		return
	}

	serverID, err := s.resolveServerID(params.ServerID, params.Name)
	if err != nil {
		s.sendToolError(id, err.Error())
		return
	}

	ctlPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.ctl.sock", serverID))

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
		s.sendToolError(id, fmt.Sprintf("failed to stop %s: %v", serverID, err))
		return
	}

	s.sendToolResult(id, resp.Message)
}

// toolMuxRestart stops a server and spawns a new daemon owner.
func (s *Server) toolMuxRestart(id json.RawMessage, args json.RawMessage) {
	var params struct {
		ServerID string `json:"server_id"`
		Name     string `json:"name"`
		Force    bool   `json:"force"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		s.sendToolError(id, fmt.Sprintf("invalid arguments: %v", err))
		return
	}

	serverID, err := s.resolveServerID(params.ServerID, params.Name)
	if err != nil {
		s.sendToolError(id, err.Error())
		return
	}

	ctlPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.ctl.sock", serverID))

	// Get current status to learn command + args
	statusResp, err := control.Send(ctlPath, control.Request{Cmd: "status"})
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("server %s unreachable: %v", serverID, err))
		return
	}

	var status struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(statusResp.Data, &status); err != nil || status.Command == "" {
		s.sendToolError(id, fmt.Sprintf("server %s has no command info", serverID))
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

// resolveServerID returns a server ID from either an explicit ID or a name substring match.
// If name is provided, scans all running instances and matches against command+args (case-insensitive).
// Prefers servers belonging to this CC session's project (by cwd).
// Fails if neither is provided, or if name matches zero or multiple servers after cwd filtering.
func (s *Server) resolveServerID(serverID, name string) (string, error) {
	if serverID != "" {
		return serverID, nil
	}
	if name == "" {
		return "", fmt.Errorf("provide either server_id or name")
	}

	myCwd, _ := os.Getwd()
	myCwd = strings.ToLower(filepath.Clean(myCwd))
	name = strings.ToLower(name)
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return "", fmt.Errorf("read temp dir: %v", err)
	}

	type candidate struct {
		sid    string
		hasCwd bool // true if this server belongs to our cwd
	}
	var candidates []candidate
	for _, entry := range entries {
		fname := entry.Name()
		if !strings.HasPrefix(fname, "mcp-mux-") || !strings.HasSuffix(fname, ".ctl.sock") {
			continue
		}

		path := filepath.Join(tmpDir, fname)
		sid := strings.TrimPrefix(strings.TrimSuffix(fname, ".ctl.sock"), "mcp-mux-")

		resp, err := control.Send(path, control.Request{Cmd: "status"})
		if err != nil || !resp.OK || resp.Data == nil {
			continue
		}

		var data map[string]any
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			continue
		}

		// Match against command + args concatenated
		cmd, _ := data["command"].(string)
		var args []string
		if rawArgs, ok := data["args"].([]any); ok {
			for _, a := range rawArgs {
				if as, ok := a.(string); ok {
					args = append(args, as)
				}
			}
		}
		haystack := strings.ToLower(cmd + " " + strings.Join(args, " "))
		if strings.Contains(haystack, name) {
			candidates = append(candidates, candidate{
				sid:    sid,
				hasCwd: s.ownerHasCwd(data, myCwd),
			})
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no server matching '%s' found", name)
	}

	// Prefer servers belonging to this session's cwd
	var myCwdMatches []string
	var allMatches []string
	for _, c := range candidates {
		allMatches = append(allMatches, c.sid)
		if c.hasCwd {
			myCwdMatches = append(myCwdMatches, c.sid)
		}
	}

	// If exactly one match in our cwd — use it (even if there are others)
	if len(myCwdMatches) == 1 {
		return myCwdMatches[0], nil
	}
	// If multiple in our cwd — still ambiguous
	if len(myCwdMatches) > 1 {
		return "", fmt.Errorf("'%s' matches %d servers in this project — use server_id", name, len(myCwdMatches))
	}
	// No cwd match — fall back to all matches
	if len(allMatches) == 1 {
		return allMatches[0], nil
	}
	return "", fmt.Errorf("'%s' matches %d servers (none in this project) — be more specific or use server_id", name, len(allMatches))
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
