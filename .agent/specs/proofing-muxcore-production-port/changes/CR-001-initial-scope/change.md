---
feature_id: ~
created_in_session: unknown
change_id: CR-001
slug: initial-scope
status: Draft
created: 2026-05-19
---

# CR-001: Initial Scope

## Summary

Create the production parity port for proofing Phase 6-9 invariants in real
`muxcore`. This CR covers the specification boundary only; implementation tasks
belong to `nvmd-plan`, `nvmd-tasks`, and `nvmd-implement`.

## Scope

- P6 concurrent out-of-order demux parity.
- P7 refresh-token reconnect parity.
- P8 generation-aware handoff parity.
- P9 persistent/idle lifecycle parity.
- Runtime smoke after parity tests.

## Acceptance

- `spec.md` records FR/NFR/user stories/edge cases/boundaries.
- Open questions are limited to three clarification items.
- ADR 010 and ADR 011 remain the decision sources for reconnect and porting
  boundaries.
