---
feature_id: ~
title: "Idempotent retry for FR-8 fallback path — defense-in-depth"
slug: inflight-idempotent-retry
status: Draft
created: 2026-04-19
modified: 2026-04-19
parent: upstream-survives-daemon-restart
children: []
supersedes: ~
superseded_by: ~
split_from: upstream-survives-daemon-restart
merges: []
aliases: [upstream-restart-retry-semantics]
open_crs: [CR-001]
---

# Feature: Idempotent retry for FR-8 fallback path — defense-in-depth

**Slug:** inflight-idempotent-retry
**Created:** 2026-04-19
**Status:** Draft
**Parent:** upstream-survives-daemon-restart
**Author:** AI Agent (seed only; not yet clarified)

> **Provenance:** Seeded by Opus 4.7 on 2026-04-19 as T038 follow-on from
> upstream-survives-daemon-restart. Evidence from: engram #109 original scope
> (Option A transparent retry / Option B structured error). Context: the parent
> spec fixes the root cause (upstream no longer dies on daemon restart). THIS
> spec covers the residual edge cases where handoff cannot fire and the old
> `drainOrphanedInflight` -32603 path is still reachable.
> Confidence: INFERRED (not yet refined via /nvmd-clarify).

## Overview

After `upstream-survives-daemon-restart` ships (v0.21.0), upstream processes survive
`upgrade --restart`, `mux_restart`, reaper idle, and daemon crash via Unix SCM_RIGHTS
FD passing / Windows DuplicateHandle. The parent spec's FR-8 preserves the current
shutdown-and-respawn behavior as a degraded fallback for scenarios where handoff is
unavailable (platform unsupported, token mismatch, protocol version skew, socket bind
failure, successor daemon crash mid-handshake).

When FR-8 fires, `drainOrphanedInflight` still emits `-32603 upstream restarted, request
lost during reconnect` to CC for every in-flight JSON-RPC request. For idempotent
methods (read-only queries: `list`, `info`, `status`, `health`, `find`, `read`), we can
transparently retry the request on the reconnected upstream before surfacing the error
to the client. For non-idempotent methods (mutating `exec`, `run`, `create`, `delete`),
we MUST still surface the error — but with a structured error code and retry advice so
CC agents can make informed retry decisions.

This spec is defense-in-depth over the parent architectural fix, NOT a standalone
solution.

## Context

### Why this exists separately

The original engram #109 asked for retry semantics (Option A transparent retry, Option
B structured error, hybrid). That scope was diagnosed as a symptom-level band-aid over
a deeper architectural problem (self-inflicted upstream kills). The parent spec
`upstream-survives-daemon-restart` removes the SOURCE of the problem by making upstream
survive daemon lifecycle events. This spec closes the long tail for the ~1% of cases
where handoff genuinely cannot fire.

### What changes

- On FR-8 fallback path, mcp-mux classifies in-flight requests by method name.
- Idempotent methods → transparent retry once after reconnect (Option A for that class).
- Non-idempotent methods → structured error with typed code + `data` fields (Option B).
- Both classes keep the same -32603 fallback code if the upstream is clearly dying (crash
  loop, circuit breaker tripped) — structured error requires a living successor upstream.

## Functional Requirements (seed — refine via clarify)

### FR-1: Method-name-based idempotency classification

The shim MUST maintain a whitelist of MCP method name suffixes that are idempotent-safe
to retry: `list`, `info`, `status`, `health`, `find`, `read`, plus any methods the
upstream declares via `x-mux.idempotent-methods: [...]` capability in its `initialize`
response.

**Classification defaults:**
- Methods matching whitelist suffix → idempotent.
- All other methods → non-idempotent (safe default — prefer false negatives).
- `initialize`, `ping`, `shutdown`, `notifications/*` handled separately (already have
  defined semantics).

### FR-2: Transparent retry for idempotent methods on FR-8 fallback

When a request in `drainOrphanedInflight` is classified idempotent, the shim MUST:
1. Hold the request instead of immediately emitting -32603.
2. Wait for `Reconnect()` callback to complete.
3. Replay the request on the new IPC path.
4. Forward the new response to CC.
5. If the replay also fails (circuit breaker, second reconnect) → emit structured error
   (FR-3).

### FR-3: Structured error code for non-idempotent / replay-failed cases

