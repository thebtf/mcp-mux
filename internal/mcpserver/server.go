// Package mcpserver implements a minimal MCP server for the control plane.
//
// It runs on stdio and exposes tools for managing mcp-mux instances
// (mux_list, mux_stop, mux_restart) and a built-in prompt ("mux-guide")
// that teaches connecting agents what mcp-mux is and how to use it.
package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/registry"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// instructions is injected into the initialize response so the connecting agent
// immediately understands what this server is and how to interact with it.
const instructions = `You are connected to mcp-mux, a transparent stdio multiplexer for MCP servers.

mcp-mux allows multiple Claude Code sessions to share a single upstream MCP server process,
reducing memory usage by ~3x. This control-plane server lets you monitor and manage all
running mcp-mux instances.

Available tools:
- mux_engines: Show opted-in native muxcore daemon engines.
- mux_prune_engines: Dry-run or remove stale/invalid native muxcore registry descriptors.
- mux_topology: Show owners, registered engines, local mcp-mux processes, and read-only cleanup plan.
- mux_list: Show all running MCP server instances (PID, sessions, classification, caches).
- mux_stop: Gracefully stop an instance by server_id (with optional drain or force).
- mux_restart: Restart one exact managed owner using its daemon-owned context and protocol era.
- mux_hold: Hold one exact local server for replacement after full managed-tree retirement.
- mux_resume: Release one exact hold ID after proven retirement.
- mux_renew: Extend one exact active hold ID, at most one hour.

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
	EngineName    string // engine name used to build owner socket paths; empty = "mcp-mux"
	// ProcessSnapshot is injectable for tests. Production uses a best-effort
	// platform process snapshot limited to mcp-mux launcher/engine processes.
	ProcessSnapshot processSnapshotFunc
}

// engineName returns the engine name used to build owner socket paths.
// Defaults to "mcp-mux" when EngineName is unset; tests for foreign engines
// (e.g. cross_engine_integration_test.go) override this to "aimux-test".
func (s *Server) engineName() string {
	if s.EngineName != "" {
		return s.EngineName
	}
	return "mcp-mux"
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
	return serverid.DaemonControlPath(s.BaseDir, s.engineName())
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
			"tools":   map[string]any{"listChanged": true},
			"prompts": map[string]any{"listChanged": true},
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

Every invocation uses local daemon-managed admission. The daemon retains the upstream's
launch context and protocol era; standalone direct owners are explicitly unsupported.

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

Legacy owners cache these responses for later sessions; native modern owners keep caches off.
Held requests never receive cached success or become replay after release:
- ` + "`" + `initialize` + "`" + ` (with protocolVersion fingerprint matching)
- ` + "`" + `tools/list` + "`" + `
- ` + "`" + `prompts/list` + "`" + `
- ` + "`" + `resources/list` + "`" + `
- ` + "`" + `resources/templates/list` + "`" + `

Caches auto-invalidate on ` + "`" + `notifications/**/list_changed` + "`" + `.

## Global Daemon

The local daemon manages upstream starts and reconnects by default:

- Persistent servers (x-mux.persistent: true) survive host disconnects
- Automatic starts and respawn are fenced during maintenance
- Active maintenance also blocks controlled restart, shutdown, activation, and idle exit

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
Restart through the daemon's exact retained launch context and protocol era, without
reconstructing command arguments or credentials in this control adapter:
- ` + "`" + `server_id` + "`" + ` or ` + "`" + `name` + "`" + `: resolved to one exact local managed owner
- ` + "`" + `force` + "`" + ` (optional): skip drain grace, never bypass maintenance

### mux_hold / mux_resume / mux_renew
Hold one exact full local ` + "`" + `server_id` + "`" + ` for executable replacement.
` + "`" + `hold_seconds` + "`" + ` defaults to 300, with integer range 1..3600;
` + "`" + `drain_timeout_ms` + "`" + ` defaults to 10000 and zero skips drain grace.
Successful hold confirms retired managed trees and returns an opaque ` + "`" + `hold_id` + "`" + `.
Resume and renew use only that exact hold ID. Errors carry stable ` + "`" + `error_code` + "`" + `
and safe maintenance readback; unsupported endpoints never fall back to stop or execution.
CLI equivalents accept flags after the ID: ` + "`" + `mcp-mux hold <exact-id> --ttl 5m --drain-timeout 10s --json` + "`" + `,
` + "`" + `mcp-mux resume <hold-id> --json` + "`" + `, and ` + "`" + `mcp-mux renew <hold-id> --ttl 5m --json` + "`" + `.
Use ` + "`" + `mcp-mux status` + "`" + ` for leases retained after owners retire.
Aware hosts keep their pipes open and receive original-ID held errors without replay.
Modern continuation uses fresh same-era admission, never legacy bootstrap or subscription replay.
Standalone, foreign-engine, and arbitrary old-binary control is outside this feature.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| MCP_MUX_ISOLATED | 0 | Force isolated mode for this server |
| MCP_MUX_STATELESS | 0 | Ignore cwd in server identity hash |
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
			"name": "mux_engines",
			"description": "List opted-in native muxcore daemon engines registered on this host. " +
				"Descriptors are advisory and are verified by daemon status before being marked healthy. " +
				"Default mux_list remains scoped to this mcp-mux daemon namespace; use mux_list(engine_name=...) to list one registered engine explicitly.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name": "mux_prune_engines",
			"description": "Remove stale or invalid native muxcore registry descriptors after verification. " +
				"Dry-run is enabled by default. This only deletes descriptor files; it never stops processes, owners, or daemon control sockets.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dry_run": map[string]any{
						"type":        "boolean",
						"description": "When true, report candidates without deleting descriptor files. Default: true.",
						"default":     true,
					},
					"engine_name": map[string]any{
						"type":        "string",
						"description": "Optional exact engine name filter. Invalid descriptors without an engine name are skipped when this is set.",
					},
					"min_age_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional minimum descriptor age before removal. Useful when pruning non-dry-run on a busy workstation.",
						"default":     0,
					},
				},
			},
		},
		{
			"name": "mux_topology",
			"description": "Return a read-only topology snapshot that joins local mcp-mux owners, opted-in native muxcore registry descriptors, " +
				"local mcp-mux launcher/engine processes, warnings, and cleanup candidates. This tool never stops processes or removes files.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"include_processes": map[string]any{
						"type":        "boolean",
						"description": "Include a best-effort OS process snapshot for local mcp-mux launcher/engine processes. Default: true.",
						"default":     true,
					},
				},
			},
		},
		{
			"name": "mux_list",
			"description": "List all running mcp-mux managed MCP server instances. " +
				"This is this mcp-mux daemon's engine namespace, not a global registry for native muxcore products. " +
				"Use mux_engines to discover opted-in native muxcore engines, then pass engine_name to query exactly one registered engine. " +
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
					"engine_name": map[string]any{
						"type":        "string",
						"description": "Exact opted-in muxcore engine name to query. Empty/default queries only this mcp-mux daemon namespace.",
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
			"description": "Restart one locally managed MCP owner from its daemon-retained context and protocol era. " +
				"Identify by server_id or name (resolved to an exact local server_id). " +
				"Maintenance and unsupported endpoints refuse without direct execution or stop fallback.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
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
						"description": "Skip drain grace; does not bypass a maintenance hold.",
						"default":     false,
					},
				},
			},
		},
	}

	tools = append(tools, maintenanceTools()...)
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
	case "mux_engines":
		s.toolMuxEngines(id, call.Arguments)
	case "mux_prune_engines":
		s.toolMuxPruneEngines(id, call.Arguments)
	case "mux_topology":
		s.toolMuxTopology(id, call.Arguments)
	case "mux_list":
		s.toolMuxList(id, call.Arguments)
	case "mux_stop":
		s.toolMuxStop(id, call.Arguments)
	case "mux_restart":
		s.toolMuxRestart(id, call.Arguments)
	case "mux_hold":
		s.toolMuxMaintenance(id, "hold", call.Arguments)
	case "mux_resume":
		s.toolMuxMaintenance(id, "resume", call.Arguments)
	case "mux_renew":
		s.toolMuxMaintenance(id, "renew", call.Arguments)
	default:
		s.sendToolError(id, fmt.Sprintf("unknown tool: %s", call.Name))
	}
}

// toolMuxList queries the mcp-mux daemon for all managed owners via the list_owners RPC.
// By default filters to servers belonging to this CC session's project (by cwd).
// Set all=true to see all servers across all projects.
func (s *Server) toolMuxList(id json.RawMessage, args json.RawMessage) {
	var params struct {
		Verbose    bool   `json:"verbose"`
		All        bool   `json:"all"`
		EngineName string `json:"engine_name"`
	}
	if args != nil {
		_ = json.Unmarshal(args, &params)
	}
	if strings.TrimSpace(params.EngineName) != "" {
		s.toolMuxListForEngine(id, params.EngineName, params.Verbose, params.All)
		return
	}

	myCwd, _ := os.Getwd()
	myCwd = normalizeCwd(myCwd)

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

	servers := s.formatOwnerList(listResp.Owners, params.Verbose, params.All, myCwd)

	s.sendJSONToolResult(id, servers)
}

func (s *Server) toolMuxEngines(id json.RawMessage, _ json.RawMessage) {
	records, err := registry.ListDescriptors(s.socketDir())
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("list muxcore engine registry: %v", err))
		return
	}
	verifiedRecords := make([]registry.VerifiedDescriptor, 0, len(records))
	for _, rec := range records {
		verifiedRecords = append(verifiedRecords, registry.VerifyDescriptor(rec))
	}
	duplicates := registry.DuplicateHealthyEngineNames(verifiedRecords)

	engines := make([]map[string]any, 0, len(verifiedRecords))
	for _, verified := range verifiedRecords {
		rec := verified.Record
		state := verified.State
		reason := verified.Reason
		if rec.Err == nil && state == registry.StateHealthy {
			if count, ok := duplicates[rec.Descriptor.EngineName]; ok {
				state = registry.StateDuplicate
				if reason == "" {
					reason = fmt.Sprintf("duplicate_engine_name: %d descriptors", count)
				}
			}
		}

		row := map[string]any{
			"descriptor_path": rec.Path,
			"state":           state,
			"reachable":       verified.Reachable,
		}
		if reason != "" {
			row["reason"] = reason
		}
		if rec.Err == nil {
			row["engine_name"] = rec.Descriptor.EngineName
			row["product_name"] = rec.Descriptor.ProductName
			row["pid"] = rec.Descriptor.PID
			if verified.PID != 0 {
				row["status_pid"] = verified.PID
			}
			row["base_dir"] = rec.Descriptor.BaseDir
			row["daemon_control_path"] = rec.Descriptor.DaemonControlPath
			row["started_at"] = rec.Descriptor.StartedAt.Format(time.RFC3339)
			row["muxcore_version"] = rec.Descriptor.MuxcoreVersion
			row["capabilities"] = rec.Descriptor.Capabilities
			row["owner_count"] = verified.OwnerCount
			if verified.DaemonGeneration != "" {
				row["daemon_generation"] = verified.DaemonGeneration
			}
		}
		engines = append(engines, row)
	}

	s.sendJSONToolResult(id, map[string]any{
		"engines":    engines,
		"duplicates": duplicates,
	})
}

func (s *Server) toolMuxPruneEngines(id json.RawMessage, args json.RawMessage) {
	dryRun := true
	var params struct {
		DryRun        *bool  `json:"dry_run"`
		EngineName    string `json:"engine_name"`
		MinAgeSeconds int64  `json:"min_age_seconds"`
	}
	if args != nil {
		_ = json.Unmarshal(args, &params)
	}
	if params.DryRun != nil {
		dryRun = *params.DryRun
	}
	engineName := strings.TrimSpace(params.EngineName)
	now := time.Now()

	records, err := registry.ListDescriptors(s.socketDir())
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("list muxcore engine registry: %v", err))
		return
	}

	candidates := make([]map[string]any, 0)
	removedCount := 0
	skippedCount := 0
	for _, rec := range records {
		verified := registry.VerifyDescriptor(rec)
		if rec.Err == nil && engineName != "" && rec.Descriptor.EngineName != engineName {
			continue
		}
		if rec.Err != nil && engineName != "" {
			skippedCount++
			continue
		}
		if verified.State == registry.StateHealthy {
			continue
		}

		row := map[string]any{
			"descriptor_path": rec.Path,
			"state":           verified.State,
			"reason":          verified.Reason,
			"removed":         false,
		}
		if rec.Err == nil {
			row["engine_name"] = rec.Descriptor.EngineName
			row["product_name"] = rec.Descriptor.ProductName
			row["pid"] = rec.Descriptor.PID
			row["started_at"] = rec.Descriptor.StartedAt.Format(time.RFC3339)
			ageSeconds := int64(now.Sub(rec.Descriptor.StartedAt).Seconds())
			row["age_seconds"] = ageSeconds
			if params.MinAgeSeconds > 0 && ageSeconds < params.MinAgeSeconds {
				row["skip_reason"] = "below_min_age_seconds"
				skippedCount++
				candidates = append(candidates, row)
				continue
			}
		}
		if dryRun {
			candidates = append(candidates, row)
			continue
		}
		if err := os.Remove(rec.Path); err != nil {
			row["error"] = err.Error()
		} else {
			row["removed"] = true
			removedCount++
		}
		candidates = append(candidates, row)
	}

	s.sendJSONToolResult(id, map[string]any{
		"dry_run":       dryRun,
		"engine_name":   engineName,
		"removed_count": removedCount,
		"skipped_count": skippedCount,
		"candidates":    candidates,
		"note":          "mux_prune_engines only removes stale/invalid registry descriptor files; it never stops processes or native muxcore daemons.",
	})
}

func (s *Server) toolMuxListForEngine(id json.RawMessage, engineName string, verbose, all bool) {
	engineName = strings.TrimSpace(engineName)
	records, err := registry.ListDescriptors(s.socketDir())
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("list muxcore engine registry: %v", err))
		return
	}
	var matches []registry.VerifiedDescriptor
	for _, rec := range records {
		if rec.Err != nil || rec.Descriptor.EngineName != engineName {
			continue
		}
		matches = append(matches, registry.VerifyDescriptor(rec))
	}
	if len(matches) == 0 {
		s.sendToolError(id, fmt.Sprintf("registered muxcore engine not found: %s", engineName))
		return
	}
	var healthy []registry.VerifiedDescriptor
	for _, match := range matches {
		if match.State == registry.StateHealthy {
			healthy = append(healthy, match)
		}
	}
	if len(healthy) == 0 {
		s.sendToolError(id, fmt.Sprintf("registered muxcore engine is not healthy: %s (%s)", engineName, registrySummary(matches)))
		return
	}
	if len(healthy) > 1 {
		s.sendToolError(id, fmt.Sprintf("registered muxcore engine is ambiguous: %s (%d healthy descriptors)", engineName, len(healthy)))
		return
	}
	rec := healthy[0].Record
	resp, err := control.Send(rec.Descriptor.DaemonControlPath, control.Request{Cmd: "list_owners"})
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("registered muxcore engine list_owners failed: %v", err))
		return
	}
	if !resp.OK {
		s.sendToolError(id, fmt.Sprintf("registered muxcore engine list_owners error: %s", resp.Message))
		return
	}
	if resp.Data == nil {
		s.sendToolError(id, "registered muxcore engine returned empty list_owners response")
		return
	}
	var listResp control.ListOwnersResponse
	if err := json.Unmarshal(resp.Data, &listResp); err != nil {
		s.sendToolError(id, fmt.Sprintf("registered muxcore engine returned invalid list_owners response: %v", err))
		return
	}
	for _, owner := range listResp.Owners {
		if owner.EngineName != "" && owner.EngineName != engineName {
			s.sendToolError(id, fmt.Sprintf("registered muxcore engine owner mismatch: requested %q, owner %q reports %q", engineName, owner.ServerID, owner.EngineName))
			return
		}
	}
	for i := range listResp.Owners {
		if listResp.Owners[i].EngineName == "" {
			listResp.Owners[i].EngineName = engineName
		}
	}
	myCwd, _ := os.Getwd()
	myCwd = normalizeCwd(myCwd)
	servers := s.formatOwnerList(listResp.Owners, verbose, all, myCwd)
	s.sendJSONToolResult(id, servers)
}

func (s *Server) formatOwnerList(owners []control.OwnerInfo, verbose, all bool, myCwd string) []map[string]any {
	var servers []map[string]any
	for _, owner := range owners {
		if !all && myCwd != "" {
			if !s.ownerInfoHasCwd(owner, myCwd) {
				continue
			}
		}

		var server map[string]any
		if verbose {
			server = map[string]any{
				"server_id":             owner.ServerID,
				"engine_name":           owner.EngineName,
				"command":               owner.Command,
				"args":                  owner.Args,
				"cwd":                   owner.Cwd,
				"cwd_set":               owner.CwdSet,
				"sessions":              owner.Sessions,
				"pending":               owner.Pending,
				"upstream_pid":          owner.UpstreamPID,
				"classification":        owner.Classification,
				"classification_source": owner.ClassificationSource,
				"classification_reason": owner.ClassificationReason,
				"mux_version":           owner.MuxVersion,
				"persistent":            owner.Persistent,
				"cached_init":           owner.CachedInit,
				"cached_tools":          owner.CachedTools,
				"cached_prompts":        owner.CachedPrompts,
				"cached_resources":      owner.CachedResources,
			}
		} else {
			server = map[string]any{
				"server_id":   owner.ServerID,
				"engine_name": owner.EngineName,
				"command":     owner.Command,
				"args":        owner.Args,
				"sessions":    owner.Sessions,
				"pending":     owner.Pending,
				"class":       owner.Classification,
				"version":     owner.MuxVersion,
			}
		}
		addOwnerPolicyFields(server, owner)
		if owner.Maintenance != nil {
			server["maintenance"] = owner.Maintenance
		}
		servers = append(servers, server)
	}
	if servers == nil {
		return []map[string]any{}
	}
	return servers
}

func addOwnerPolicyFields(server map[string]any, owner control.OwnerInfo) {
	if owner.ProtocolEra != "" {
		server["protocol_era"] = owner.ProtocolEra
	}
	if owner.SharingPolicy != "" {
		server["sharing_policy"] = owner.SharingPolicy
	}
	if owner.CachePolicy != "" {
		server["cache_policy"] = owner.CachePolicy
	}
	if owner.LifecyclePolicy != "" {
		server["lifecycle_policy"] = owner.LifecyclePolicy
	}
}

func registrySummary(matches []registry.VerifiedDescriptor) string {
	parts := make([]string, 0, len(matches))
	for _, match := range matches {
		reason := match.Reason
		if reason == "" {
			reason = match.State
		}
		parts = append(parts, fmt.Sprintf("%s: %s", match.State, reason))
	}
	return strings.Join(parts, "; ")
}

// normalizeCwd cleans a path and lowercases only on Windows. Linux/macOS
// filesystems are case-sensitive — lowercasing there would collapse `/Repo`
// and `/repo` into one project namespace and let resolveOwner pick a foreign
// owner. Empty input → empty output (callers treat empty as "no filter").
func normalizeCwd(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// ownerInfoHasCwd checks if an OwnerInfo matches a given cwd. Caller passes the
// already-normalized cwd; this helper normalizes the OwnerInfo paths to the
// same convention before comparing.
func (s *Server) ownerInfoHasCwd(info control.OwnerInfo, cwd string) bool {
	if cwd == "" {
		return false
	}
	if normalizeCwd(info.Cwd) == cwd {
		return true
	}
	for _, c := range info.CwdSet {
		if normalizeCwd(c) == cwd {
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
	if err != nil {
		return control.OwnerInfo{}, fmt.Errorf("mcp-mux daemon not reachable: %v", err)
	}
	if !resp.OK {
		return control.OwnerInfo{}, fmt.Errorf("mcp-mux daemon error: %s", resp.Message)
	}
	if resp.Data == nil {
		return control.OwnerInfo{}, fmt.Errorf("mcp-mux daemon returned empty list_owners response")
	}
	var listResp control.ListOwnersResponse
	if err := json.Unmarshal(resp.Data, &listResp); err != nil {
		return control.OwnerInfo{}, fmt.Errorf("invalid list_owners response: %v", err)
	}

	if serverID != "" {
		// Accept full server_id OR the 8-char shorthand documented in mux-guide.
		// Exact match wins; otherwise prefix match resolves uniquely or rejects
		// as ambiguous.
		var prefixMatches []control.OwnerInfo
		for _, owner := range listResp.Owners {
			if owner.ServerID == serverID {
				return owner, nil
			}
			if strings.HasPrefix(owner.ServerID, serverID) {
				prefixMatches = append(prefixMatches, owner)
			}
		}
		if len(prefixMatches) == 1 {
			return prefixMatches[0], nil
		}
		if len(prefixMatches) > 1 {
			return control.OwnerInfo{}, fmt.Errorf("server_id %s is ambiguous — use the full id", serverID)
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

	// Project-scoping: prefer matches that belong to this session's cwd. Restored
	// from pre-T011 resolveServerID — the regression was flagged by Gemini on PR #105.
	myCwd, _ := os.Getwd()
	myCwd = normalizeCwd(myCwd)
	var myCwdMatches []control.OwnerInfo
	for _, m := range matches {
		if s.ownerInfoHasCwd(m, myCwd) {
			myCwdMatches = append(myCwdMatches, m)
		}
	}
	if len(myCwdMatches) == 1 {
		return myCwdMatches[0], nil
	}
	if len(myCwdMatches) > 1 {
		return control.OwnerInfo{}, fmt.Errorf("'%s' matches %d servers in this project — use server_id", name, len(myCwdMatches))
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return control.OwnerInfo{}, fmt.Errorf("'%s' matches %d servers (none in this project) — be more specific or use server_id", name, len(matches))
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

	drainMs := 30000
	timeout := 35 * time.Second
	if params.Force {
		drainMs = 0
		timeout = 5 * time.Second
	}

	resp, err := s.stopOwner(owner, drainMs, timeout, params.Force || (owner.Sessions == 0 && owner.Pending == 0))
	if err != nil {
		s.sendControlToolError(id, err)
		return
	}

	s.sendToolResult(id, resp.Message)
}

func (s *Server) stopOwner(owner control.OwnerInfo, drainMs int, timeout time.Duration, preferDaemon bool) (*control.Response, error) {
	if preferDaemon {
		resp, err := control.SendWithTimeout(s.daemonCtlPath(), control.Request{
			Cmd:            "stop_owner",
			ServerID:       owner.ServerID,
			Command:        owner.ServerID,
			DrainTimeoutMs: drainMs,
		}, timeout)
		if err == nil {
			err = resp.Err()
		}
		if err == nil {
			return resp, nil
		}
		var maintenanceErr *control.MaintenanceError
		if errors.As(err, &maintenanceErr) {
			return resp, err
		}
		if resp != nil && !stopOwnerUnsupported(resp.Message) {
			return resp, err
		}
	}

	ctlPath := serverid.ControlPath(s.socketDir(), s.engineName(), owner.ServerID)
	resp, err := control.SendWithTimeout(ctlPath, control.Request{
		Cmd:            "shutdown",
		DrainTimeoutMs: drainMs,
	}, timeout)
	if err != nil {
		return resp, err
	}
	return resp, resp.Err()
}

func stopOwnerUnsupported(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "unknown command: stop_owner") ||
		strings.Contains(lower, "stop_owner not supported")
}

// toolMuxRestart delegates replacement to the daemon's retained launch authority.
func (s *Server) toolMuxRestart(id json.RawMessage, args json.RawMessage) {
	var params struct {
		ServerID string `json:"server_id"`
		Name     string `json:"name"`
		Force    bool   `json:"force"`
	}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		s.sendControlToolError(id, control.ErrMaintenanceInvalid)
		return
	}

	owner, err := s.resolveOwner(params.ServerID, params.Name)
	if err != nil {
		s.sendToolError(id, err.Error())
		return
	}

	// Force changes drain grace, never maintenance admission.
	drainMs := 30000
	if params.Force {
		drainMs = 0
	}

	resp, err := control.SendWithTimeout(s.daemonCtlPath(), control.Request{
		Cmd: "restart_owner", ServerID: owner.ServerID, DrainTimeoutMs: drainMs,
	}, 0)
	if err == nil {
		err = resp.Err()
		if err != nil && resp != nil && resp.ErrorCode == "" && !errors.Is(err, control.ErrMaintenanceInvalid) {
			err = control.ErrMaintenanceUnsupported
		}
	}
	if err != nil {
		s.sendControlToolError(id, err)
		return
	}
	if resp.ServerID == "" || resp.IPCPath == "" || resp.Token == "" || resp.ProtocolEra != owner.ProtocolEra {
		s.sendControlToolError(id, control.ErrMaintenanceInvalid)
		return
	}
	s.sendJSONToolResult(id, map[string]any{"ok": true, "server_id": resp.ServerID, "protocol_era": resp.ProtocolEra})
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

func (s *Server) sendJSONToolResult(id json.RawMessage, payload any) {
	result, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		s.sendToolError(id, fmt.Sprintf("internal: marshal tool result: %v", err))
		return
	}
	s.sendToolResult(id, string(result))
}

func (s *Server) sendToolError(id json.RawMessage, text string) {
	s.sendResult(id, map[string]any{
		"isError": true,
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
}
