# Feature: muxcore — Embeddable MCP Multiplexer Engine

**Slug:** muxcore-library
**Created:** 2026-04-13
**Status:** Draft
**Author:** AI Agent (reviewed by user)

> **Provenance:** Specified by claude-opus-4-6 on 2026-04-13.
> Evidence from: architecture.md (same directory), 3 codebase explorations (mcp-mux, aimux, engram),
> MCP spec (Context7), CC source analysis, Go module research (go.dev), user requirements (4 key facts).
> Confidence: Architecture VERIFIED against source, consumer needs VERIFIED via exploration.

## Overview

muxcore is the full multiplexer engine extracted from mcp-mux into `internal/muxcore/`, embeddable
into any Go-based MCP stdio server. Every server becomes "its own mcp-mux" — running a daemon that
serves all local Claude Code sessions through a single upstream process. When running behind mcp-mux,
the embedded engine switches to proxy mode, deferring multiplexing to the parent.

## Context

mcp-mux currently lives as a monolithic binary. Its multiplexing capabilities (daemon, session
management, process lifecycle, progress reporting, graceful restart) are locked inside `internal/`
and cannot be reused. Two other Go projects — aimux (AI CLI multiplexer) and engram (memory server)
— would benefit from the same multi-session daemon architecture:

- **Without mcp-mux installed**: servers distributed via Claude Code plugins need to self-multiplex
  (one daemon per server, shared across all CC sessions)
- **With mcp-mux installed**: servers should cooperate with mcp-mux as the master daemon

Currently: aimux has its own buggy process management (no tree kill), engram has no subprocess
management at all. Both spawn separate processes per CC session, wasting memory.

## Functional Requirements

### FR-1: Embeddable Engine
The library must provide a single entry point (`engine.New(config).Run(ctx)`) that any MCP stdio
server can call to get full multiplexing capabilities without understanding muxcore internals.

### FR-2: Three Operating Modes
The engine must automatically detect and switch between:
- **Daemon mode**: first invocation becomes the daemon, spawns MCP logic, listens on IPC
- **Client mode**: subsequent invocations connect to existing daemon via IPC, bridge stdio
- **Proxy mode**: when running behind a parent mcp-mux, skip daemon creation, run as simple stdio server

### FR-3: Process Tree Kill
The lifecycle manager must kill the ENTIRE process tree on shutdown — not just the leader process.
This must work correctly on both Unix (process groups via pgid) and Windows (Job Objects).

### FR-4: Multi-Session Multiplexing
The engine must route requests from multiple concurrent CC sessions through a single upstream
MCP server process, maintaining correct session isolation (request ID namespacing, progress
token routing, session-aware metadata).

### FR-5: Graceful Restart with Snapshot
The engine must serialize daemon state (owner cache, session metadata) to a snapshot file before
shutdown and restore from it on restart, enabling zero-downtime upgrades with instant cached
responses.

### FR-6: Activity-Aware Reaping
The engine must track last activity, inflight requests, progress tokens, and busy declarations
to avoid killing upstream processes that are actively working. Configurable idle timeout per owner.

### FR-7: Synthetic Progress Notifications
The engine must generate synthetic `notifications/progress` messages for long-running tool calls
when upstream stays silent, showing tool name and elapsed time in CC's UI.

### FR-8: Hierarchical Cooperation
When a muxcore-enabled server detects it's running behind mcp-mux (via environment variable),
it must operate in proxy mode — delegating all multiplexing to the parent while retaining
access to session metadata (session ID, cwd) from mcp-mux's injection.

### FR-9: Self-Spawn Daemon
In daemon mode, the engine must re-exec the server binary as a detached background process
for the daemon, ensuring crash isolation between the daemon and the shim process.

### FR-10: Backward Compatibility
During the extraction phase (`internal/muxcore/` inside mcp-mux), all existing mcp-mux behavior
must be preserved. The extraction is a refactor, not a rewrite. All existing tests must pass.

## Non-Functional Requirements

### NFR-1: Performance
Multiplexing overhead must be <1ms per message. Synthetic progress tick must complete in <1ms.
No allocations on hot paths when no work is needed (empty inflight tracker, no pending requests).

### NFR-2: Zero External Dependencies (beyond stdlib)
The library must minimize external dependencies. Current allowed: `google/uuid` (for isolated
mode IDs), `thejerf/suture/v4` (supervisor). No logging framework — use callback injection.