When transparent retry is unavailable or fails, emit JSON-RPC error with:

```json
{
  "code": -32099,
  "message": "UPSTREAM_RESTARTED",
  "data": {
    "upstream": "<command name>",
    "reason": "server_restart" | "crash_loop" | "handoff_failed",
    "retry_after_ms": 1000,
    "request_was_idempotent": false,
    "handoff_available": false,
    "suggestion": "retry after reconnect window, or escalate to operator if reason is crash_loop"
  }
}
```

Error code `-32099` reserved from JSON-RPC implementation-defined range. Distinct from
`-32603` (generic internal) which remains the degraded-beyond-structured fallback.

### FR-4: Retry budget and crash-loop detection

Single retry only per request. If the shim's `drainOrphanedInflight` fires twice for
the same request ID within 60 seconds → skip retry, emit structured error with
`reason: "crash_loop"`. Prevents retry amplification during upstream crash loops.

### FR-5: Extended observability

`mux_list --verbose` exposes a `retry_stats` block per upstream: retries attempted,
retries succeeded, retries failed, structured-error count, classification distribution
(idempotent vs non-idempotent).

## Non-Functional Requirements

### NFR-1: Retry latency overhead < 100 ms

For idempotent retry on a healthy reconnected upstream, the total latency overhead
introduced by FR-2 (hold + wait-reconnect + replay) must be < 100 ms p99 on reference
hardware. Measured from original request send to client-visible response.

### NFR-2: Zero spec changes required by upstream MCP servers

Retry classification uses method-name heuristic by default. Upstream servers MAY
declare `x-mux.idempotent-methods` in their `initialize` response to refine
classification, but MUST NOT be required to do so. Zero-configuration preserved
(constitution #2).

## User Stories (seed)

### US1: Read-only query succeeds transparently after upstream restart (P1)

**As a** CC agent making `sessions/list` during an `upgrade --restart`,
**I want** the query to succeed without visible error,
**so that** agent loop continues without interruption even when handoff is unavailable.

### US2: Write operation fails with actionable retry advice (P1)

**As a** CC agent making `tools/call create_file` during upstream restart,
**I want** a structured error indicating the request was non-idempotent and giving me
a `retry_after_ms` hint,
**so that** the agent orchestrator can make an informed decision (replay, ask user, or
abort).

### US3: Crash-loop is surfaced clearly to operator (P2)

**As an** operator running a buggy upstream that crashes every few seconds,
**I want** structured errors to switch from `server_restart` reason to `crash_loop`
after the second failure in 60s,
**so that** I know to investigate the upstream rather than retry harder.

## Out of Scope

- **Transport handoff mechanism** — covered by parent spec `upstream-survives-daemon-restart`.
- **Per-method retry policy DSL** — simple whitelist + upstream-declared override is
  sufficient.
- **Exponential backoff across many retries** — single retry only (FR-4). If more
  sophisticated retry is needed, that is a client-side concern.

## Dependencies

Parent spec `upstream-survives-daemon-restart` MUST be shipped first. FR-8 fallback
path in the parent spec is the surface area this spec operates on. If the parent
spec's FR-1 (handoff) is working correctly, this spec's code paths rarely fire.

## Open Questions

- **Q1:** [NEEDS CLARIFICATION] Should the `x-mux.idempotent-methods` capability be
  additive only, or can upstream declare that default-whitelisted methods (e.g., `list`)
  are NOT idempotent in their implementation? Recommendation: additive-only for
  simplicity; upstream that has non-idempotent `list` should rename to `execute_list`.
- **Q2:** [NEEDS CLARIFICATION] Retry budget — fixed 1 retry, or should the shim
  support operator-tunable budget via env var? Recommendation: fixed 1 retry. More is
  complexity for marginal gain.
- **Q3:** [NEEDS CLARIFICATION] Crash-loop detection window — 60s default, or match
  the existing daemon circuit-breaker window? Recommendation: align with existing
  circuit-breaker constants in `muxcore/daemon/daemon.go` for observability consistency.

## Target release

- **muxcore/v0.21.1** (MINOR — additive `x-mux.idempotent-methods` capability, new
  `-32099` error code, no breaking changes to v0.21.0 semantics).
- **mcp-mux/v0.10.1** (MINOR).
