# Feature: listChanged Trigger Mechanism

**Slug:** listchanged-trigger
**Created:** 2026-04-14
**Status:** Draft
**Author:** AI Agent

> **Provenance:** Specified by claude-opus-4-6 on 2026-04-14.
> Evidence from: codebase (owner.go lines 1997-2002 NOTE, listchanged/inject.go),
> CC MCP client source (useManageMCPConnections.ts lines 618-699).
> Confidence: VERIFIED — inject functions exist, wiring gap confirmed in code.

## Overview

Wire the existing `listchanged.InjectInitializeCapability` into the owner's init cache
path, and emit `notifications/{tools,prompts,resources}/list_changed` to all connected
sessions when the upstream process restarts or reloads. This makes CC invalidate its
cached tool/prompt/resource lists automatically.

## Context

CC's MCP client only subscribes to list_changed notifications when the server declares
`listChanged: true` in its initialize capabilities. Most MCP servers don't set this flag.
mcp-mux must inject it unconditionally so CC subscribes, then emit the notifications
when upstream state changes (restart, hot reload, mux_restart).

**Existing code:**
- `internal/muxcore/listchanged/inject.go` — `InjectInitializeCapability()` ready, 10 tests
- `internal/muxcore/owner/owner.go:1997` — NOTE saying injection is intentionally not wired yet

## Functional Requirements

### FR-1: Inject listChanged in Cached Init Response
When `cacheResponse("initialize")` stores the init response, call
`InjectInitializeCapability` on the raw bytes before caching. All sessions
that receive the cached init will see `listChanged: true` for tools, prompts,
and resources.

### FR-2: Emit list_changed on Upstream Restart
When the suture supervisor restarts the upstream process (after crash or
mux_restart), emit `notifications/tools/list_changed`,
`notifications/prompts/list_changed`, and `notifications/resources/list_changed`
to ALL connected sessions. This triggers CC to re-fetch tools/list etc.

### FR-3: Emit list_changed on Cache Invalidation
When `invalidateCacheIfNeeded` clears a cached list (tools, prompts, resources)
because upstream sent a real list_changed notification, forward that notification
to all sessions (already partially implemented — verify and complete).

### FR-4: No False Notifications
Do not emit list_changed during normal request/response traffic. Only emit on:
(a) upstream restart, (b) real upstream list_changed notification, (c) mux_restart.

## Non-Functional Requirements

### NFR-1: Zero Regression
All existing tests pass. No behavior change for sessions that don't use listChanged.

### NFR-2: Latency
Injection adds < 1ms to init cache path (JSON parse + rewrite).

## User Stories

### US1: CC Sees Updated Tools After Restart (P1)
**As a** CC user, **I want** my tool list to refresh automatically when a server restarts,
**so that** I see new/removed tools without manually reconnecting.

**Acceptance Criteria:**
- [ ] After mux_restart, CC receives list_changed notification
- [ ] CC re-fetches tools/list and shows updated tools
- [ ] No manual /mcp reconnect needed

### US2: CC Subscribes to listChanged (P1)
**As a** CC session, **I want** the init response to declare listChanged: true,
**so that** CC's notification handler subscribes to list_changed events.

**Acceptance Criteria:**
- [ ] Cached init response contains `"listChanged": true` for tools/prompts/resources
- [ ] Injection does not alter other fields (protocolVersion, serverInfo, etc.)

## Edge Cases

- Upstream already declares listChanged: true → injection is no-op (idempotent)
- Upstream has no tools capability → injection skips tools (does not synthesize)
- Session connects after restart → gets fresh cached init (already correct)
- Multiple sessions connected during restart → all receive list_changed

## Out of Scope

- Hot reload detection (filesystem watcher) — only restart/explicit triggers
- Per-session selective notification — all sessions get all list_changed

## Dependencies

- muxcore/listchanged package (DONE — inject functions exist)
- muxcore/owner cacheResponse path (exists)
- suture supervisor restart events (exist in daemon)

## Success Criteria

- [ ] `InjectInitializeCapability` called in cacheResponse("initialize")
- [ ] list_changed notifications emitted to all sessions on upstream restart
- [ ] CC auto-refreshes tool list after mux_restart
- [ ] 4+ new tests covering injection wiring + notification emit