### NFR-3: Platform Support
Must work on Linux, macOS, and Windows. Platform-specific code must be behind build tags.
Process group management uses native APIs: `Setpgid`/`Kill(-pgid)` on Unix, `CreateJobObject`/
`KILL_ON_JOB_CLOSE` on Windows. Must NOT use `CREATE_NEW_PROCESS_GROUP` (breaks dotnet builds).

### NFR-4: API Stability
Start at v0.x.y while in `internal/muxcore/`. Freeze API at v1 when promoted to standalone repo.
Use keyed struct literals in public API. Keep interfaces small (1-3 methods). Never export what
isn't intended to be supported long-term.

### NFR-5: Testability
Every package must be independently testable without starting real processes or listening on
real sockets. Mock-friendly interfaces for IPC, process spawning, and time.

## User Stories

### US1: MCP Server Self-Multiplexes (P1)
**As an** MCP server author, **I want** to embed muxcore so my server handles multiple CC sessions
through one process, **so that** users don't need mcp-mux installed and I don't waste N processes
for N sessions.

**Acceptance Criteria:**
- [ ] Add `engine.New(config).Run(ctx)` to server's main() — compiles and runs
- [ ] First CC session: server starts as daemon, MCP logic runs, session connects
- [ ] Second CC session: new shim connects to existing daemon, gets cached init instantly
- [ ] Kill all CC sessions: daemon stays alive for idle timeout, then exits gracefully
- [ ] Process tree killed cleanly on shutdown (zero orphans)

### US2: Cooperative Behind mcp-mux (P1)
**As a** user with mcp-mux installed, **I want** muxcore-enabled servers to cooperate with mcp-mux
as the master daemon, **so that** I get one daemon managing everything, not N competing daemons.

**Acceptance Criteria:**
- [ ] mcp-mux sets `MCP_MUX_SESSION_ID` when spawning upstream
- [ ] muxcore detects env var → enters proxy mode (no own daemon)
- [ ] mcp-mux handles all multiplexing; server runs as simple stdio MCP server
- [ ] Session metadata (_meta.muxSessionId, _meta.muxCwd) available to server in proxy mode
- [ ] All muxcore features (progress, busy) work through parent mcp-mux

### US3: mcp-mux Refactored to Use muxcore (P1)
**As the** mcp-mux maintainer, **I want** mcp-mux to use muxcore internally, **so that** mcp-mux
becomes a thin CLI wrapper and the engine is shared code.

**Acceptance Criteria:**
- [ ] mcp-mux imports from `internal/muxcore/` instead of `internal/mux/`, `internal/daemon/`, etc.
- [ ] All existing mcp-mux tests pass without modification
- [ ] mcp-mux binary size does not increase by more than 10%
- [ ] `mux_list`, `mux_stop`, `mux_restart` tools still work
- [ ] Graceful restart (upgrade --restart) still works with snapshots

### US4: Process Tree Kill (P1)
**As a** system administrator, **I want** muxcore to kill entire process trees on shutdown,
**so that** no orphan processes consume memory after upgrade or restart.

**Acceptance Criteria:**
- [ ] Unix: `syscall.Kill(-pgid, SIGTERM)` followed by SIGKILL on timeout
- [ ] Windows: `TerminateJobObject` kills all processes in job
- [ ] Zero orphan node.exe / python.exe after `upgrade --restart` cycle
- [ ] Graceful shutdown phases preserved: stdin close → drain → SIGTERM → SIGKILL

### US5: Plugin Distribution Without mcp-mux (P2)
**As a** plugin author distributing an MCP server via Claude Code plugins, **I want** my server
to self-multiplex without requiring the user to install mcp-mux, **so that** installation is
just adding the plugin config.

**Acceptance Criteria:**
- [ ] User adds server to `.mcp.json` → first CC session starts daemon transparently
- [ ] Multiple CC sessions share one server process
- [ ] No mcp-mux binary needed on the system
- [ ] If user later installs mcp-mux, server switches to proxy mode automatically

### US6: Synthetic Progress for Embedded Servers (P2)
**As a** Claude Code user, **I want** to see elapsed time for long-running tool calls from any
muxcore-enabled server, **so that** I know my request is being processed.

**Acceptance Criteria:**
- [ ] After 5 seconds inflight, CC UI shows "{tool_name}: 5s elapsed"
- [ ] Updates every 5 seconds (configurable via x-mux.progressInterval)
- [ ] Stops when real upstream progress arrives (dedup)
- [ ] Stops immediately when tool call completes

