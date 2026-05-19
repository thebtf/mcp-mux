# Clarification Summary -- proofing-muxcore-production-port

Date: 2026-05-19

Spec: `.agent/specs/proofing-muxcore-production-port/spec.md`

## Questions Asked

| # | Category | Question | Resolution |
| --- | --- | --- | --- |
| C1 | Observability / Public Surface | Should P8/P9 generation evidence be exposed through stable status fields, or should tests use package-private hooks only? | Full stable status contract: expose all generation and lifecycle evidence as documented operator API. |
| C2 | Lifecycle Cleanup Ownership | Where should stale pending-token cleanup for reaped owners live? | A single daemon-level owner-removal helper is authoritative; session manager exposes cleanup primitives but does not decide owner lifecycle. |
| C3 | Runtime Smoke Target | Which real MCP upstream is the canonical runtime smoke target for this workstation? | `time` is the primary smoke target; `serena` is optional secondary after primary passes. |

## Coverage

| Category | Status | Notes |
| --- | --- | --- |
| Functional Scope | Resolved | P6-P9 parity slices, stable status contract, unified cleanup, and runtime smoke target are explicit. |
| User Roles | Clear | User/operator/maintainer roles are covered by US1-US4. |
| Domain/Data Model | Clear | DDD not needed; runtime entities are Owner, Session, Token, Snapshot, Handoff, Reaper, and Shim. |
| Data Lifecycle | Resolved | C2 resolves cleanup ownership for owner lifecycle artifacts. |
| Interaction & UX Flow | Clear | User-visible workflow is fresh-session attach and runtime smoke, not UI interaction. |
| Non-Functional: Perf/Scale | Clear | Test runtime bound and deterministic evidence requirements are stated. |
| Non-Functional: Reliability | Resolved | Reliability is gated by production parity tests and runtime smoke. |
| Non-Functional: Security | Clear | No new auth/secrets surface; stable status fields are additive operator API. |
| Integration | Resolved | C3 fixes primary runtime integration target as `time`; `serena` is optional secondary. |
| Edge Cases | Clear | False-positive handoff, stale pending token, bypassed cleanup helper, and old-binary smoke are covered. |
| Constraints & Tradeoffs | Resolved | Current topology, ADR 010 retry contract, no PoC import, and no launcher redesign are fixed boundaries. |
| Terminology | Clear | Canonical terms remain shim, daemon, owner, upstream, handoff, reaper, reconnect, and status. |
| Completion Signals | Clear | Success criteria include P6-P9 tests, root/muxcore tests, PoC runner, and primary runtime smoke. |
| Miscellaneous | Clear | No remaining `[NEEDS CLARIFICATION]` markers after this pass. |

## Status

Questions asked/answered: 3/5.

Spec status: Ready for planning.

Remaining blockers: none at clarification level.

Planning handoff:

- Treat stable status fields as public operator API.
- Implement owner lifecycle cleanup through a single daemon-level owner-removal helper.
- Use `time` as the primary runtime smoke target; run `serena` only as optional secondary after `time` passes.
