# mcp-mux Server Protocol

**Document version:** 1.1
**Legacy MCP reference:** 2025-11-25
**Native R1 route:** MCP 2026-07-28
**Updated:** 2026-08-31

## Overview

mcp-mux is a transparent STDIO multiplexer for MCP servers. Multiple Claude Code sessions
share a single upstream MCP server process, reducing memory usage by ~3x.

This document describes how MCP servers can declare their multiplexing compatibility
for zero-config operation with mcp-mux.

### Version scope

The legacy reference from **Quick Start** through **Migration Path** documents the MCP `2025-11-25` route. Its `x-mux` capability, sharing modes, session metadata, cache, and replay rules do not select or describe the native R1 route.

The **MCP 2026-07-28 R1 native route** section is authoritative for `--mcp-protocol=2026-07-28`. That route is additive and native-only. When callers do not select it, mcp-mux keeps the legacy route.

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
      "muxSessionId": "sess_a1b2c3d4",
      "muxCwd": "D:\\Dev\\novascript",
      "muxEnv": { "GITHUB_TOKEN": "ghp_..." }
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

### Per-Session Working Directory (`_meta.muxCwd`)

- The CC session's project directory (where Claude Code was launched)
- Allows session-aware servers with `--project-from-cwd` to scope per-request
- Works with git worktrees — each worktree session gets its own cwd
- Only injected when the session has a bound cwd (daemon mode with token handshake)

### Per-Session Environment (`_meta.muxEnv`)

- Environment variable diff between the session and the owner
- Allows session-aware servers to use project-scope credentials
- Only injected when the session has bound env vars

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

> **Legacy-only behavior:** This table describes the MCP `2025-11-25` route. It does not apply to the native R1 route, which does not use legacy bootstrap, cache, template, or replay behavior.

## Migration Path

When MCP adds native session identity to STDIO transport:

1. mcp-mux will use the standard mechanism instead of `_meta.muxSessionId`
2. Transition period: both `_meta.muxSessionId` and native ID sent
3. Servers remove `_meta` handling once native support is universal
4. `x-mux` capability becomes unnecessary

No breaking changes — backward compatible at every step.

## MCP 2026-07-28 R1 native route

This section is authoritative for the additive R1 route. Use it only for a known MCP `2026-07-28` host and a same-era upstream. R1 forwards native frames and does not translate legacy initialization, methods, results, errors, or callbacks.

### Select a protocol era, not a sharing mode

| Input | R1 meaning |
|-------|------------|
| `--mcp-protocol=2026-07-28` | Selects the CLI native route. |
| `engine.Config.ProtocolPolicy = era.PolicyModern20260728` | Selects the library native route. |
| `era.PolicyLegacyOnly` | The zero-value policy. It preserves existing legacy ingress. |
| `MCP_MUX_ISOLATED`, `--isolated`, `MCP_MUX_STATELESS`, `--stateless`, and `x-mux.sharing` | Existing sharing inputs. They never select an MCP protocol era. |

The native route requires daemon control routing. It selects an immutable `era.ProtocolEra` before owner election and accepts a modern attachment only after `control.Response.ProtocolEra` echoes exactly `2026-07-28`. `control.Request.ProtocolEra`, `owner.OwnerConfig.ProtocolEra`, and `owner.ResilientClientConfig.ProtocolEra` carry that selected era through admission and reconnect.

R1 always creates one `forced-isolated` modern owner. Historical sharing inputs, command similarity, working directory, environment, and an existing legacy owner never authorize modern sharing or reuse.

`clientInfo`, discovery responses, upstream names, and failed modern attempts also never select an era.

### Admit one native opening request

The first host frame must be one newline-delimited JSON-RPC request. Its `params._meta.io.modelcontextprotocol/protocolVersion` must be `2026-07-28`, and its `params._meta.io.modelcontextprotocol/clientCapabilities` must be an object. `params._meta.io.modelcontextprotocol/clientInfo` is optional. If present, it has string `name` and `version` values.

After the exact control echo, mcp-mux forwards the opening frame byte-for-byte to the same-era upstream. A host-sent `server/discover` request is valid and reaches the upstream unchanged. mcp-mux never uses it as an injected handshake. A malformed opening, an unsupported version, a missing or mismatched control echo, or a mixed-era target is refused. R1 does not automatically fall back to legacy.

### Keep modern traffic native

An R1 modern owner sends no mux-generated legacy `initialize`, `notifications/initialized`, roots/list, list-change, cache, template, or replay traffic. Response caching, template reuse, cache invalidation, and reconnect replay are off. A modern upstream JSON-RPC request is contained and never reaches the downstream host.

After a request opts into `io.modelcontextprotocol/logLevel`, a valid request-scoped standard log reaches the sole recipient once. mcp-mux does not synthesize or broadcast the log. Opaque multi-round-trip request state remains native data. A host-issued retry is a new request with a new JSON-RPC ID.

### Quarantine lifecycle transitions

R1 keeps the existing daemon and owner authority. `OwnerGeneration`, exact-current-process and in-flight fences, stale-event rejection, bounded retries, reaper and zero-session gates, and `upstream.Process.RetirementProven()` remain in charge. R1 adds no second daemon, reaper, spawn lock, replay loop, or cleanup authority.

`RetirementProven()` remains required before the owner retires process-tree authority or a replacement can proceed. If proof is unavailable, the exact owner remains authoritative and follows the existing finalization retry path. mcp-mux does not delete it or create a competing or legacy replacement.

The current snapshot and handoff payloads are era-less. R1 excludes a modern owner from those payloads instead of adding an R1 era field or schema version. The only safe outcomes are exact-era in-memory continuation, drain and removal, cold start after fresh explicit modern admission, or explicit refusal. No path restores or attaches a modern owner as legacy.

After daemon loss, upstream loss, or downstream reconnect, mcp-mux ends current modern work once and clears existing request, progress, and subscription state. If the daemon survives an upstream-generation loss, the next route requires fresh exact-era admission. The host sends fresh native traffic and a new `subscriptions/listen` request when needed. R1 does not replay requests, multi-round-trip retries, progress, subscriptions, cache state, or legacy bootstrap.

### Inspect and roll back an R1 owner

The exact control echo proves modern admission. Existing direct owner status and status/list projections that carry `control.OwnerInfo` report these facts for an active R1 owner:

```text
protocol_era = 2026-07-28
sharing_policy = forced-isolated
cache_policy = off
lifecycle_policy = r1-quarantine
```

Existing readiness fields retain their established meaning. R1 readback does not expose request bodies, opaque request state, credentials, tokens, raw environment values, authorization material, cache/template content, or route identifiers.

To roll back, stop new explicit-modern admissions and drain or remove existing modern owners through the existing lifecycle path. Rollback never converts live modern work to legacy or replays unfinished work.

R1 excludes modern sharing or reuse, shared causal correlation, semantic translation, automatic dual-era fallback, modern response caching or template reuse, persisted modern snapshot or handoff transfer, automatic subscription continuation, and new registry, topology, lifecycle-state, or counter readbacks.
