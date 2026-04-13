# mcp-mux Server Protocol Specification

## Status: Draft v1.1
## Date: 2026-03-16
## MCP Protocol Version: 2025-11-25 (verified via Context7)

## Problem

mcp-mux needs to know whether an MCP server is safe to share across sessions.
Currently this is determined by:
1. Manual `--isolated` flag (user burden)
2. Tool name pattern matching (heuristic, false negatives possible)

Neither approach is reliable or zero-config. Servers themselves know best whether they maintain per-session state.

## Design Principles

- **MCP spec compliant** — use only documented extension points
- **Backward compatible** — servers without support work as before
- **Forward compatible** — align with MCP session identity when it arrives
- **Layered** — each level adds capability without requiring previous levels

## Three Levels

### Level 1: Capability Declaration (`x-mux`)

Server declares its sharing mode in the `initialize` response via custom capability.

**Server response:**
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "protocolVersion": "2025-06-18",
    "capabilities": {
      "tools": {},
      "x-mux": {
        "sharing": "shared"
      }
    },
    "serverInfo": {
      "name": "my-server",
      "version": "1.0.0"
    }
  }
}
```

**Schema:**
```
x-mux: {
  sharing: "shared" | "isolated" | "session-aware"   // REQUIRED
  stateless?: boolean                                  // default: false
  cacheableTools?: boolean                             // default: true
  cacheableInit?: boolean                              // default: true
}
```

**Values:**
| Value | Meaning |
|-------|---------|
| `shared` | Stateless or globally shared state. Safe to multiplex. One upstream serves all clients. |
| `isolated` | Per-session state that cannot be partitioned. Each client needs its own upstream. |
| `session-aware` | Server supports Level 2 session identity. Can multiplex stateful sessions over one connection. |

**Optional fields:**
- `stateless: true` — server doesn't depend on cwd, env, or any caller context. Allows global dedup (same upstream for all cwds).
- `cacheableTools: false` — tools/list response changes per session (rare). Disables tools/list caching.
- `cacheableInit: false` — initialize response varies (rare). Disables initialize caching.

### Level 2: Session Identity via `_meta`

mcp-mux injects a session identifier into every request forwarded to the upstream.
This enables stateful servers to isolate state per logical session while sharing a single upstream process.

**Injection by mcp-mux (downstream → upstream):**
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

**Rules:**
- `_meta.muxSessionId` is a unique opaque string per downstream client
- Stable for the lifetime of the downstream connection
- Format: `sess_` prefix + 8 random hex chars (e.g., `sess_a1b2c3d4`)
- Injected into ALL requests (initialize, tools/list, tools/call, etc.)
- NOT injected into notifications (they have no params.id context)

**Server behavior:**
- Servers declaring `"sharing": "session-aware"` MUST use `_meta.muxSessionId` to partition state
- Servers declaring `"sharing": "shared"` MAY ignore it
- Servers declaring `"sharing": "isolated"` will never see it (they get their own upstream)
- Servers without x-mux capability will never see it (legacy behavior preserved)

**Why `_meta`:** MCP spec uses `_meta` as a reserved namespace for protocol-level metadata
(precedent: `_meta.progressToken`). Unknown `_meta` fields are ignored by compliant servers,
making this fully backward compatible.

**Key naming:** Use flat vendor-prefixed key `_meta.muxSessionId` (not nested `_meta.mux.sessionId`).
MCP `_meta` convention uses flat keys. Avoid `mcp` prefix (reserved by spec).
Consider domain-scoped alternative: `_meta["bitswan.space/muxSessionId"]` for zero collision risk.

### Level 3: Native MCP Session Identity (Future)

MCP is moving toward native session identity support for all transports, including STDIO.
Ref: [MCP Transport Future](https://modelcontextprotocol.info/blog/transport-future)

**Migration path:**
1. When MCP adds native session identity to STDIO, mcp-mux will use the standard mechanism
2. `_meta.muxSessionId` becomes unnecessary — servers can read the native session ID
3. `x-mux.sharing: "session-aware"` maps to native session support
4. Transition period: mcp-mux sends BOTH `_meta.muxSessionId` and native session ID
5. Deprecation: after ecosystem adoption, `_meta.muxSessionId` is removed

**No server changes needed at Level 3** — the native mechanism replaces our injection transparently.

## Mode Detection Priority Chain

mcp-mux determines the sharing mode using this priority (highest first):

```
1. MCP_MUX_ISOLATED=1 env var           → isolated (user override)
2. --isolated CLI flag                    → isolated (user override)
3. x-mux.sharing capability              → as declared (server authority)
4. Tool name pattern classification       → heuristic (convention-based)
5. Default                                → shared
```

When `x-mux.sharing` is `"session-aware"`:
- mcp-mux operates in shared mode (one upstream)
- Injects `_meta.muxSessionId` into all requests
- Server handles state isolation internally

## Server Implementation Guide

### Minimal (Level 1 only)

Add `x-mux` to your server's initialize response capabilities:

**Python (FastMCP):**
```python
@server.init()
async def initialize():
    return {
        "capabilities": {
            "tools": {},
            "x-mux": {"sharing": "shared"}
        }
    }
