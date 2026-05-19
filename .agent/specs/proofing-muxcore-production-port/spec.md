---
feature_id: ~
title: "Proofing to Muxcore Production Port"
slug: proofing-muxcore-production-port
status: Draft
created: 2026-05-19
modified: 2026-05-19
parent: ~
children: []
supersedes: ~
superseded_by: ~
split_from: ~
merges: []
aliases: []
open_crs: [CR-001]
sweep_status: BLOCKED_LEGACY_REGISTRY
sweep_error: "missing .agent/specs/_index.json; legacy slug-only SpecKit layout"
---

# Feature: Proofing to Muxcore Production Port

**Slug:** proofing-muxcore-production-port
**Created:** 2026-05-19
**Status:** Draft
**Author:** AI Agent (reviewed by user)

> **Provenance:** Specified by Codex on 2026-05-19 via
> `$nvmd-platform:nvmd-specify`, after ADR 011.
> Evidence from: current user incident thread, `user_job_statement.md`,
> `.agent/proofing/current-topology-poc/proofing.md`,
> `.agent/arch/decisions/010-resilient-shim-reconnect-contract.md`,
> `.agent/arch/decisions/011-proofing-to-muxcore-production-port.md`,
> SocratiCode search, and read-only symbol/file checks in `muxcore`.
> Confidence: VERIFIED for scope and affected production owners; BLOCKED for
> production parity completion until CR-001 is implemented and smoke-tested.
> Pre-create sweep: BLOCKED by missing `.agent/specs/_index.json`; this repo is
> still on a legacy slug-only SpecKit layout, so this spec is created as a
> legacy-compatible folder without registry mutation.

## Overview

Port the proven Phase 6-9 `current-topology-poc` invariants into real
`muxcore` production tests and refactors. The feature turns the proofing result
into a release-grade reliability contract for fresh-session attach, reconnect,
generation-aware handoff, and persistent/idle lifecycle behavior.

## Clarifications

### Session 2026-05-19

| # | Category | Question | Resolution | Date |
| --- | --- | --- | --- | --- |
| C1 | Observability / Public Surface | Should P8/P9 generation evidence be exposed through stable status fields, or should tests use package-private hooks only? | Full stable status contract: expose all generation and lifecycle evidence as documented operator API. | 2026-05-19 |
| C2 | Lifecycle Cleanup Ownership | Where should stale pending-token cleanup for reaped owners live: daemon owner-removal helper, reaper sweep, or session manager? | A single daemon-level owner-removal helper is authoritative; session manager exposes cleanup primitives but does not decide owner lifecycle. | 2026-05-19 |
| C3 | Runtime Smoke Target | Which real MCP upstream is the canonical runtime smoke target for this workstation? | `time` is the primary smoke target; `serena` is optional secondary after primary passes. | 2026-05-19 |

- Q: Should P8/P9 generation evidence be exposed through stable status fields,
  or should tests use package-private hooks only? -> A: Full stable status
  contract. All generation and lifecycle evidence needed for P8/P9 MUST be
  visible through the documented control status/operator API, not only through
  package-private test hooks.
- Q: Where should stale pending-token cleanup for reaped owners live: daemon
  owner-removal helper, reaper sweep, or session manager? -> A: A single
  daemon-level owner-removal helper is authoritative. The daemon owns owner
  lifecycle decisions and calls session-manager cleanup primitives; cleanup
  MUST NOT be duplicated separately in each removal path.
- Q: Which real MCP upstream is the canonical runtime smoke target for this
  workstation? -> A: `time` is the primary smoke target. `serena` is an
  optional secondary smoke target that runs only after the primary `time` smoke
  passes, so core mux reliability is proven before heavier upstream-specific
  behavior is evaluated.

## Context

Users are seeing random MCP startup and reconnect failures in new Claude
Code/Codex sessions when servers run through `mcp-mux` or through `muxcore`
consumers such as `engram`, `socraticode`, `serena`, and `time`.

