# CR-001: Initial scope — upstream survives daemon restart

**Feature:** upstream-survives-daemon-restart
**Change type:** CREATE
**Date:** 2026-04-19
**Author:** Opus 4.7 (reviewed by user)

## Summary

Establish the initial scope for making upstream MCP processes survive every planned
daemon lifecycle event (`upgrade --restart`, graceful shutdown, daemon crash) via Unix
SCM_RIGHTS FD passing and Windows Job Object detachment + DuplicateHandle.

This is the root cause fix for engram issue #109, which was initially scoped as "structured
error + idempotent retry semantics" but subsequently diagnosed as an architectural violation
of constitution principles #1 (Transparent Proxy) and #9 (Atomic Upgrades). The retry
subfeature is deferred to `inflight-idempotent-retry` as defense-in-depth for the FR-8
degraded fallback path only.

## Motivation

Today:

- `Owner.Shutdown()` → `upstream.Close()` = SIGTERM → 5s wait → SIGKILL. Runs for every
  daemon-level event including `upgrade --restart`, `mux_restart`, idle reaper, daemon stop.
- New daemon unconditionally respawns upstream via `SpawnUpstreamBackground()`.
- Inflight JSON-RPC requests in the murdered child are physically lost.
- `drainOrphanedInflight` honestly reports `-32603 "upstream restarted, request lost during
  reconnect"` to CC per orphaned request.

Without mcp-mux, CC stdio MCP client has NO retry logic for stdio transports — but without
mcp-mux, upstream restart NEVER happens (upstream = CC's own child, session-lifetime-bound).
mcp-mux introduces the entire restart event class. Constitution #1: "A server running
through mcp-mux MUST behave identically to running without it." Violated.

## Changes

### Added

- `.agent/specs/upstream-survives-daemon-restart/spec.md` (310 lines) — full spec covering:
  - 11 functional requirements (FR-1…FR-11)
  - 7 non-functional requirements (NFR-1…NFR-7)
  - 4 user stories (US1 P0, US2 P1, US3 P1, US4 P0) with 13 acceptance criteria
  - Edge cases: rolling upgrade skew, successor crash mid-handoff, concurrent shim reconnect,
    permission denied, removed mounts, reaper eviction during handoff, token handshake failure
  - Out of scope: retry semantics (separate spec), cross-host migration, upstream binary
    upgrade, mid-request serialization
  - Dependencies: internal (Owner, Process, daemon, snapshot, ipc, cmd), external
    (`golang.org/x/sys/unix`, `golang.org/x/sys/windows`), existing work preservation
    (sockperm, token handshake)
  - 7 success criteria
  - 3 open questions flagged for `/nvmd-clarify`: `mux_restart --graceful` flag,
    Windows Job Object ownership model, macOS launchd interaction

### Not changed (explicit)

- **Retry semantics remain out of scope.** The follow-on `inflight-idempotent-retry`
  spec will handle defense-in-depth for the FR-8 fallback path — IF that fallback fires.
- **`mux_restart <sid>` remains a hard-restart.** US2 preserves operator intent clarity.
- **Legacy `MCP_MUX_NO_DAEMON=1` mode unchanged.** No daemon to restart → FR-1 is N/A.

## Constitutional alignment

- **#1 Transparent Proxy.** Implemented — upstream no longer observes mcp-mux lifecycle.
- **#5 Graceful Degradation.** FR-8 degraded fallback preserved when handoff unavailable.
- **#6 Cross-Platform.** FR-2 Unix + FR-3 Windows parity explicit.
- **#9 Atomic Upgrades.** Implemented — upgrade no longer crashes upstreams.

## Traceability

- **Engram issue:** #109 (mcp-mux target, from 689ee718, opened 2026-04-18 23:47).
- **Root cause comment:** engram #109 comment 224 by claude-code at 2026-04-19.
- **Semantic trace tools:** socraticode + serena over muxcore/owner/owner.go,
  muxcore/upstream/process.go, muxcore/daemon/snapshot.go, cmd/mcp-mux/main.go.
- **External source reference:** CC stdio client `D:\Dev\_EXTRAS_\claude-code\src\services\mcp\client.ts`.
- **Target release:** muxcore/v0.21.0 (MAJOR — breaking upstream lifecycle semantics),
  mcp-mux/v0.10.0 (MAJOR).

## Next steps

1. `/nvmd-clarify` — resolve Q1 (`mux_restart --graceful`), Q2 (Windows Job Object model),
   Q3 (macOS launchd interaction).
2. `/nvmd-plan` — implementation plan with platform-split, handoff protocol v1 schema,
   migration strategy.
3. `/nvmd-tasks` — task breakdown. Expected ≥15 tasks across ~8 files + tests.
4. `/nvmd-validate` — cross-artifact consistency check.
5. Implement in `feat/upstream-survives-restart` worktree.
