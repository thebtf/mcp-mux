# Session Transport Layer — Design Spec

## Problem

`lastActiveSessionID` is a global heuristic ("who wrote last"). It has no causal binding
to upstream callbacks. When N sessions share one upstream via dedup, server→client requests
(roots/list, sampling, elicitation) are routed based on this heuristic — which can be wrong
when sessions overlap.

## Solution: SessionManager + Request Correlation + Token Handshake

### SessionManager (new component)

Tracks session identity, metadata, and in-flight request correlation.

```go
type SessionManager struct {
    sessions map[int]*SessionContext  // session ID → context
    inflight map[string]int           // remapped request ID → session ID
    pending  map[string]string        // token → cwd (pre-registered by daemon)
    mu       sync.RWMutex
}

type SessionContext struct {
    Session *Session
    Cwd     string
}
```

**ResolveCallback** — replaces `lastActiveSessionID`:
- Collects unique sessions with in-flight requests
- 1 active session → deterministic (guaranteed correct)
- 0 active sessions → fallback
- N active sessions → most recent among in-flight (scoped ambiguity)

### Token Handshake (cwd binding via control plane)

1. Daemon.Spawn receives cwd from shim
2. Daemon generates token, calls `owner.sessionMgr.PreRegister(token, cwd)`
3. Daemon returns `{ipcPath, serverID, token}` to shim
4. Shim sends token as first line on IPC connect (before MCP traffic)
5. Owner reads token byte-by-byte, calls `sessionMgr.Bind(token, session)`
6. Session.Cwd = cwd from token lookup

### Response Strategy for server→client requests

| Callback | Strategy | Rationale |
|----------|----------|-----------|
| roots/list | Forward to resolved session | CC knows real roots; roots can change mid-session (MCP spec: listChanged) |
| sampling | Forward to resolved session | CC must process (LLM inference) |
| elicitation | Forward to resolved session | CC must show UI |
| ping | Answer locally | Session-independent |

Note: 44-minute hang during initial testing was caused by replayInit bufio over-read
(fixed in v0.3.3). Forward approach is confirmed working — CC responds to roots/list
in <1ms via TRACE verification.

### Fallback (no active session or forward fails)

roots/list: answer from Session.Cwd (token-bound) or cwdSet.
Others: return JSON-RPC error -32603.

## Changes Required

| File | What | LOC |
|------|------|-----|
| internal/mux/session_manager.go | NEW: SessionManager | ~80 |
| internal/mux/owner.go | Replace lastActiveSessionID with sessionMgr | ~30 |
| internal/mux/session.go | Add Cwd string | +1 |
| internal/control/protocol.go | Add Token to Response | +1 |
| internal/daemon/daemon.go | Generate token, PreRegister | ~10 |
| cmd/mcp-mux/main.go | Shim sends token on connect | ~5 |
| internal/mux/resilient_client.go | Token send on connect/reconnect | ~10 |
| Tests | session_manager_test.go + updates | ~100 |

## What This Removes

- lastActiveSessionID — fully replaced by inflight correlation
- cwdSet for roots routing — replaced by per-session Cwd (keep for diagnostics)