Evidence anchor:

- Q1: "в каждой новой сессии часть mcp серверов которые идут через mcp-mux - фейлится"
- Q2: "mcp-mux сейчас ненадежный мультиплексор, не выполняющий основную идею"
- Q3: "даже со стартом проблемы, всегда"

The proofing ladder showed that the current topology can be made reliable in a
standalone substrate through explicit invariants:

- Phase 6: true concurrent owner dispatch and out-of-order response demux by
  JSON-RPC ID across restart.
- Phase 7: refresh-token reconnect without fallback spawn when the restored
  owner is alive.
- Phase 8: generation-aware handoff evidence for predecessor, successor,
  restored owner count, and old owner socket retirement.
- Phase 9: persistent/idle lifecycle classification with active-session
  protection, stale ticket cleanup, respawn generation, and persistent restore.

ADR 011 establishes the production port boundary: no more toy PoC phases before
mapping these invariants into `muxcore`. ADR 010 remains binding for
already-sent in-flight requests: default behavior is one JSON-RPC error by
original ID, not blind replay.

## Strangler Fig

This feature replaces unreliable parts of a running production runtime without
changing the public topology.

- **Legacy system identification:** existing production `muxcore` reconnect,
  owner lifecycle, daemon handoff/status, session token history, and reaper
  behavior in `muxcore/owner`, `muxcore/daemon`, `muxcore/session`, and
  `muxcore/snapshot`.
- **Seam declaration:** the existing `consumer -> shim -> daemon -> owner ->
  upstream` boundary remains the facade. Tests and refactors attach at
  package-level production seams, not at the standalone PoC.
- **Parity NFR:** every ported slice must prove the same causal invariant as
  the PoC with real `muxcore` code and must include a false-positive guard:
  daemon or process transition, owner generation or server ID, reconnect path,
  lifecycle counter, and request ID where applicable.
- **Removal trigger:** the standalone PoC stops being the primary authority only
  after P6-P9 production parity tests pass and a fresh `mcp-mux.exe` runtime
  smoke proves new-session attach plus restart against at least one real MCP
  upstream.

## Domain Modeling

DDD evaluated -- not needed. The relevant concepts are protocol/lifecycle
infrastructure: Owner, Session, Token, Snapshot, Handoff, Reaper, and Shim.
These are already `muxcore` runtime entities and do not require a new business
bounded context.

## Functional Requirements

### FR-1: Production parity slices

The system MUST port Phase 6, Phase 7, Phase 8, and Phase 9 as separate
production parity slices. Each slice MUST begin with a production test that
fails for the missing invariant, or with a written note that an existing
production test already proves the exact invariant.

Evidence: Q4, Q5, Q6; ADR 011 Port Sequence.

### FR-2: Phase 6 concurrent demux parity

The production `muxcore` path MUST prove that two outstanding JSON-RPC requests
can be active at the owner boundary, receive out-of-order responses, and route
responses back by original JSON-RPC ID across a daemon/owner restart boundary.

Acceptance proof MUST include request IDs, observed response order, and
successor or restored-owner evidence.

Evidence: Q5, Q6; proofing Phase 6 PASS; SocratiCode search result for
`experiments/current-topology-poc/PHASES.md` Phase 6; production owner
locations in `muxcore/owner/resilient_client.go`.

### FR-3: Phase 7 refresh-token reconnect parity

The production path MUST prove that reconnect tries `refresh-token` before
fallback spawn and avoids fallback spawn when the restored owner is alive.

Acceptance proof MUST include original token, refreshed token, refresh counter,
fallback counter, and owner-alive evidence.

Evidence: Q1, Q2; proofing Phase 7 PASS; existing production tests around
`TestReconnect_RefreshesAndSucceeds`,
`TestHandleRefreshSessionToken_CountersIncrement`, and
`session.Manager.RegisterReconnect`.

### FR-4: Phase 8 generation-aware handoff parity

