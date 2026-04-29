# API Requirements Quality Checklist — muxcore Multi-Tenant Extensions

**Purpose:** Unit tests for requirements writing — validates spec quality, not implementation.
**Created:** 2026-04-29
**Spec:** `.agent/specs/muxcore-multi-tenant-extensions/spec.md`
**Plan:** `.agent/specs/muxcore-multi-tenant-extensions/plan.md`
**Audience:** Reviewer (PR)
**Depth:** Standard

## Requirement Completeness

- [ ] CHK001 - Are public types `ConnInfo`, `SessionMeta`, `SessionAuth`, `FrameAction` defined with full field lists, types, and zero-value semantics? [Spec §FR-1, FR-2, FR-3, FR-4 — Completeness]
- [ ] CHK002 - Are documentation requirements specified for every exported type and field (godoc with usage example)? [Gap — Completeness]
- [ ] CHK003 - Is the `peerCreds` extractor specified for all 3 supported platforms (Linux + Windows + Darwin) with explicit fallback when extraction fails? [Spec §FR-5 — Completeness]
- [ ] CHK004 - Is dispatch precedence between `*WithSessionMeta` and legacy interfaces unambiguous (single-path resolution, no double-dispatch)? [Spec §EC-7 — Completeness]

## Requirement Clarity

- [ ] CHK005 - Is "byte-identical to v0.23.x" quantified — what observables are checked (log markers, JSON fields, message ordering, accept-loop latency)? [Spec §NFR-3 — Ambiguity]
- [ ] CHK006 - Is "single source of peer identity per session" defined formally — same struct value reference or same field values byte-equal? [Spec §FR-2 — Ambiguity]
- [ ] CHK007 - Is fail-open semantic for OnFrameReceived timeout specified at goroutine cancellation level (does the original callback continue in background)? [Spec §FR-4 — Ambiguity]

## Requirement Consistency

- [ ] CHK008 - Are JSON-RPC error codes (-32000 for AuthDeny, -32004 for FrameError) consistent with existing muxcore error code conventions in the codebase? [Spec §FR-3, FR-4 — Consistency]
- [ ] CHK009 - Is the SessionMeta lifetime model (mutate-once-then-immutable) consistent with concurrent handler invocation on multiple goroutines (no read-after-write race)? [Spec §FR-6 — Consistency]

## Acceptance Criteria Quality

- [ ] CHK010 - Are NFR-1 (200µs accept overhead) and NFR-2 (1ms OnFrameReceived budget) thresholds testable via `go test -bench` benchmarks with explicit p99 measurement points? [Spec §NFR-1, NFR-2 — Measurability]
- [ ] CHK011 - Are Success Criteria binary (pass/fail) with defined measurement points and CI verification? [Spec §Success Criteria — Measurability]

## Scenario Coverage

- [ ] CHK012 - Is dispatch behavior specified for handlers implementing only `*WithSessionMeta`, only legacy `SessionHandler`, or both interfaces simultaneously? [Spec §EC-7, US1, US2 — Coverage]
- [ ] CHK013 - Is behavior specified when AuthorizeSession returns `AuthAllow{TenantID: ""}` (allow but no tenant assigned)? [Gap — Coverage]
- [ ] CHK014 - Is the scope of OnFrameReceived explicitly limited to inbound (client→server) frames, NOT outbound (server→client) responses or notifications? [Gap — Coverage]

## Edge Case Coverage

- [ ] CHK015 - Is shutdown ordering specified for in-flight AuthorizeSession callbacks when daemon receives SIGTERM/Shutdown — does the callback complete or get cancelled? [Gap — Edge Cases]
- [ ] CHK016 - Is concurrent OnFrameReceived behavior specified across multiple sessions sharing one Owner (per-session reader, single Config callback)? [Spec §FR-4 — Edge Cases]
- [ ] CHK017 - Is connection-close-during-AuthorizeSession behavior specified (peer disconnects while callback is running)? [Spec §EC-1 — Edge Cases]

## Non-Functional Requirements

- [ ] CHK018 - Are observability requirements complete — every new code path emits a structured log marker AND a counter in HandleStatus JSON? [Spec §NFR-6 — NFRs]
- [ ] CHK019 - Is the security threat model documented for AuthorizeSession bypass scenarios (panic recovery, timeout, callback returning corrupted SessionAuth)? [Spec §EC-2 — NFRs]
- [ ] CHK020 - Are CI runner requirements specified for all 3 platforms (existing Linux/Windows/Darwin matrix from previous releases)? [Spec §NFR-4 — NFRs]
- [ ] CHK021 - Are failure-mode requirements specified when peer-creds extraction fails — does AuthorizeSession still get called with zero ConnInfo, and is consumer expected to handle? [Spec §EC-1, EC-9 — NFRs]

## Dependencies & Assumptions

- [ ] CHK022 - Is the `windows.GetNamedPipeClientProcessId` availability assumption documented with primary + fallback path (x/sys/windows OR NewLazySystemDLL)? [Spec §C2 — Dependencies]
- [ ] CHK023 - Is the winio `Fd()` interface assertion stability documented as a known risk with version-pin requirement? [Spec §EC-9, R2 — Dependencies]
- [ ] CHK024 - Is the snapshot/handoff (v0.21.0) interaction documented — SessionMeta does NOT survive handoff, re-attaching shims trigger fresh AuthorizeSession on new owner? [Spec §Out of Scope — Dependencies]

## Ambiguities & Conflicts

- [ ] CHK025 - Is "single-shot" in AuthorizeSession defined precisely — one call per session lifetime, or one call per token bind (relevant for token-refresh scenarios)? [Spec §FR-3 — Ambiguity]
- [ ] CHK026 - Is it explicit that AuthorizeSession callback receives `ConnInfo` (NOT `SessionMeta`) because at authorize time TenantID is what the callback PRODUCES? [Spec §FR-3 — Ambiguity, resolved]
- [ ] CHK027 - Is "session lifetime" defined for SessionMeta caching — until client disconnect, until session expulsion from owner.sessions map, or until owner shutdown? [Spec §FR-6 — Ambiguity]
