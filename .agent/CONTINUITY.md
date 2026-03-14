# Continuity State

**Last Updated:** 2026-03-14
**Session:** Project genesis — specification and architecture

## Done
- Project created at D:\Dev\mcp-mux, git initialized
- .agent directory structure created (data, plans, reports, specs, arch/decisions)
- CLAUDE.md + AGENTS.md bootstrap
- Full specification: `.agent/specs/mcp-mux.md`
- 4 ADRs written:
  - ADR-001: stdio proxy over HTTP conversion
  - ADR-002: named pipes downstream transport (Phase 1: wrapper script MVP)
  - ADR-003: session-prefixed id remapping strategy
  - ADR-004: server categorization — shared vs isolated

## Now
- Nothing active — project ready for implementation planning

## Next
- Create implementation plan (`/deep-planning` with spec as input)
- Phase 1 MVP: wrapper script + mux daemon with shared mode only
  - Downstream: wrapper bridges CC stdio → named pipe
  - Upstream: spawn + manage shared MCP servers
  - Core: JSON-RPC parse, id remap, tool→upstream routing
  - Test: 2 mock CC sessions, 2 mock MCP servers, concurrent requests
- Phase 2: Isolated mode for stateful servers
- Phase 3: Health monitoring, auto-restart, status endpoint
- Phase 4: Config generation from existing .mcp.json / .claude.json

## Architecture Summary

```
CC 1 ──stdio──> mcp-mux-client ──pipe──┐
CC 2 ──stdio──> mcp-mux-client ──pipe──┤──> mcp-mux daemon
CC 3 ──stdio──> mcp-mux-client ──pipe──┤    ├── engram (shared, 1 process)
CC 4 ──stdio──> mcp-mux-client ──pipe──┘    ├── tavily (shared, 1 process)
                                            ├── context7 (shared, 1 process)
                                            ├── nvmd-ssh (isolated, 4 processes)
                                            └── aimux (isolated, 4 processes)
```

Without mux: 4 × 12 = 48 processes (~4.8 GB)
With mux: 8 shared + 8 isolated + 4 clients + 1 daemon = 21 processes (~2 GB)

## Key Decisions
- TypeScript, Node.js >= 20, zero npm deps for core
- JSON-RPC id remapping: session prefix "s{N}:{id}"
- Named pipes (Windows \\.\pipe\, Unix socket) for downstream IPC
- Phase 1 MVP uses wrapper script (mcp-mux-client) to bridge CC stdio → pipe
- Servers categorized as shared (default) or isolated in config

## Context
- Motivation: 4 parallel CC sessions × 12 MCP servers = 48 node processes, 4.8 GB RAM
- Primary platform: Windows 11 (named pipes), secondary: Linux (AI Harbour)
- MCP spec version: 2025-06-18 (JSON-RPC 2.0 over stdio)

## Blockers
- None — ready for implementation planning