The production path MUST expose documented status fields that distinguish
predecessor daemon, successor daemon, restored owner count, restored owner
identity/generation evidence, and retired old owner sockets.

Acceptance proof MUST fail when traffic succeeds only through fresh spawn
without restored-owner evidence.

Evidence: Q2, Q7; proofing Phase 8 PASS; ADR 011 handoff evidence requirement;
production status/handoff fields in `muxcore/daemon.HandleStatus`.

### FR-5: Phase 9 persistent/idle lifecycle parity

The production path MUST prove that non-persistent idle owners are reaped,
persistent owners survive idle and restart, active non-persistent sessions are
not reaped while connected, reaped owners do not retain usable pending tickets,
and respawned owners get a new generation or equivalent identity evidence.

Acceptance proof MUST use controlled TTLs plus state/counter assertions, not
sleep-only timing.

Evidence: Q2, Q7; proofing Phase 9 PASS; `muxcore/daemon/reaper.go` eligibility
contract; `muxcore/session` pending/bound cleanup functions.

### FR-6: Stable operator status contract

The control status/operator API MUST document and expose the generation,
handoff, reconnect, and lifecycle evidence needed by P8/P9 and runtime smoke.
At minimum this includes predecessor/successor handoff identity, restored owner
count, owner identity/generation or equivalent per-owner identity, old-owner
retirement evidence, persistent classification, active session count,
reaped-owner count, pending-ticket cleanup evidence, refresh-token counters,
and fallback-spawn counters.

These fields are a stable public operator contract. Any future removal,
rename, or semantic change MUST go through release notes and compatibility
policy.

Evidence: C1 clarification; ADR 011 Hyrum-sensitive status warning.

### FR-7: Unified owner removal cleanup

All owner removal paths MUST route through one daemon-level owner-removal helper
that owns lifecycle reason, registry deletion, stale pending-token cleanup,
reconnect-history cleanup where applicable, and status/counter updates.

The session manager MUST provide cleanup primitives keyed by owner/server
identity, but it MUST NOT independently decide that an owner is gone. Reaper,
explicit remove/stop/restart, failed restore cleanup, and any future soft-remove
path MUST use the same helper.

Evidence: C2 clarification; proofing Phase 9 stale ticket cleanup guard.

### FR-8: Runtime smoke after parity

After P6-P9 production parity tests pass, the project MUST build a fresh
`mcp-mux.exe` and run a controlled runtime smoke against the real `time` MCP
upstream as the primary target.

The smoke MUST verify that a fresh session attaches without
`connection closed: initialize response`, that restart behavior uses the
expected reconnect/handoff path, and that status exposes the relevant counters
or equivalent evidence.

`serena` MAY be run as an optional secondary smoke target after the primary
`time` smoke passes. A `serena` failure MUST be classified separately as
upstream-specific or muxcore-specific before blocking the core parity port.

Evidence: Q1, Q3; C3 clarification; ADR 011 runtime smoke requirement.

### FR-9: No PoC import into production

Production implementation MUST NOT import or depend on
`experiments/current-topology-poc`. The PoC remains a regression oracle and
design artifact only.

Evidence: Q4, Q5, Q6; ADR 011 Non-Goals.

### FR-10: Preserve already-sent in-flight request contract

The feature MUST preserve ADR 010 behavior for already-sent in-flight requests:
the default is one JSON-RPC error response by original ID, not blind replay.

Evidence: ADR 010; ADR 011 Non-Goals.

## Non-Functional Requirements

### NFR-1: Transparency

For successful non-error paths, `mcp-mux` MUST remain transparent to MCP
consumers and upstream servers. No new synthetic MCP messages may be introduced
unless they are valid under the MCP protocol and tied to a client-issued token.

### NFR-2: Cross-platform reliability

The production parity suite MUST pass on Windows and Unix-supported paths used
by existing CI. Any platform-specific handoff or IPC assertion MUST be isolated
behind build tags or platform-specific tests.

### NFR-3: Deterministic evidence