```

**TypeScript (MCP SDK):**
```typescript
server.setRequestHandler(InitializeRequestSchema, async () => ({
  protocolVersion: "2025-06-18",
  capabilities: {
    tools: {},
    "x-mux": { sharing: "shared" },
  },
  serverInfo: { name: "my-server", version: "1.0.0" },
}));
```

**Go:**
```go
capabilities["x-mux"] = map[string]any{
    "sharing": "shared",
}
```

### Full (Level 1 + Level 2)

For stateful servers that want to benefit from shared upstream:

1. Declare `"sharing": "session-aware"` in capabilities
2. Read `_meta.muxSessionId` from every request
3. Use it as the key for state isolation

**Example: session-aware state management**
```typescript
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
  // ... use session-specific state
});
```

### Stateless servers

```json
{
  "x-mux": {
    "sharing": "shared",
    "stateless": true
  }
}
```

Stateless servers (search APIs, LLM proxies, doc lookup) should declare `stateless: true`.
This tells mcp-mux the server doesn't depend on cwd, enabling global deduplication
(one upstream instance regardless of which directory the client runs from).

## Tool Naming Convention (Soft Requirement)

Servers SHOULD follow these naming conventions to support heuristic classification
as a fallback when `x-mux` capability is absent:

**Isolation-indicating prefixes:** `browser_`, `session_`, `editor_`, `navigate`, `page_`, `tab_`
**Isolation-indicating substrings:** `_process`, `_document`, `_editor_`, `snapshot`

Servers with tools matching these patterns will be auto-classified as `isolated` in the absence of `x-mux`.

If your server is safe to share despite having such tool names, declare `x-mux.sharing: "shared"` explicitly to override the heuristic.

## Testing

Servers implementing this spec should verify:

1. `mcp-mux status` shows `auto_classification` matching the declared mode
2. With `x-mux.sharing: "shared"`: multiple CC sessions share one upstream process
3. With `x-mux.sharing: "isolated"`: each CC session gets its own upstream
4. With `x-mux.sharing: "session-aware"`: shared upstream, requests contain `_meta.muxSessionId`
5. Without `x-mux`: tool name heuristic produces correct classification

## Compatibility Matrix

| Server has x-mux? | Server is session-aware? | mcp-mux behavior |
|--------------------|------------------------|-------------------|
| No | N/A | Fall back to tool name classification |
| Yes, sharing=shared | No | Shared mode, no session injection |
| Yes, sharing=isolated | No | Isolated mode (own upstream) |
| Yes, sharing=session-aware | Yes | Shared mode + _meta.muxSessionId injection |

## MCP Protocol Coverage

### Gap Analysis (MCP 2025-11-25)

#### Client → Server Requests

| Method | Status | Issue |
|--------|--------|-------|
| `initialize` | ✅ cached + replay + fingerprint | — |
| `tools/list` | ✅ cached + replay + classify | — |
| `tools/call` | ✅ forwarded | — |
| `tools/call` (task-augmented) | ✅ forwarded | `task` field in params preserved through forwarding |
| `ping` | ✅ forwarded | — |
| `prompts/list` | ✅ cached + replay | — |
| `prompts/get` | ✅ forwarded | — |
| `resources/list` | ✅ cached + replay | — |
| `resources/read` | ✅ forwarded | — |
| `resources/templates/list` | ✅ cached + replay | — |
| `resources/subscribe` | ⚠️ forwarded | Subscription not per-session |
| `tasks/list` | ✅ forwarded | Generic forwarding via ID remap works |
| `tasks/get` | ✅ forwarded | — |
| `tasks/cancel` | ✅ forwarded | — |
| `tasks/result` | ✅ forwarded | Long-poll safe (readUpstream goroutine handles async) |
| `resources/unsubscribe` | ⚠️ forwarded | See above |
| `logging/setLevel` | ⚠️ forwarded | Affects all sessions (one upstream) |
| `completion/complete` | ✅ forwarded | — |

#### Server → Client Requests

| Method | Status | Issue |
|--------|--------|-------|
| `roots/list` | ✅ handled | Responds with owner's cwd (but only owner's — see below) |
| `sampling/createMessage` | ✅ routed | Via lastActiveSessionID |
| `elicitation/create` | ✅ routed | Cancel fallback when no session |
| `ping` (server→client) | ✅ handled locally | Empty result response |

**`sampling/createMessage` + `elicitation/create` problem:** Server sends these as requests
expecting a response from the client. mcp-mux logs "unhandled request" and drops them.
Any server using sampling or elicitation is completely broken through mcp-mux.

**`elicitation/create` difference:** When target session is unavailable, respond with
`{action: "cancel"}` (not error) to let the server proceed cleanly.

**Fix:** Track `lastActiveSessionID` when forwarding requests to upstream. Route
server→client requests to that session. For `ping`: respond locally with empty result.

**Caveat:** `lastActiveSession` is a heuristic. STDIO servers typically process requests
sequentially, making it deterministic. If multiple sessions have concurrent in-flight
requests, routing is best-effort. Document this limitation.

**`roots/list` architectural issue:** A shared upstream serving N sessions in N different
directories sees only the owner's cwd. Late-joining sessions don't transmit their cwd.
Roots-dependent servers (Serena, etc.) only index the owner's directory — wrong for others.
Fix options: IPC handshake to collect cwds, or force roots-dependent servers to isolated mode.

#### Server → Client Notifications

| Method | Status | Issue |
|--------|--------|-------|
| `notifications/message` | ✅ broadcast | — |
| `notifications/progress` | ⚠️ broadcast | Should route to session that owns the progressToken |
| `notifications/tools/list_changed` | ⚠️ broadcast only | **Cache not invalidated** |
| `notifications/prompts/list_changed` | ⚠️ broadcast only | **Cache not invalidated** |
| `notifications/resources/list_changed` | ⚠️ broadcast only | **Cache not invalidated** |
| `notifications/resources/updated` | ⚠️ broadcast | Should go to subscribing sessions only |
| `notifications/cancelled` | ❌ **ID NOT REMAPPED** | **P1 — forwarded with original ID, upstream ignores** |
| `notifications/initialized` | ⚠️ over-forwarded | **P1 — late-joining cached clients send extra to upstream** |

#### Client → Server Notifications (New Gaps)

**`notifications/cancelled` bug:** When downstream sends `notifications/cancelled` with
`requestId: 42`, it's forwarded as-is. But upstream knows the request by its remapped ID
(e.g., `"s3:n:42"`). The cancellation references an ID upstream doesn't recognize → silently
ineffective. Fix: intercept, remap `params.requestId`, then forward.

**`notifications/initialized` over-forwarding:** When a client receives a cached initialize
replay (not from upstream), it still sends `notifications/initialized` upstream. Upstream
gets one per downstream client instead of exactly one per connection. Fix: suppress this
notification from sessions that received cached (replayed) initialize responses.

### Severity Classification

**P0 — Broken functionality:** ALL FIXED ✅
- ~~`sampling/createMessage` dropped~~ → routed via lastActiveSessionID
- ~~`elicitation/create` dropped~~ → routed with cancel fallback

**P1 — Incorrect behavior:** ALL FIXED ✅ (except Tasks API — new)
- ~~`notifications/cancelled` ID not remapped~~ → remapped before forwarding
- ~~`notifications/initialized` over-forwarded~~ → suppressed for cached-replay sessions
- ~~`ping` (server→client) dropped~~ → handled locally
- ~~Initialize fingerprint mismatch~~ → protocolVersion validated before replay
- ~~`prompts/list`, `resources/list`, `resources/templates/list` not cached~~ → cached + replay
- ~~Cache invalidation missing~~ → invalidate on `*_changed` notifications

**P1 — Remaining (MCP 2025-11-25):**
- Tasks API — works via generic forwarding (no special handling needed)
- Task-augmented `tools/call` — `task` field preserved through forwarding
- `resources/subscribe` — broadcast leaks resource URIs across sessions (minor)

**P2 — Imprecise routing:**
- `notifications/progress` broadcast instead of targeted to progressToken owner
- `logging/setLevel` affects all sessions
- `roots/list` returns only owner's cwd — wrong for multi-cwd shared sessions

## Changes Required in mcp-mux

### From x-mux Protocol (this spec)

| Component | Change |
|-----------|--------|
| `internal/mux/owner.go` | Parse `x-mux` from cached initResp, use in mode detection |
| `internal/mux/owner.go` | Inject `_meta.muxSessionId` for session-aware servers |
| `internal/classify/classify.go` | Add `ClassifyCapabilities(initJSON)` function |
| `cmd/mcp-mux/main.go` | Defer mode decision until after first initialize (for non-flag cases) |

### From Gap Analysis (P0)

| Component | Change |
|-----------|--------|
| `internal/mux/owner.go` | Track `lastActiveSessionID` when forwarding requests |
| `internal/mux/owner.go` | Route `sampling/createMessage` to lastActiveSession |

### From Gap Analysis (P1)

| Component | Change |
|-----------|--------|
| `internal/mux/owner.go` | Add `promptList`, `resourceList`, `resourceTemplateList` cache fields |
| `internal/mux/owner.go` | Tag + cache responses for `prompts/list`, `resources/list`, `resources/templates/list` |
| `internal/mux/owner.go` | Replay from cache for new clients (same as initialize/tools/list) |
| `internal/mux/owner.go` | In broadcast(), check for `*_changed` notifications and invalidate corresponding cache |

## Cacheable Responses

mcp-mux caches these responses from the first client and replays them for subsequent clients:

| Method | Cached | Cache Key | Invalidated By |
|--------|--------|-----------|----------------|
| `initialize` | ✅ Yes | `initResp` | Never (server restarts = new upstream) |
| `tools/list` | ✅ Yes | `toolList` | `notifications/tools/list_changed` |
| `prompts/list` | Planned | `promptList` | `notifications/prompts/list_changed` |
| `resources/list` | Planned | `resourceList` | `notifications/resources/list_changed` |
| `resources/templates/list` | Planned | `resourceTemplateList` | `notifications/resources/list_changed` |

**Cache invalidation:** When the server sends a `*_changed` notification, the
corresponding cache entry is cleared AND the notification is broadcast to all sessions.
The next request for that method goes to upstream and re-populates the cache.

## Open Questions

1. Should `_meta.muxSessionId` be injected into `initialize` itself, or only subsequent requests?
   (Initialize has no `_meta` convention yet in MCP spec)
2. Should session-aware servers send cleanup notifications when a session disconnects?
3. Should mcp-mux forward `notifications/session/ended` (custom) when a downstream disconnects?
4. Should `logging/setLevel` be intercepted and tracked per-session?
   (Would require mcp-mux to send the "highest" log level to upstream)
5. Should `resources/subscribe` track subscriptions per-session and only forward
   `resources/updated` to subscribing sessions?
