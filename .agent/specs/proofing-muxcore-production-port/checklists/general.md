# General Requirements Quality Checklist -- Proofing to Muxcore Production Port

**Purpose:** Unit tests for requirements writing -- validates spec quality, not implementation.
**Created:** 2026-05-19
**Spec:** `.agent/specs/proofing-muxcore-production-port/spec.md`
**Plan:** `.agent/specs/proofing-muxcore-production-port/plan.md`
**Audience:** Reviewer
**Depth:** Comprehensive

## Requirement Completeness

- [x] CHK001 - Are all production parity slices named with their target invariant? [Completeness, Spec FR-1..FR-5] -- PASS: P6, P7, P8, and P9 are separate requirements with acceptance proof.
- [x] CHK002 - Are user-visible reliability goals covered by user stories? [Completeness, Spec US1..US4] -- PASS: fresh attach, restart false-positive prevention, concurrent routing, and lifecycle cleanup are covered.
- [x] CHK003 - Are implementation boundaries stated for both included and excluded work? [Completeness, Spec Boundaries/Out of Scope] -- PASS: topology, PoC import ban, launcher redesign exclusion, and downstream consumer exclusion are explicit.
- [x] CHK004 - Are plan phases mapped to every functional requirement? [Completeness, Plan Phases] -- PASS: FR-1..FR-10 are mapped across phases 1..7.

## Requirement Clarity

- [x] CHK005 - Is "stable operator status contract" defined with concrete fields or field classes? [Clarity, Spec FR-6; Plan API Contracts] -- PASS: plan names daemon, handoff, owner, and cleanup status fields.
- [x] CHK006 - Is "single daemon-level owner-removal helper" defined enough for implementation? [Clarity, Spec FR-7; Plan API Contracts] -- PASS: plan provides helper shape, result data, and wrapper compatibility.
- [x] CHK007 - Is "time primary, serena optional" unambiguous? [Clarity, Spec FR-8; Clarification C3] -- PASS: `time` gates core smoke; `serena` is secondary only after `time` passes.
- [x] CHK008 - Is ADR 010 replay behavior preserved without ambiguity? [Clarity, Spec FR-10; Plan Phase 2] -- PASS: already-sent in-flight requests keep error-by-ID, not blind replay.

## Requirement Consistency

- [x] CHK009 - Are spec, ADR 011, and plan aligned on no additional PoC phases before production porting? [Consistency, Spec FR-9; Plan Contract Check] -- PASS: all artifacts keep the PoC as oracle only.
- [x] CHK010 - Are runtime smoke requirements consistent between spec and plan? [Consistency, Spec FR-8; Plan Phase 7] -- PASS: both require fresh-binary smoke with `time` before optional `serena`.
- [x] CHK011 - Are lifecycle cleanup requirements consistent between clarification and plan? [Consistency, Clarification C2; Plan Phase 1/5] -- PASS: daemon owns lifecycle decisions and session manager exposes primitives.

## Acceptance Criteria Quality

- [x] CHK012 - Can P6 parity be objectively measured? [Measurability, Spec FR-2; Plan Phase 3] -- PASS: original IDs, response order, exact-once delivery, and generation evidence are required.
- [x] CHK013 - Can P7 parity be objectively measured? [Measurability, Spec FR-3; Plan Phase 2] -- PASS: original token, refreshed token, refresh counter, fallback counter, and owner-alive evidence are required.
- [x] CHK014 - Can P8 parity reject false-positive fresh-spawn success? [Measurability, Spec FR-4; Plan Phase 4] -- PASS: restored-owner evidence and predecessor/successor data are required.
- [x] CHK015 - Can P9 parity be measured without production-duration sleeps? [Measurability, Spec FR-5; Plan Phase 5] -- PASS: controlled TTLs, counters, active session state, and generation evidence are required.
- [x] CHK016 - Can release readiness be evaluated from written gates? [Measurability, Spec NFR-7; Plan Phase 6/7] -- PASS: muxcore tests, root tests, PoC runner, and runtime smoke are listed.

## Scenario Coverage

- [x] CHK017 - Are primary success scenarios covered? [Coverage, Spec User Outcome Coverage] -- PASS: new-session attach and restart/lifecycle reliability map to FR and US entries.
- [x] CHK018 - Are failure-path scenarios covered? [Coverage, Spec Edge Cases] -- PASS: fallback counter drift, fresh-spawn masquerade, stale tokens, old binary smoke, and optional `serena` failure are covered.
- [x] CHK019 - Are topology-preservation scenarios covered? [Coverage, Spec Boundaries; Plan Architecture] -- PASS: consumer -> shim -> daemon -> owner -> upstream remains the active seam.

## Edge Case Coverage

- [x] CHK020 - Does the spec cover stale pending tickets after owner reaping? [Edge Cases, Spec Edge Cases; Plan Phase 5] -- PASS: stale pending token after reaping is a named failure condition.
- [x] CHK021 - Does the spec cover active non-persistent sessions during idle TTL? [Edge Cases, Spec FR-5; Plan Phase 5] -- PASS: active sessions must survive and reap only after close.
- [x] CHK022 - Does the spec cover optional secondary smoke failure classification? [Edge Cases, Spec Edge Cases] -- PASS: `serena` failure must be classified as upstream-specific or muxcore-specific.

## Non-Functional Requirements

- [x] CHK023 - Are transparency and protocol limits specified? [NFRs, Spec NFR-1; Plan Source Requirements] -- PASS: no new synthetic MCP messages and MCP/JSON-RPC verification requirements are stated.
- [x] CHK024 - Are cross-platform constraints specified? [NFRs, Spec NFR-2; Plan Constitution Compliance] -- PASS: platform-specific assertions require isolation or build tags.
- [x] CHK025 - Are compatibility expectations specified for status and muxcore consumers? [NFRs, Spec NFR-5/NFR-6; Plan API Contracts] -- PASS: additions are additive and status changes require compatibility policy.

## Dependencies & Assumptions

- [x] CHK026 - Are dependency inputs documented? [Dependencies, Spec Dependencies; Plan Tech Stack] -- PASS: ADRs, proofing ledger, muxcore packages, and `time` upstream are listed.
- [ ] CHK027 - Is the exact workstation `time` smoke command fully documented? [Dependencies, Assumption] -- OPEN: spec and plan identify `time` as the primary target, but the final command/script contract is intentionally left to the implementation task.
- [x] CHK028 - Are release-readiness dependencies acknowledged? [Dependencies, Plan Unknowns And Risks] -- PASS: missing critical suite/playbook are named as release blockers outside the parity path.

## Ambiguities & Conflicts

- [x] CHK029 - Are all `[NEEDS CLARIFICATION]` markers resolved? [Ambiguities, Spec Open Questions] -- PASS: C1-C3 are resolved and no unresolved markers remain in `spec.md`.
- [x] CHK030 - Are any plan decisions in conflict with the constitution? [Conflict, Plan Constitution Compliance] -- PASS: plan maps to transparent proxy, zero-config, upstream authority, session isolation, cross-platform, and test-before-ship principles.