Parity tests MUST assert causal evidence rather than relying on timing alone.
Required evidence includes request ID, owner/server identity, reconnect path,
handoff/status counters, active-session state, or lifecycle counters depending
on the slice.

### NFR-4: Test runtime bound

Each new parity test SHOULD complete within 10 seconds locally. Any test using
TTL/reaper timing MUST use injectable short TTLs or package-private test hooks
instead of production-duration sleeps.

### NFR-5: Compatibility

Public API or status field additions MUST be additive. Existing `muxcore`
consumers that do not use the new evidence fields MUST continue to compile and
run.

### NFR-6: Status contract stability

Generation, handoff, reconnect, and lifecycle status fields introduced or
documented by this feature are stable operator API. They MUST be versioned in
release notes, covered by tests, and changed only through a compatibility-aware
release.

### NFR-7: Release gate

The feature is not production-ready until `go test ./... -count=1` passes in
both root and `muxcore`, the current-topology PoC runner remains green, and the
fresh-binary runtime smoke passes.

## User Stories

### US1: Reliable new-session attach (P1)

**As a** Claude Code or Codex user, **I want** MCP servers routed through
`mcp-mux` to attach in every fresh session, **so that** I do not have to debug
random `initialize response` startup failures before doing actual work.

**Acceptance Criteria:**

- [ ] Fresh-session smoke starts at least one real MCP upstream through the
      fresh `mcp-mux.exe`.
- [ ] No startup failure contains `connection closed: initialize response`.
- [ ] Status or logs show the expected daemon/owner/reconnect lifecycle path.

### US2: No false-positive restart success (P1)

**As a** maintainer, **I want** restart tests to prove predecessor/successor and
restored-owner evidence, **so that** a fresh-spawn fallback cannot masquerade as
a correct handoff.

**Acceptance Criteria:**

- [ ] P8 parity test fails if restored-owner evidence is absent.
- [ ] P8 parity test records predecessor and successor evidence.
- [ ] Old owner socket retirement or equivalent owner invalidation is asserted.

### US3: Safe concurrent request routing (P1)

**As a** MCP consumer with concurrent tool calls, **I want** responses routed by
JSON-RPC ID even when they complete out of order, **so that** restart recovery
does not corrupt request/response ownership.

**Acceptance Criteria:**

- [ ] P6 parity test has two outstanding requests.
- [ ] The fast response is observed before the slow response.
- [ ] Both responses are delivered exactly once to the correct original IDs.

### US4: Correct lifecycle cleanup (P1)

**As a** daemon operator, **I want** idle cleanup to remove only disposable
owners, **so that** active sessions and persistent owners survive while stale
tokens/tickets do not accumulate.

**Acceptance Criteria:**

- [ ] P9 parity test proves non-persistent idle reaping.
- [ ] Active non-persistent owner survives while connected and reaps after close.
- [ ] Persistent owner survives idle and restart.
- [ ] Pending tickets for reaped owners are not usable afterward.

## User Outcome Coverage

| User evidence | Covered by FR/US | Acceptance proof | Surface / workflow |
| --- | --- | --- | --- |
| Q1: fresh sessions randomly fail some mux-routed MCP servers | FR-8, US1 | Fresh-binary runtime smoke with `time` as primary real MCP upstream and no `initialize response` closure | CLI + MCP host startup |
| Q2: mux is not a reliable multiplexer | FR-1, FR-2, FR-3, FR-4, FR-5, FR-6, FR-7 | P6-P9 production parity tests with causal guards | `muxcore` test suite + status/log evidence |
| Q3: startup problems happen constantly | FR-8, NFR-7, US1 | Runtime attach smoke after library parity | Local workstation binary |
| Q4/Q5/Q6: build proof by experimental phases, then port the correct implementation | FR-1, FR-9 | No PoC import; separate production tests per slice | SpecKit production port workflow |
| Q7: achieve result without breaking the current topology | FR-3, FR-4, FR-5, FR-10 | Refresh-before-fallback, handoff evidence, lifecycle parity, and ADR 010 in-flight contract | Shim/daemon/owner lifecycle |