## Edge Cases

- Two CC sessions start simultaneously with no existing daemon: race resolved by file lock
  on daemon startup (same as current mcp-mux behavior with `mcp-muxd.lock`)
- mcp-mux and self-mux daemon compete for same IPC socket: different socket paths — self-mux
  uses `muxcore-{server-hash}.sock`, mcp-mux uses `mcp-mux-{server-hash}.sock`
- Server binary updated while daemon running: graceful restart via snapshot (same as mcp-mux
  `upgrade --restart`)
- Proxy mode but parent mcp-mux crashes: muxcore detects broken IPC, falls back to standalone
  daemon mode on next invocation (self-healing)
- Multiple muxcore-enabled servers on same system without mcp-mux: each gets its own daemon
  with independent IPC sockets (no conflict)
- `MCP_MUX_SESSION_ID` env var set accidentally (not by mcp-mux): proxy mode enters, server
  works as simple stdio — degraded but functional

## Out of Scope

- **Standalone repo**: extraction starts in `internal/muxcore/` inside mcp-mux. Promotion to
  standalone `github.com/thebtf/muxcore` is a future step after API stabilization.
- **Non-Go consumers**: library is Go-only. Other languages use mcp-mux as external binary.
- **HTTP/SSE transport**: only stdio multiplexing. HTTP transport is a separate concern.
- **MCP control-plane tools**: `mux_list`, `mux_stop`, `mux_restart` remain in mcp-mux as
  its unique management layer. muxcore provides the engine; mcp-mux provides the CLI.
- **aimux/engram integration**: embedding muxcore in consumers is Phase 6, after mcp-mux
  refactor stabilizes.

## Dependencies

- `thejerf/suture/v4` — OTP-style supervisor for owner lifecycle
- `google/uuid` — for isolated mode unique IDs
- Go stdlib: `os/exec`, `sync`, `net`, `encoding/json`, `syscall`
- Win32 API (Windows only): `CreateJobObject`, `AssignProcessToJobObject`, `TerminateJobObject`

## Success Criteria

- [ ] mcp-mux refactored to use `internal/muxcore/` — all tests pass, behavior identical
- [ ] `internal/muxcore/engine` provides `New(Config).Run(ctx)` entry point
- [ ] Three modes work: daemon, client, proxy
- [ ] Process tree kill works on Unix and Windows (zero orphans)
- [ ] Synthetic progress works in embedded mode
- [ ] Graceful restart with snapshot works in embedded mode
- [ ] Activity-aware reaping works in embedded mode
- [ ] mcp-mux binary size change <10%
- [ ] No new external dependencies beyond suture + uuid

## Clarifications

### Session 2026-04-13 (user input)

| # | Category | Question | Resolution | Date |
|---|----------|----------|------------|------|
| C1 | Module name | muxcore vs stdiomux vs procmux | User chose: **muxcore** | 2026-04-13 |
| C2 | suture | In library or mcp-mux only? | User: in library if needed there | 2026-04-13 |
| C3 | remap | MCP-specific or shared? | User: all projects are MCP servers, all need everything | 2026-04-13 |
| C4 | Multi-session | Which projects need it? | User: ALL three, each must be "self-mux" | 2026-04-13 |
| C5 | Distribution | Why self-mux? | User: for plugins on systems WITHOUT mcp-mux | 2026-04-13 |
| C6 | Hierarchy | How does mcp-mux interact? | User: mcp-mux = master, others = proxy through it | 2026-04-13 |
| C7 | Repo | Standalone now? | User: in mcp-mux internal/ first, separate repo later | 2026-04-13 |

## Open Questions

None — all resolved.

| # | Category | Question | Resolution | Date |
|---|----------|----------|------------|------|
| C8 | Integration | Handler interface: simple or MCP-aware? | Simple `func(ctx, io.Reader, io.Writer) error` — consumers rewrite main() to match | 2026-04-13 |
| C9 | Integration | Self-spawn: re-exec or in-process? | Re-exec (crash isolation). Consumers add `--daemon` arg branch. | 2026-04-13 |
| C10 | Integration | Version negotiation between muxcore↔mcp-mux? | Not needed — all projects ours, updated together. Add later if needed (YAGNI). | 2026-04-13 |
| C11 | Constraints | Can consumers rewrite code for integration? | Yes — all projects are ours. Playbook with instructions, no backwards-compat hacks. | 2026-04-13 |
