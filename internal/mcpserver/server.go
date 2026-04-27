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

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
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
	reader        *bufio.Scanner
	writer        io.Writer
	logger        *log.Logger
	BaseDir       string // directory scanned for .ctl.sock files; empty = os.TempDir()
	DaemonCtlPath string // injectable daemon control path; empty = serverid.DaemonControlPath("", "mcp-mux")
}

// socketDir returns the directory to scan for .ctl.sock files.
func (s *Server) socketDir() string {
	if s.BaseDir != "" {
		return s.BaseDir
	}
	return os.TempDir()
}

// daemonCtlPath returns the path to the mcp-mux daemon control socket.
// Uses DaemonCtlPath if set, otherwise composes from BaseDir + engine name "mcp-mux".
// BaseDir matters for test isolation: a test fixture sets BaseDir to a temp dir so
// daemonCtlPath() points at a non-existent socket inside that temp dir, not at the
// real workstation daemon's socket in os.TempDir().
func (s *Server) daemonCtlPath() string {
	if s.DaemonCtlPath != "" {
		return s.DaemonCtlPath
	}
	return serverid.DaemonControlPath(s.BaseDir, "mcp-mux")
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

// toolMuxList queries the mcp-mux daemon for all managed owners via the list_owners RPC.
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

	myCwd, _ := os.Getwd()
	myCwd = strings.ToLower(filepath.Clean(myCwd))

	resp, err := control.Send(s.daemonCtlPath(), control.Request{Cmd: "list_owners"})
	if err != nil || !resp.OK || resp.Data == nil {
		result, _ := json.Marshal(map[string]any{
			"servers": []any{},
			"note":    "local mcp-mux daemon not running — start it with `mcp-mux daemon` or invoke any mcp-mux-wrapped tool to auto-spawn",
		})
		s.sendToolResult(id, string(result))
		return
	}

	var listResp control.ListOwnersResponse
	if err := json.Unmarshal(resp.Data, &listResp); err != nil {
		result, _ := json.Marshal(map[string]any{
			"servers": []any{},
			"note":    "local mcp-mux daemon not running — start it with `mcp-mux daemon` or invoke any mcp-mux-wrapped tool to auto-spawn",
		})
		s.sendToolResult(id, string(result))
		return
	}

	var servers []map[string]any
	for _, owner := range listResp.Owners {
		if !params.All && myCwd != "" {
			if !s.ownerInfoHasCwd(owner, myCwd) {
				continue
			}
		}
		if params.Verbose {
			servers = append(servers, map[string]any{
				"server_id":      owner.ServerID,
				"command":        owner.Command,
				"args":           owner.Args,
				"cwd":            owner.Cwd,
				"cwd_set":        owner.CwdSet,
				"sessions":       owner.Sessions,
				"pending":        owner.Pending,
				"classification": owner.Classification,
				"mux_version":    owner.MuxVersion,
				"persistent":     owner.Persistent,
			})
		} else {
			servers = append(servers, map[string]any{
				"server_id": owner.ServerID,
				"command":   owner.Command,
				"args":      owner.Args,
				"sessions":  owner.Sessions,
				"pending":   owner.Pending,
				"class":     owner.Classification,
				"version":   owner.MuxVersion,
			})
		}
	}

	result, _ := json.MarshalIndent(servers, "", "  ")
	s.sendToolResult(id, string(result))
}

// ownerInfoHasCwd checks if an OwnerInfo matches a given cwd (case-insensitive, cleaned paths).
func (s *Server) ownerInfoHasCwd(info control.OwnerInfo, cwd string) bool {
	if strings.ToLower(filepath.Clean(info.Cwd)) == cwd {
		return true
	}
	for _, c := range info.CwdSet {
		if strings.ToLower(filepath.Clean(c)) == cwd {
			return true
		}
	}
	return false
}

// resolveOwner resolves a server by exact server_id or name substring via the daemon's list_owners RPC.
// Returns the matching OwnerInfo or an error if not found or daemon unavailable.
func (s *Server) resolveOwner(serverID, name string) (control.OwnerInfo, error) {
	if serverID == "" && name == "" {
		return control.OwnerInfo{}, fmt.Errorf("provide either server_id or name")
	}

	resp, err := control.Send(s.daemonCtlPath(), control.Request{Cmd: "list_owners"})
	if err != nil || !resp.OK || resp.Data == nil {
		return control.OwnerInfo{}, fmt.Errorf("mcp-mux daemon not reachable: %v", err)
	}
	var listResp control.ListOwnersResponse
	if err := json.Unmarshal(resp.Data, &listResp); err != nil {
		return control.OwnerInfo{}, fmt.Errorf("invalid list_owners response: %v", err)
	}

	if serverID != "" {
		for _, owner := range listResp.Owners {
			if owner.ServerID == serverID {
				return owner, nil
			}
		}
		return control.OwnerInfo{}, fmt.Errorf("server_id %s is not managed by this mcp-mux daemon", serverID)
	}

	needle := strings.ToLower(name)
	var matches []control.OwnerInfo
	for _, owner := range listResp.Owners {
		haystack := strings.ToLower(owner.Command + " " + strings.Join(owner.Args, " "))
		if strings.Contains(haystack, needle) {
			matches = append(matches, owner)
		}
	}
	if len(matches) == 0 {
		return control.OwnerInfo{}, fmt.Errorf("no server matching '%s' found", name)
	}
	if len(matches) > 1 {
		return control.OwnerInfo{}, fmt.Errorf("'%s' matches %d servers — use server_id", name, len(matches))
	}
	return matches[0], nil
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

	owner, err := s.resolveOwner(params.ServerID, params.Name)
	if err != nil {
		s.sendToolError(id, err.Error())
		return
	}

	ctlPath := filepath.Join(s.socketDir(), fmt.Sprintf("mcp-mux-%s.ctl.sock", owner.ServerID))

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
		s.sendToolError(id, fmt.Sprintf("failed to stop %s: %v", owner.ServerID, err))
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

	owner, err := s.resolveOwner(params.ServerID, params.Name)
	if err != nil {
		s.sendToolError(id, err.Error())
		return
	}

	if owner.Command == "" {
		s.sendToolError(id, fmt.Sprintf("server %s has no command info", owner.ServerID))
		return
	}

	// Reject restart when requests are in-flight unless force is set
	if owner.Pending > 0 && !params.Force {
		s.sendToolError(id, fmt.Sprintf("server %s has %d pending requests. Use force=true to kill them, or wait for completion.", owner.ServerID[:8], owner.Pending))
		return
	}

	ctlPath := filepath.Join(s.socketDir(), fmt.Sprintf("mcp-mux-%s.ctl.sock", owner.ServerID))

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
		s.sendToolError(id, fmt.Sprintf("failed to stop %s: %v", owner.ServerID, err))
		return
	}

	// Wait briefly for shutdown to complete
	time.Sleep(500 * time.Millisecond)

	// Verify the old owner is gone
	if ipc.IsAvailable(filepath.Join(s.socketDir(), fmt.Sprintf("mcp-mux-%s.sock", owner.ServerID))) {
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
	daemonArgs = append(daemonArgs, owner.Command)
	daemonArgs = append(daemonArgs, owner.Args...)

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

	warning := ""
	if params.Force && owner.Pending > 0 {
		warning = fmt.Sprintf("WARNING: force restart killed %d pending requests. ", owner.Pending)
	}
	s.sendToolResult(id, fmt.Sprintf("%srestarted: stopped (%s), new daemon PID %d", warning, stopResp.Message, cmd.Process.Pid))
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
