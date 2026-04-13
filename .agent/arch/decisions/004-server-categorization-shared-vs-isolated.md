# ADR-004: Server Categorization — Shared vs Isolated (Revised)

## Status

Accepted (revised 2026-03-15)

## Date

2026-03-14 (original), 2026-03-15 (revised)

## Context

Not all MCP servers are safe to share across sessions. Servers that maintain per-session state (SSH connections, browser sessions, file locks) would produce incorrect behavior if requests from different sessions were interleaved.

The 2026-03-15 brainstorm added a key architectural principle: **one code path** for both modes. The proxy logic is identical — only the policy differs.

## Decision

Two modes per server, specified via CLI flag:

### `shared` (default)

- Single upstream process serves all downstream clients
- Requests multiplexed via id remapping
- First mcp-mux instance becomes owner, others connect as clients via IPC

### `isolated` (opt-in: `mcp-mux --isolated <command> [args...]`)

- Each mcp-mux instance spawns its own dedicated upstream
- No IPC coordination — each instance is its own owner
- Same proxy code path — policy is simply "don't share"

## One Code Path Principle

The proxy engine (JSON-RPC parse, id remap, routing, lifecycle management) is identical in both modes. The only difference:

| Aspect | Shared | Isolated |
|--------|--------|----------|
| Upstream instances | 1 per server identity | 1 per downstream client |
| IPC coordination | Yes (owner/client) | No (every instance is owner) |
| Resource savings | High | None (equivalent to no mux) |
| Benefit | Process deduplication | Centralized management, CLI monitoring |

## Categorization of Current Servers

| Server | Mode | Reason |
|--------|------|--------|
| engram | shared | Pure request/response, no session state |
| tavily | shared | Stateless web search |
| context7 | shared | Stateless doc lookup |
| litellm | shared | Stateless LLM proxy |
| context-please | shared | Stateless code search |
| proxmox | shared | Stateless API proxy |
| kubernetes | shared | Stateless kubectl proxy |
| k8sgpt | shared | Stateless cluster analysis |
| memory | shared | Stateless memory store/recall |
| openrouter | shared | Stateless model routing |
| nvmd-ssh | **isolated** | SSH connections are per-session |
| aimux | **isolated** | Session management, process state per client |
| playwright | **isolated** | Browser instance per session |
| desktop-commander | **isolated** | Process management per session |
| pencil | **isolated** | Editor state per session |
| serena | **isolated** | Project activation state per session |

## Consequences

- Operator specifies `--isolated` flag when needed — miscategorizing a stateful server as shared causes data corruption
- Default shared maximizes resource savings
- Isolated servers still benefit from CLI monitoring (`mcp-mux status`)
- Future: auto-detection of stateful servers (known list or heuristic)
