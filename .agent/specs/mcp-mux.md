# Specification: mcp-mux — MCP Stdio Multiplexer

## Overview

A local proxy that allows multiple MCP clients (Claude Code sessions) to share single instances of MCP servers. Eliminates the N×M process explosion when running N parallel sessions with M MCP servers.

## UX Model: Transparent Wrapper

mcp-mux works as a **command prefix** — the user changes one line in their MCP config:

```json
// Before:
{ "command": "uvx", "args": ["--refresh", "--from", "git+...", "serena", ...] }

// After:
{ "command": "mcp-mux", "args": ["uvx", "--refresh", "--from", "git+...", "serena", ...] }
```

mcp-mux takes `argv[1:]` as the full original command. Works with any package manager or runtime: `uvx`, `npx`, `node`, `python`, `go run`, etc.

The user decides per-server whether to route through mcp-mux or run directly. No separate config file for mux — server identity derived from command + args.

## Functional Requirements

### Core Proxy

- FR1: Accept downstream stdio from MCP client (CC), proxy to shared or dedicated upstream
- FR2: Maintain a single shared upstream process per unique server identity (command + args hash)
- FR3: Multiplex downstream requests to shared upstream with JSON-RPC id remapping
- FR4: Route responses back to the correct downstream client based on remapped id
- FR5: Handle MCP `initialize` handshake — first client triggers real init, subsequent clients receive cached `InitializeResult`
- FR6: Support server modes: `shared` (default, stateless) vs `isolated` (per-session state)
- FR7: Broadcast server-to-client notifications to all connected downstream clients (for shared servers)
- FR8: Handle client disconnect gracefully — clean up id mappings, do not terminate shared upstream
- FR9: Terminate upstream when last client disconnects (configurable: keep-alive or shutdown)

### Coordination

- FR10: First mcp-mux invocation for a given server becomes the "owner" — spawns the real upstream, listens on IPC
- FR11: Subsequent invocations detect existing owner via IPC (named pipe/socket), connect as clients
- FR12: Server identity = hash(command + args + selected env vars) — determines IPC endpoint name

### CLI Management

- FR13: `mcp-mux status` — active sessions, upstream processes, memory usage
- FR14: `mcp-mux ps` — per-process detail (pending requests, last activity)
- FR15: `mcp-mux restart <server>` — restart a hung upstream
- FR16: `mcp-mux setup` — scan .mcp.json / .claude.json, offer to wrap servers, rewrite config (project/user scope)
- FR17: Health detection — identify hung processes (no response within timeout, no activity)
- FR18: Diagnostics — what a process is doing (pending request count, last request time)

### Security

- FR19: cwd sandbox — client passes its cwd, upstream operates strictly within it
- FR20: Path traversal prevention — block `../` escapes beyond cwd
- FR21: Symlink resolution — verify resolved path stays within cwd boundary
- FR22: Each downstream client can have its own cwd (different projects)

## Non-Functional Requirements

- NFR1: Latency overhead < 1ms per proxied request (JSON parse + id remap + route)
- NFR2: Memory footprint of mux process itself < 20 MB (Go binary)
- NFR3: Support at least 8 simultaneous downstream clients
- NFR4: Cross-platform: Windows (primary), Linux, macOS
- NFR5: Single static binary, zero runtime dependencies
- NFR6: Startup time < 100ms (excluding upstream server spawn time)

## Architecture Principles

### One Code Path (shared + isolated)

The proxy logic (id remap, routing, lifecycle) is identical for shared and isolated modes. The only difference is **policy**: how many upstream instances per server.

- Shared: 1 upstream, N downstream → mux routes
- Isolated: N upstreams (1:1 with downstream) → same mux code, policy = "don't share"

### Transport Abstraction (future)

MVP: stdio → stdio only.

Future: pluggable transport adapters for any-to-any conversion:
- stdio → Streamable HTTP
- Streamable HTTP → stdio
- Streamable HTTP → Streamable HTTP

Internal representation is transport-agnostic JSON-RPC messages.

## Tech Stack

- **Language**: Go
- **Rationale**: Single binary (no runtime deps), goroutines ideal for per-connection concurrency, excellent process/stream management (`os/exec`, `io.Pipe`, `net`), fast cross-compilation
- **MCP protocol**: 2025-06-18 (JSON-RPC 2.0 over stdio)

## Acceptance Criteria

