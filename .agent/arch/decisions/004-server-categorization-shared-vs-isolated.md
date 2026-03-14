# ADR-004: Server Categorization — Shared vs Isolated

## Status

Accepted

## Date

2026-03-14

## Context

Not all MCP servers are safe to share across sessions. Servers that maintain per-session state (SSH connections, browser sessions, file locks) would produce incorrect behavior if requests from different sessions were interleaved.

## Decision

Two modes per upstream server, configured explicitly:

### `shared` (default)

- Single upstream process serves all downstream clients
- Requests are multiplexed via id remapping
- Safe for: stateless request/response servers (engram, tavily, context7, litellm, proxmox, k8sgpt)

### `isolated`

- One upstream process spawned per downstream client
- No multiplexing — dedicated 1:1 pipe per session
- Required for: servers with per-session state (nvmd-ssh, playwright, aimux)

## Categorization of Current Servers

| Server | Mode | Reason |
|--------|------|--------|
| engram | shared | Pure request/response, no session state |
| tavily | shared | Stateless web search |
| context7 | shared | Stateless doc lookup |
| litellm | shared | Stateless LLM proxy |
| context-please | shared | Stateless code search |
| proxmox | shared | Stateless API proxy (REST calls to Proxmox) |
| kubernetes | shared | Stateless kubectl proxy |
| k8sgpt | shared | Stateless cluster analysis |
| memory | shared | Stateless memory store/recall |
| nvmd-ssh | **isolated** | SSH connections are per-session (session_id state) |
| aimux | **isolated** | Session management, process state per client |
| playwright | **isolated** | Browser instance per session |
| desktop-commander | **isolated** | Process management per session |
| pencil | **isolated** | Editor state per session |

## Rationale

- Explicit configuration avoids runtime errors from incorrectly sharing stateful servers
- Default `shared` maximizes resource savings (most servers are stateless)
- `isolated` mode degrades gracefully to current behavior (no multiplexing benefit, but no breakage)

## Consequences

- Operator must correctly categorize servers — miscategorizing a stateful server as `shared` causes data corruption
- `isolated` servers still benefit from centralized management (single config, health monitoring)
- Memory savings come primarily from `shared` servers — if most servers are `isolated`, benefit is minimal
