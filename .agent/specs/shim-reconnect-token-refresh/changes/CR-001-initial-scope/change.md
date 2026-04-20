---
cr_id: CR-001
feature_id: NVMD-140
slug: initial-scope
title: Initial implementation scope — token refresh endpoint + shim fallback
status: OPEN
created: 2026-04-20
---

# CR-001: Initial Implementation Scope

## Summary

Initial materialization of the F2 feature (NVMD-140). This CR covers the
full initial implementation scope defined by FR-1 through FR-6 and
NFR-1 through NFR-5 in `../../spec.md`.

Subsequent CRs will be opened only if follow-up changes emerge after this
scope lands on master (e.g., operator feedback, metric tuning, legacy-shim
handling refinements).

## Scope

In scope for CR-001 (= current entire spec.md body):

- **FR-1** — New daemon control-plane method `RefreshSessionToken(prev_token) → new_token`.
- **FR-2** — Shim rejection-counter fallback to fresh `spawn` after `N=3` consecutive
  `invalid/missing token` rejections.
- **FR-3** — RefreshToken-first, spawn-fallback order on reconnect.
- **FR-4** — `RefreshSessionToken` validates the previous token was bound to a
  still-alive owner; returns `ErrOwnerGone` otherwise.
- **FR-5** — `HandleStatus` metric counters:
  `shim_reconnect_refreshed`, `shim_reconnect_fallback_spawned`,
  `shim_reconnect_gave_up`.
- **FR-6** — Structured log markers:
  `shim.reconnect.refresh_ok`, `shim.reconnect.refresh_fail reason=<>`,
  `shim.reconnect.fallback_spawn owner=<sid>`.
- **NFR-1..5** — Stdlib-only, <100 ms latency, full unit + integration test coverage,
  back-compat with v0.20.4 shims.
- **SC-1..4** — All four success criteria covered by the implementation.

## Rationale

The spec was filed as a single coherent design (forensic investigation →
feature spec → implementation). Splitting it into multiple CRs at this
stage would add tracking overhead without engineering value: all listed
FRs/NFRs are interlocking and ship as one change.

## References

- Spec: `../../spec.md` (NVMD-140, v1)
- Forensic data: `../../../../debug/daemon-idle-hang/investigation-pr-reconnect.md`
- Engram issue: `engram://mcp-mux#137`
