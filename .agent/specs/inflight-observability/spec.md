# Feature: Inflight Request Observability

**Slug:** inflight-observability
**Created:** 2026-04-07
**Status:** Draft
**Author:** AI Agent (reviewed by user)

> **Provenance:** Specified by Claude Opus 4.6 on 2026-04-07.
> Evidence from: codebase analysis (session_manager.go, owner.go Status()),
> user report (serena activate_project hanging 3+ minutes with only "pending: 1" visible),
> constitution.md principles.
> Confidence: VERIFIED (existing data structures), INFERRED (output format).

## Overview

When `mux_list --verbose` shows `pending_requests > 0`, include detailed inflight
request information: method name, tool name (for tools/call), originating session ID,
start timestamp, and elapsed time. This replaces the opaque `pending: 1` counter with
actionable diagnostic data.

## Context

Currently `mux_list` shows `pending_requests: N` — a bare counter. When a server hangs
(e.g., serena's `activate_project` taking 3+ minutes), the user sees "pending: 1" and
has no way to know:
- **What** is pending (which MCP method? which tool?)
- **How long** it's been pending (3 seconds or 3 minutes?)
- **Who** sent the request (which CC session?)

The owner already tracks partial data: `sessionMgr.inflight` maps remapped IDs to
session IDs, and `methodTags` maps remapped IDs to method names (but only for cacheable
methods). For tools/call and other non-cacheable requests, no method or params are recorded.

## Functional Requirements

### FR-1: Inflight Request Tracking
The owner must track metadata for every request forwarded to upstream: method name,
tool name (extracted from tools/call params), originating session ID, and start timestamp.
Metadata is recorded when the request is forwarded and removed when the response arrives.

### FR-2: Inflight Details in Status Output
`Owner.Status()` must include an `inflight` array when `pending_requests > 0`. Each entry
contains: method, tool name (if tools/call), session ID, start time (RFC3339), and
elapsed seconds. The array is omitted when empty (no overhead for idle servers).

### FR-3: Inflight Details in mux_list Verbose
`mux_list` with `verbose=true` must include the inflight array from Status() in its
output. Compact mode (`verbose=false`) continues to show only `pending: N` count.

### FR-4: Tool Name Extraction
For `tools/call` requests, the tool name must be extracted from `params.name` in the
JSON-RPC request before forwarding to upstream. For other methods, tool name is empty.

## Non-Functional Requirements

### NFR-1: Zero Overhead for Idle Servers (Performance)
Inflight tracking must add zero allocation when no requests are pending. The tracking
structure should only allocate when requests are actually in flight.

### NFR-2: Thread Safety (Correctness)
Inflight tracking must be safe for concurrent access from multiple session goroutines
and the upstream reader goroutine.

### NFR-3: No Message Modification (Transparency)
Extracting tool names must not modify the JSON-RPC message. Read-only parsing only.
Constitution principle 1: transparent proxy.

## User Stories

### US1: Diagnose Hanging Tool Call (P1)
**As a** developer, **I want** to see which specific tool call is hanging and for how long,
**so that** I can decide whether to wait, restart the server, or report an upstream bug.

**Acceptance Criteria:**
- [ ] `mux_list --verbose` shows method, tool name, session, elapsed for each pending request
- [ ] serena with activate_project pending shows: `"method": "tools/call", "tool": "activate_project", "elapsed_seconds": 186.5`
- [ ] Elapsed time updates on each mux_list call (not cached)

### US2: Identify Slow Upstream Patterns (P2)
**As a** developer, **I want** to see start timestamps for pending requests,
**so that** I can correlate with daemon logs and identify recurring slow patterns.

**Acceptance Criteria:**
- [ ] Start time is RFC3339 format with millisecond precision
- [ ] Start time matches the moment the request was forwarded to upstream (not when received from session)

## Edge Cases

- Request completes between Status() reading inflight map and serializing to JSON — stale entry is acceptable (will be gone on next call)
- tools/call with malformed params (no "name" field) — tool name is empty string, not error
- Proactive init requests (mux-init-0, mux-init-1) — shown with method "initialize"/"tools/list", session 0 (no real session)
- 100+ concurrent inflight requests — array may be large; no pagination needed (rare scenario)

## Out of Scope

- Request/response body capture (too large, privacy concerns)
- Historical request log (only currently inflight, not completed)
- Alerting or timeout enforcement (observability only, not policy)
- Inflight tracking for notifications (only requests have responses to track)

## Dependencies

- Existing: `sessionMgr.inflight` map, `pendingRequests` counter, `methodTags` sync.Map
- Existing: `Owner.Status()` used by `mux_list`
- No new external dependencies

## Success Criteria

- [ ] `mux_list --verbose` with pending request shows method + tool + elapsed
- [ ] Zero performance regression for idle servers (benchmark: Status() call < 1ms)
- [ ] All existing tests pass + new tests for inflight tracking
- [ ] Proactive init requests visible in inflight (method but no session)

## Open Questions

- None
