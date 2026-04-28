# Validation — muxcore-oninject-callback

**Date:** 2026-04-28
**Mode:** Pipeline (full directory consistency analysis)
**Artifact:** `.agent/specs/muxcore-oninject-callback/`
**Files:** spec.md, plan.md, tasks.md, checklists/api.md, completeness-report.md, clarification-report-2026-04-28.md

## Verdict: **PASS** (1 LOW warning)

## A. Structural validation

### spec.md

| Section | Present | Notes |
|---------|---------|-------|
| `# Feature: ...` | ✓ | |
| `## Overview` | ✓ | 2 sentences |
| `## Context` | ✓ | + FM-10 evidence anchor (verbatim issue quote) |
| `## Functional Requirements` (FR-1..7) | ✓ | 7 FRs, each with traceability |
| `## Non-Functional Requirements` (NFR-1..6) | ✓ | 6 NFRs, all measurable |
| `## User Stories` (US1-3) | ✓ | P1/P2/P1 priorities, AC binary |
| `## Edge Cases` (EC-1..8) | ✓ | 8 cases |
| `## Out of Scope` | ✓ | 6 items |
| `## Dependencies` | ✓ | aimux engram#178, gist `af3b003` |
| `## Success Criteria` | ✓ | 7 binary checks |
| `## Open Questions` | ✓ | empty (intentional — patch is complete) |
| `## Clarifications` | ✓ | C1, C2, C3 from auto-clarify |
| `## Domain Modeling` | ✓ | "DDD evaluated — not needed" rationale |
| `## Strangler Fig` | ✓ | "Not applicable" rationale |

### plan.md

| Section | Present | Notes |
|---------|---------|-------|
| `# Title` | ✓ | |
| `## Tech Stack` | ✓ | |
| `## Architecture` + Reversibility Decision Table | ✓ | 5 decisions classified, REVERSIBILITY_AUDIT: PASS |
| `## Data Model` | ✓ | |
| `## API Contracts` | ✓ | closure contract, single-fire invariant, concurrent safety |
| `## File Structure` | ✓ | |
| `## Phases` (1, 2, 3) | ✓ | with Concurrent Work Directives + Contingency Branches |
| `## Library Decisions` | ✓ | |
| `## Reusability Awareness` | ✓ | 1 candidate (Draft) |
| `## Domain Modeling` | ✓ | "not needed" |
| `## Unknowns and Risks` | ✓ | R1-R4, all LOW/MEDIUM |
| `## Constitution Compliance` | ✓ | 6/6 PASS |
| `## Plan Validation Checklist` | ✓ | 11/11 [x] |

### tasks.md

| Section | Present | Notes |
|---------|---------|-------|
| User-value anchors | ✓ | Phase 0 PHASE_0_COMPLETE |
| `## Phase 1` (US3+US2 foundation) | ✓ | T001-T003 |
| `## Phase 2` (delivery) | ✓ | T004-T005 + G001 GATE |
| `## Phase 3` (release) | ✓ | T006-T009 |
| Concurrent Execution Map | ✓ | |
| Dependencies | ✓ | |
| Suggested MVP scope | ✓ | |
| Per-task AC + verification method | ✓ | all 9 T tasks |
| GATE task (G001) | ✓ | RUN/CHECK/ENFORCE/RESOLVE/SAVE |
| `IF-WRONG:` directives | ✓ | T003, T006, T007 (IRREVERSIBLE/PARTIALLY REVERSIBLE) |
| `[P]` provenance | ✓ | T005 PARALLEL-WITH T004 (graph evidence cited) |

## B. Cross-artifact consistency

### Spec FR → Plan/Tasks coverage

| FR | Plan ref | Task ref |
|----|----------|----------|
| FR-1 (OnInject field on ResilientClientConfig) | §Data Model, §Architecture | T001+T002 (RED), T003 (GREEN) |
| FR-2 (msgFromCC reuse, no priority lane) | §Architecture, D3 Reversibility | T003 |
| FR-3 (ErrInjectFull/ErrInjectClosed) | §Data Model, D4 Reversibility | T002, T003 |
| FR-4 (single-fire) | §API Contracts, D2 Reversibility | T001 (assertion), T003 (sync.Once) |
| FR-5 (lifecycle-aware) | §Architecture | T003 (closed flag) |
| FR-6 (engine.Config passthrough) | §Architecture component map | T004 |
| FR-7 (zero-source-change back-compat) | §Constitution Compliance | T004 AC, G001 ENFORCE |

