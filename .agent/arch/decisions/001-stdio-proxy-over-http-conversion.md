# ADR-001: Stdio Proxy Over HTTP Conversion

## Status

Accepted

## Date

2026-03-14

## Context

Multiple Claude Code sessions running in parallel each spawn their own copy of every configured MCP server (stdio transport). With 4 sessions × 12 servers = ~48 processes consuming ~4.8 GB RAM. We need to reduce this overhead.

Two approaches were considered:

1. **Convert stdio servers to HTTP transport** — rewrite or wrap each MCP server to serve HTTP, then CC sessions share a single HTTP endpoint
2. **Stdio proxy/multiplexer** — keep servers as-is (stdio), insert a proxy that multiplexes requests from multiple clients to shared server instances

## Decision

We chose **stdio proxy/multiplexer** (option 2).

## Rationale

- Most MCP servers only support stdio — converting to HTTP requires per-server changes or wrappers
- The MCP spec is moving toward Streamable HTTP for remote, but stdio remains the standard for local
- A proxy preserves the existing server ecosystem — no forks, no patches, no maintenance burden
- The proxy is a single component to build and maintain vs N server-specific HTTP wrappers
- CC already knows how to connect via stdio — no client-side changes needed
- The proxy can be used with ANY stdio MCP server, not just ours

## Consequences

- Need to handle JSON-RPC id remapping (moderate complexity)
- Need named pipes (Windows) or Unix sockets for downstream connections
- Stateful servers (SSH, browser) still need per-session isolation (handled via `isolated` mode)
- Adds ~1ms latency per request (acceptable for MCP operations)
