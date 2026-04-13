# ADR-008: MCP-Aware Proxy with Roots Propagation

## Status

Accepted

## Date

2026-03-15

## Context

The original design treated mcp-mux as a "dumb" JSON-RPC byte proxy — it doesn't understand MCP protocol, just forwards messages with ID remapping.

This creates the cwd problem: multiple CC sessions in different projects share one upstream MCP server, but the server was spawned with the first client's cwd. Other clients get wrong project context.

Research (deep research + multi-model dialog + competitor analysis) showed:
- MCP spec has `roots` capability — clients declare filesystem boundaries to servers
- `roots/list` is dynamically changeable via `notifications/roots/list_changed`
- BUT roots are per-session, not per-request — no way to bind a specific root to a specific `tools/call`
- No existing project solves this for N:1 multiplexing (rmcp-mux, mcpman, mcpm.sh all punt on it)

The question "wrapper vs aggregator" is a false dichotomy. mcp-mux can be a wrapper by UX and MCP-aware proxy by protocol.

## Decision

mcp-mux is an **MCP-aware proxy**, not a dumb JSON-RPC forwarder.

It understands:
- `initialize` / `initialized` handshake — cache and replay
- `roots/list` — intercept upstream's request, respond with per-client roots
- `notifications/roots/list_changed` — send to upstream when client set changes
- `tools/list` — cache and replay
- All other messages — standard ID remap forwarding

### Two-tier server classification

**Stateless servers** (time, tavily, nia, openrouter, n8n, notion, wsl):
- ServerID = hash(command + args) — NO cwd in hash
- One instance globally shared across all projects
- Roots propagation: aggregate union of all client roots (informational only)

**cwd-dependent servers** (serena, cclsp, context-please):
- ServerID = hash(command + args + cwd) — cwd in hash
- Shared per-project: same cwd = share, different cwd = separate upstream
- Roots propagation: all clients have same root (same cwd), so no conflict

**Stateful/isolated servers** (aimux, playwright, desktop-commander, pencil):
- `--isolated` flag — always separate upstream per client
- No sharing, no roots propagation needed

### How server type is determined

Default: cwd-dependent (safest).
User can opt out per-server:
- `mcp-mux --stateless uvx mcp-server-time` → hash without cwd
- `mcp-mux --isolated npx playwright-mcp` → no sharing
- `mcp-mux uvx serena` → default, cwd in hash

Future: auto-detection based on server capabilities (if server doesn't declare roots capability → stateless).

## Rationale

- "Dumb proxy" leaves the core problem (cwd) unsolved — project is pointless
- MCP-aware proxy with per-project sharing gives **real** resource savings
- 4 CC sessions × 12 servers in same project → 12 processes instead of 48
- 4 CC sessions × 12 servers across 4 projects → 48 processes (no worse than without mux)
- Mixed (2 projects × 6 stateless + 6 cwd-dep each) → 6 + 12 = 18 instead of 48

## Consequences

- More complex proxy logic (MCP protocol awareness)
- Need to handle `roots/list` interception correctly
- Need to handle `initialize` caching with capability merging
- But: the wrapper UX remains identical — user just adds `mcp-mux` to command
- And: the cwd problem is solved for the real use case
