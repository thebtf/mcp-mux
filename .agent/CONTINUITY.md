# Continuity State

**Last Updated:** 2026-03-15
**Session:** Architecture brainstorm — revised spec

## Done
- Project created at D:\Dev\mcp-mux, git initialized
- .agent directory structure created (data, plans, reports, specs, arch/decisions)
- CLAUDE.md + AGENTS.md bootstrap
- Full specification v1: `.agent/specs/mcp-mux.md`
- 4 ADRs written (001-004)
- **Architecture brainstorm session** — major design revisions:
  - Wrapper pattern (mcp-mux as command prefix, not daemon+client)
  - Go instead of TypeScript
  - Self-coordinating instances (first = owner, rest = clients via IPC)
  - One code path for shared/isolated
  - cwd sandbox with symlink checking
  - CLI monitoring (status/ps/restart)
  - Transport abstraction (future: any-to-any)
- Spec v2 updated with all brainstorm decisions

## Now
- Nothing active — spec finalized, ready for implementation planning

## Next
- Update ADRs to reflect new decisions (wrapper pattern, Go stack, self-coordination)
- Create implementation plan (`/deep-planning` with spec v2 as input)
- Phase 1 MVP:
  - Go binary: mcp-mux wrapper + owner/client coordination
  - IPC via named pipes (Windows) / Unix sockets
  - JSON-RPC parse, id remap, tool routing
  - Shared mode only (isolated = Phase 2)
  - `mcp-mux status` basic command
  - Test: 2 mock CC sessions, 2 mock MCP servers, concurrent requests
- Phase 2: Isolated mode, `mcp-mux setup` CLI helper
- Phase 3: Health monitoring, restart, diagnostics
- Phase 4: Transport adapters (stdio ↔ Streamable HTTP)
- Research: Remote workers feasibility for stateless servers

## Architecture Summary

```
CC 1 ──stdio──> mcp-mux ──IPC──┐
CC 2 ──stdio──> mcp-mux ──IPC──┤──> mcp-mux (owner) ──stdio──> engram (1×)
CC 3 ──stdio──> mcp-mux ──IPC──┤
CC 4 ──stdio──> mcp-mux ──IPC──┘
```

First instance = owner (spawns upstream, listens on IPC).
Subsequent instances = clients (connect to owner).
Server ID = hash(command + args + selected env).

## Key Decisions
- **Go** — single binary, goroutines, zero runtime deps
- **Wrapper pattern** — `mcp-mux <original-command> [args...]`
- **Self-coordination** — no separate daemon, first instance becomes owner
- **One code path** — shared/isolated differ only in policy, not logic
- **cwd sandbox** — strict, path traversal blocked, symlinks verified
- **No remote fs mounts** — too fragile; remote workers only for network-only MCP
- **No web dashboard** — CLI monitoring (status/ps/restart) sufficient
- **No separate mux config** — server identity derived from command+args hash

## Context
- Motivation: 4 parallel CC sessions × 12 MCP servers = 48 node processes, 4.8 GB RAM
- Primary platform: Windows 11 (named pipes), secondary: Linux (AI Harbour)
- MCP spec version: 2025-06-18 (JSON-RPC 2.0 over stdio)

## Blockers
- None — ready for ADR update + implementation planning