- [ ] AC1: 4 CC sessions connect via mcp-mux wrapper, each sees full tool list
- [ ] AC2: Request from Session 1 returns only to Session 1
- [ ] AC3: Two sessions with same JSON-RPC id=1 both get correct responses
- [ ] AC4: Upstream spawned only once — second mcp-mux instance reuses via IPC
- [ ] AC5: Client disconnect does not crash mux or affect other clients
- [ ] AC6: `isolated` mode spawns separate upstream per client
- [ ] AC7: Memory usage with 4 clients × 6 shared upstreams < 200 MB total
- [ ] AC8: Works on Windows with named pipes
- [ ] AC9: `mcp-mux status` shows correct session/process info
- [ ] AC10: cwd sandbox prevents path traversal

## Out of Scope (MVP)

- HTTP/SSE MCP transport (future: transport adapters)
- Remote workers (research needed — see Planning Features)
- Web dashboard (CLI monitoring sufficient)
- Auto-discovery / onboarding from multiple CLI configs (future)
- Authentication between clients and mux (local-only, trusted)

## Planning Features (post-MVP)

### Onboarding & Multi-CLI Support

- `mcp-mux setup` scans MCP configs from all known CLI locations (Claude, Codex, Gemini, Cursor, etc.)
- Each CLI has different config format — mux must read/write all formats
- MVP: Claude Code (.mcp.json, ~/.claude.json) only, others added incrementally

### Transport Adapters

- stdio ↔ Streamable HTTP any-to-any conversion
- Pluggable adapter interface

### Remote Workers

- Offload heavy/stateless MCP servers to remote machines
- **Hard constraint**: NO remote filesystem mounting (NFS, FUSE, sshfs) — too fragile
- If transparent fs-proxy proves impossible/fragile → simply disallow remote workers for fs-dependent servers
- Remote workers only viable for network-only MCP (tavily, openrouter, context7, etc.)
- Research needed: can we do on-demand file proxying transparently?

### Web Dashboard

- Only if CLI monitoring proves insufficient
- Likely unnecessary with good `mcp-mux status/ps` commands

## Configuration

No separate mux config file. Server identity derived automatically:

```
Server ID = hash(command + args + selected_env_keys)
IPC endpoint = \\.\pipe\mcp-mux-{server_id}  (Windows)
             = /tmp/mcp-mux-{server_id}.sock  (Unix)
```

Server mode (shared/isolated) can be specified via:
- `mcp-mux --isolated <command> [args...]` flag
- Or auto-detected (future: known stateful servers list)
- Default: shared

## Architecture Overview

```
CC 1 ──stdio──> mcp-mux ──IPC──┐
CC 2 ──stdio──> mcp-mux ──IPC──┤──> mcp-mux (owner) ──stdio──> engram (1 process)
CC 3 ──stdio──> mcp-mux ──IPC──┤
CC 4 ──stdio──> mcp-mux ──IPC──┘

First mcp-mux instance = owner (spawns upstream, accepts IPC)
Subsequent instances = clients (connect to owner via IPC)
```

### ID Remapping Strategy

```
Downstream (Session 1): { "id": 1, "method": "tools/call", ... }
    ↓ remap
Upstream:               { "id": "s1:1", "method": "tools/call", ... }
    ↓ response
Upstream:               { "id": "s1:1", "result": ... }
    ↓ de-remap
Downstream (Session 1): { "id": 1, "result": ... }
```

### Tool Routing

- On upstream init, capture `tools/list` response
- Build tool→upstream routing table
- Route `tools/call` by tool name lookup

### Lifecycle

1. CC starts with `mcp-mux uvx ... serena` — mcp-mux checks if owner exists for this server identity
2. No owner → become owner: spawn upstream, init, cache result, listen on IPC
3. Owner exists → become client: connect to owner via IPC
4. CC sends `initialize` → return cached result
5. CC sends `tools/list` → return cached tool list
6. CC sends `tools/call` → route to upstream, remap id
7. CC disconnects → cleanup; if last client and no keep-alive → shutdown upstream

### Edge Cases

- **Tool name collision**: Two upstreams expose same tool name → N/A in wrapper model (one mcp-mux per server)
- **Upstream crash**: Detect via process exit, propagate error to all clients, attempt restart
- **Slow upstream**: Timeout per request (configurable), return JSON-RPC error
- **Owner crash**: Clients detect broken IPC, one becomes new owner, respawns upstream
- **Backpressure**: Bounded per-session queue, reject if full
