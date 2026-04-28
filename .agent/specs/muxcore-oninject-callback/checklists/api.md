# API Requirements Quality Checklist — muxcore-oninject-callback

**Purpose:** Unit tests for requirements writing — validates spec quality, not implementation.
**Created:** 2026-04-28
**Spec:** `.agent/specs/muxcore-oninject-callback/spec.md`
**Plan:** `.agent/specs/muxcore-oninject-callback/plan.md`
**Audience:** Reviewer (PR)
**Depth:** Standard

## Requirement Completeness

- [ ] CHK001 - Are all 7 functional requirements (FR-1..7) traced to at least one user story (US1, US2, or US3)? [Completeness, Spec §FR/§User Stories]
- [ ] CHK002 - Are sentinel error semantics (`ErrInjectFull`, `ErrInjectClosed`) defined for every failure path the closure can encounter? [Completeness, Spec §FR-3]
- [ ] CHK003 - Is the lifecycle of the `OnInject` callback fully specified across CONNECTED, RECONNECTING, and exit states? [Completeness, Spec §FR-4/§FR-5/§EC-2]

## Requirement Clarity

- [ ] CHK004 - Is "single-fire" explicitly distinguished from "fires on each reconnect" with a measurable invariant? [Clarity, Spec §FR-4]
- [ ] CHK005 - Is "non-blocking" quantified beyond the prose ("sub-microsecond") with a concrete test condition? [Clarity, Spec §NFR-1]
- [ ] CHK006 - Is "single-fire across reconnects" semantics unambiguous about what happens to the closure handle the consumer holds after the first fire? [Clarity, Spec §FR-4/§FR-5]

## Requirement Consistency

- [ ] CHK007 - Are `ResilientClientConfig.OnInject` and `engine.Config.OnInject` field signatures byte-identical (`func(inject func([]byte) error)`)? [Consistency, Spec §FR-1/§FR-6]
- [ ] CHK008 - Are FR-2 ("no priority lane") and Out of Scope ("Priority lane / out-of-band frames") aligned with no contradiction? [Consistency, Spec §FR-2/§Out of Scope]
- [ ] CHK009 - Are NFR-2 ("zero allocations on hot path when nil") and the implementation contract in plan.md compatible? [Consistency, Spec §NFR-2 / Plan §Architecture]

## Acceptance Criteria Quality

- [ ] CHK010 - Are US1, US2, US3 acceptance criteria binary (each criterion has a single pass/fail outcome)? [Measurability, Spec §User Stories]
- [ ] CHK011 - Are Success Criteria items each tied to an executable check (build/vet/test/release) rather than subjective? [Measurability, Spec §Success Criteria]
- [ ] CHK012 - Is NFR-5 test count requirement (≥2 regression tests) sufficient to validate FR-1..FR-5 coverage, or is a stronger floor needed? [Coverage, Spec §NFR-5]

## Scenario Coverage

- [ ] CHK013 - Are concurrent injects from N goroutines under saturation pressure addressed in requirements? [Coverage, Spec §EC-7]
- [ ] CHK014 - Are cross-platform (Linux, macOS, Windows) test obligations specified for the regression suite? [Coverage, Spec §NFR-5]
- [ ] CHK015 - Is the buffer-during-reconnect behavior for injected frames inherited from existing tests, with a forward link to that contract? [Coverage, Spec §EC-2]

## Edge Case Coverage

- [ ] CHK016 - Does the spec define behavior when the consumer's `OnInject` callback panics during invocation? [Edge Cases, Spec §EC-6/§Clarifications C1]
- [ ] CHK017 - Does the spec define behavior when `inject(b)` is called concurrent with proxy `Close()`? [Edge Cases, Spec §EC-4]
- [ ] CHK018 - Does the spec define behavior when malformed (non-JSON-RPC) bytes are passed to `inject(b)`? [Edge Cases, Spec §EC-5]

## Non-Functional Requirements

- [ ] CHK019 - Are performance requirements (NFR-1 latency, NFR-2 memory) quantified with measurable thresholds? [NFRs, Spec §NFR-1/§NFR-2]
- [ ] CHK020 - Are observability requirements (NFR-3 log markers) specified with named markers (`proxy.inject.armed`, `.delivered`, `.dropped`)? [NFRs, Spec §NFR-3]
- [ ] CHK021 - Are security requirements addressed (trust boundary explicitly clarified — does adding `OnInject` widen the attack surface)? [NFRs, Spec §Clarifications C2]

## Dependencies & Assumptions

- [ ] CHK022 - Is the downstream consumer dependency (aimux `engram#178`) documented with a concrete integration point (≤20 LOC wiring)? [Dependencies, Spec §Dependencies/§US1]
- [ ] CHK023 - Is the reference patch (gist `af3b003`) cited with a stable, retrievable URL? [Dependencies, Spec §header]
- [ ] CHK024 - Are non-adopting consumers (mcp-launcher, engram) explicitly tested for byte-identical behavior? [Dependencies, Spec §US3]

## Ambiguities & Conflicts

- [ ] CHK025 - Is the choice between `sync.Once` vs `atomic.Bool` for single-fire enforcement explicitly deferred to implementation, or already pinned? [Ambiguity, Plan §Library Decisions]
- [ ] CHK026 - Is the SemVer bump rationale (MINOR `v0.23.0` vs PATCH `v0.22.2`) documented and unambiguous? [Ambiguity, Plan §Reversibility D5]