## Edge Cases

- Refresh-token succeeds but fallback counter also increments: P7 must fail.
- Two concurrent requests complete out of order after reconnect: P6 must preserve
  both original IDs and deliver exactly one response for each.
- Handoff traffic succeeds but owner was freshly spawned: P8 must fail because
  restored-owner evidence is missing.
- Reaper TTL elapses while a non-persistent owner has an active session: P9 must
  keep the owner.
- Persistent owner's upstream dies: existing persistent respawn behavior must
  remain compatible with new lifecycle assertions.
- Stale pending token remains after owner reaping: P9 must fail.
- Any owner-removal path bypasses the daemon-level cleanup helper: parity tests
  must fail for missing cleanup evidence or inconsistent lifecycle counters.
- Primary `time` smoke passes but optional `serena` smoke fails: classify the
  failure as upstream-specific or muxcore-specific before blocking the feature.
- Fresh runtime smoke passes only because the old live binary is still serving:
  smoke must verify the fresh binary/version or process identity.

## Out of Scope

- Redesigning `mcp-mux` into launcher -> binary versioning.
- Introducing a new one-binary or no-daemon topology.
- Importing the PoC as production code.
- Changing default retry semantics for already-sent in-flight requests.
- Rebuilding downstream consumers such as `engram` or `aimux`; downstream proof
  remains a later release-readiness gate.
- Migrating the entire legacy `.agent/specs` registry to `_index.json`.

## Dependencies

- ADR 010: resilient shim reconnect contract.
- ADR 011: proofing-to-production port architecture.
- Proofing ledger Phase 6-9 PASS evidence.
- Existing `muxcore` packages: `owner`, `daemon`, `session`, `snapshot`, and
  `control`.
- `time` MCP server as the controlled primary runtime smoke target.
- Optional secondary `serena` smoke target after primary `time` smoke passes.

## Success Criteria

- [ ] P6 production parity test proves concurrent out-of-order demux by JSON-RPC
      ID across restart.
- [ ] P7 production parity test proves refresh-token reconnect without fallback
      spawn when the restored owner is alive.
- [ ] P8 production parity test proves generation-aware handoff or equivalent
      restored-owner evidence.
- [ ] P9 production parity test proves persistent/idle lifecycle classification,
      active-session protection, stale ticket cleanup, and persistent restore.
- [ ] Root and `muxcore` Go test suites pass with `-count=1`.
- [ ] `.\scripts\run-current-topology-poc.ps1 -WatchSeconds 1` remains green.
- [ ] Fresh `mcp-mux.exe` runtime smoke passes with `time` as the primary real
      MCP upstream.
- [ ] Optional `serena` smoke is either passing or explicitly classified as
      upstream-specific vs. muxcore-specific.

## Boundaries

Always:

- Keep `consumer -> shim -> daemon -> owner -> upstream` as the feature topology.
- Write production parity tests before production behavior changes.
- Preserve ADR 010's already-sent in-flight error-by-ID contract.
- Use the PoC as evidence, not as imported production code.

Ask First:

- Changing public `muxcore` API types.
- Removing, renaming, or changing semantics of documented status fields.
- Introducing a new deployment or launcher architecture.

Never:

- Claim production readiness from the standalone PoC alone.
- Hide fresh-spawn fallback behind successful traffic.
- Use real Claude/Codex sessions as the first proof layer.
- Change retry semantics for side-effecting calls without a new ADR.

## Open Questions

1. Resolved C1: P8/P9 generation and lifecycle evidence will be exposed as a
   full stable status/operator API contract.
2. Resolved C2: authoritative stale token/ticket cleanup lives in a single
   daemon-level owner-removal helper; session manager exposes cleanup primitives.
3. Resolved C3: primary runtime smoke target is `time`; `serena` is optional
   secondary after primary passes.