### Spec NFR → Plan/Tasks coverage

| NFR | Plan ref | Task ref |
|-----|----------|----------|
| NFR-1 (non-blocking, sub-microsecond) | §Architecture closure | T002 (BufferFull test asserts <1ms) |
| NFR-2 (zero allocations on hot path) | §Library Decisions | T003 AC ("verify zero-allocation path") |
| NFR-3 (log markers `proxy.inject.armed/.delivered/.dropped`) | not explicitly named | **WARNING** — see below |
| NFR-4 (concurrent safe) | §API Contracts | T001 implicit |
| NFR-5 (≥2 regression tests) | §Phases | T001 + T002 |
| NFR-6 (AGENTS.md docs) | §Phases | T005 |

### User Story → Phase mapping

| US | Priority | Phase |
|----|----------|-------|
| US3 (zero source change) | P1 | Phase 1 + Phase 2 (T004 AC + G001 ENFORCE) |
| US2 (consumer pushes telemetry) | P2 | Phase 1 (T003) + Phase 2 (T004) |
| US1 (aimux logger wires API) | P1 | Phase 3 (release unblocks downstream — out-of-repo) |

Phase ordering follows P1→P2→P1: US3 + US2 deliver in Phase 1+2 (mcp-mux scope); US1 enabled in Phase 3 (downstream consumption — out-of-scope). Anchor for value sequence is in tasks.md header.

## C. Spec Identity (FR-9 / FR-11)

- spec.md frontmatter: legacy slug-only (no `feature_id` field)
- `.agent/specs/_index.json`: absent
- → **Legacy fallback (NFR-1):** migration offer issued; structural checks proceed; Spec Identity (a)-(e) skipped
- No relationship fields, no registry, no cycle check needed

## D. Unverified claims scan

- `UNVERIFIED:` markers: 0
- Library version pins: N/A (stdlib only — no third-party)
- API method cross-references: all symbols (`OnInject`, `ErrInjectFull`, `ErrInjectClosed`, `msgFromCC`, `runIPCWriter`, `flushBuffer`, `runProxy`) cross-referenced ≥2 places (spec + plan + tasks)
- gist URL stable + cited

## E. Warnings

**W1 (LOW):** NFR-3 (log markers `proxy.inject.armed`, `proxy.inject.delivered`, `proxy.inject.dropped`) is specified but not explicitly enumerated in any task AC. Implementation MAY add markers as part of T003 production code, but the spec-to-task trace is implicit, not explicit.

**Recommended action (not blocking):** during implementation of T003, add log markers per NFR-3. No spec amendment needed — NFR-3 is unambiguous; the gap is task-level documentation, not requirement quality.

## F. Verification method coverage

| Task | Method (12-method taxonomy) | Evidence |
|------|----------------------------|----------|
| T001 | Tests | failing test compile output |
| T002 | Tests | failing test compile output |
| T003 | Tests + lint + type checks | go test green, go vet clean |
| T004 | Tests + lint + type checks | go build/vet/test green |
| T005 | Document existence | AGENTS.md diff |
| G001 | Command output | full muxcore suite output |
| T006 | Human approval (review) + command output (CI) | PR review + CI green |
| T007 | Command output + artifact checksum | git ls-remote --tags origin |
| T008 | API calls (gh release create) | gh release view |
| T009 | API calls (gh issue close) | gh issue view --json state |

## G. Pipeline STOP

This is a designated gap point per nvmd-validate contract. Do NOT auto-invoke `/nvmd-implement`. Validation reports PASS; user / autopilot decides GO.

## H. Next step (manual GO required)

User input said «и далее автоматически к реализации» — this requests autopilot dispatch through the gap. Operator decision needed:

1. **GO** → `Skill("nvmd-platform:nvmd-implement", "muxcore-oninject-callback")` to execute T001..T009
2. **REVIEW** → user reviews spec.md/plan.md/tasks.md before implementation
3. **AMEND** → if any spec/plan adjustment desired
