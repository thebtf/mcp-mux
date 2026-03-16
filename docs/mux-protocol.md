# mcp-mux Server Protocol

**Version:** 1.0
**MCP Protocol:** 2025-11-25
**Date:** 2026-03-16

## Overview

mcp-mux is a transparent STDIO multiplexer for MCP servers. Multiple Claude Code sessions
share a single upstream MCP server process, reducing memory usage by ~3x.

This document describes how MCP servers can declare their multiplexing compatibility
for zero-config operation with mcp-mux.

## Quick Start

Add `x-mux` to your server's initialize response capabilities:

```json
{
  "protocolVersion": "2025-11-25",
  "capabilities": {
    "tools": {},
    "x-mux": {
      "sharing": "shared"
    }
  },
  "serverInfo": { "name": "my-server", "version": "1.0.0" }
}
```

That's it. mcp-mux reads this capability and configures itself automatically.

## Sharing Modes

| Mode | Meaning | Use When |
|------|---------|----------|
| `shared` | One upstream serves all clients. No per-session state. | Stateless servers (search, LLM proxy, docs) |
| `isolated` | Each client gets its own upstream. | Per-session state (browser, editor, SSH) |
| `session-aware` | One upstream, but state partitioned by session ID. | Stateful servers that can isolate via session key |

### `shared` — Default, Stateless Servers

```json
{ "x-mux": { "sharing": "shared" } }
```

Requests from all sessions are multiplexed through one upstream process.
Ideal for servers where every request is independent.

**Optional:** Add `"stateless": true` to indicate the server doesn't depend on cwd,
enabling global deduplication (one instance regardless of working directory).

```json
{ "x-mux": { "sharing": "shared", "stateless": true } }
```

### `isolated` — Stateful Servers

```json
{ "x-mux": { "sharing": "isolated" } }
```

Each client gets a dedicated upstream process. Use this when the server maintains
state that cannot be partitioned (browser tabs, SSH connections, editor buffers).

### `session-aware` — Best of Both Worlds

```json
{ "x-mux": { "sharing": "session-aware" } }
```

One upstream process serves all clients, but mcp-mux injects a session identifier
into every request. The server uses this to partition state per session.

**Benefits:**
- Single process (low memory, ~3x savings)
- Full state isolation per session
- Future-proof (aligns with MCP session identity roadmap)

## Session Identity (`_meta.muxSessionId`)

When a server declares `session-aware`, mcp-mux injects a unique session ID
into every JSON-RPC request via the `_meta` field:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "_meta": {
      "muxSessionId": "sess_a1b2c3d4"
    },
    "name": "exec",
    "arguments": { "command": "echo hello" }
  }
}
```

### Session ID Properties

- Format: `sess_` + 8 hex chars (e.g., `sess_a1b2c3d4`)
- Unique per downstream client connection
- Stable for the connection lifetime
- Injected into ALL requests (not notifications)

### Why `_meta`

MCP spec reserves `_meta` for protocol-level metadata (precedent: `_meta.progressToken`).
Unknown `_meta` fields are ignored by compliant servers, making this fully backward compatible.

## Implementation Examples

### TypeScript (MCP SDK)

```typescript
// Initialize response
server.setRequestHandler(InitializeRequestSchema, async () => ({
  protocolVersion: "2025-11-25",
  capabilities: {
    tools: {},
    "x-mux": { sharing: "session-aware" },
  },
  serverInfo: { name: "my-server", version: "1.0.0" },
}));

// Session-aware state management
const sessions = new Map<string, SessionState>();

function getSession(params: any): SessionState {
  const sessionId = params?._meta?.muxSessionId ?? "default";
  if (!sessions.has(sessionId)) {
    sessions.set(sessionId, new SessionState());
  }
  return sessions.get(sessionId)!;
}

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const session = getSession(request.params);
  // Use session-specific state...
});
```

### Python (FastMCP)

```python
from mcp.server import Server

server = Server("my-server")

# Sessions keyed by muxSessionId
sessions: dict[str, dict] = {}

def get_session(params: dict) -> dict:
    session_id = params.get("_meta", {}).get("muxSessionId", "default")
    if session_id not in sessions:
        sessions[session_id] = {}
    return sessions[session_id]

@server.call_tool()
async def call_tool(name: str, arguments: dict, _meta: dict | None = None) -> list:
    session = get_session({"_meta": _meta or {}})
    # Use session-specific state...
```

### Go

```go
// In initialize response
capabilities["x-mux"] = map[string]any{
    "sharing": "session-aware",
}

// Extract session ID from request
func getSessionID(params json.RawMessage) string {
    var p struct {
        Meta struct {
            MuxSessionID string `json:"muxSessionId"`
        } `json:"_meta"`
    }
    if err := json.Unmarshal(params, &p); err != nil || p.Meta.MuxSessionID == "" {
        return "default"
    }
    return p.Meta.MuxSessionID
}
```

## Mode Detection Priority

mcp-mux determines the sharing mode using this priority (highest first):

```
1. MCP_MUX_ISOLATED=1 env var        → isolated (user override)
2. --isolated CLI flag                → isolated (user override)
3. x-mux.sharing capability          → as declared (server authority)
4. Tool name pattern classification   → heuristic (convention-based)
5. Default                            → shared
```

## Tool Naming Conventions

When `x-mux` capability is absent, mcp-mux classifies servers by tool names:

**Isolation-indicating prefixes:** `browser_`, `session_`, `editor_`, `navigate`, `page_`, `tab_`

**Isolation-indicating substrings:** `_process`, `_document`, `_editor_`, `snapshot`

If your server is safe to share despite having matching tool names,
declare `x-mux.sharing: "shared"` to override the heuristic.

## What mcp-mux Handles Transparently

| Feature | Status |
|---------|--------|
| `initialize` / `tools/list` / `prompts/list` / `resources/list` | Cached + replayed to new clients |
| `sampling/createMessage` / `elicitation/create` | Routed to active session |
| `notifications/cancelled` | Request ID remapped before forwarding |
| `notifications/initialized` | Suppressed for cached-replay sessions |
| Cache invalidation on `*_changed` | Automatic |
| Tasks API (`tasks/list`, `tasks/get`, etc.) | Generic forwarding (no special handling needed) |
| Server→client `ping` | Handled locally by mcp-mux |
| Initialize fingerprint (protocolVersion) | Validated before replaying |

## Migration Path

When MCP adds native session identity to STDIO transport:

1. mcp-mux will use the standard mechanism instead of `_meta.muxSessionId`
2. Transition period: both `_meta.muxSessionId` and native ID sent
3. Servers remove `_meta` handling once native support is universal
4. `x-mux` capability becomes unnecessary

No breaking changes — backward compatible at every step.
