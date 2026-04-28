# Clarification Report — muxcore-oninject-callback

**Date:** 2026-04-28
**Mode:** `--auto` (auto-applied recommended answers)
**Spec:** `.agent/specs/muxcore-oninject-callback/spec.md`

## Taxonomy Coverage

| # | Category | Status | Notes |
|---|----------|--------|-------|
| 1 | Functional Scope | Clear | 7 FRs trace to US1-US3; explicit Out of Scope (6 items) |
| 2 | User Roles | Clear | aimux integrator (US1), muxcore consumer (US2), non-adopting maintainer (US3) |
| 3 | Domain/Data Model | Clear | API surface = 2 struct fields + 2 sentinel errors |
| 4 | Data Lifecycle | Clear | Single-fire (FR-4), close semantics (FR-5), buffer-during-reconnect inherited |
| 5 | Interaction & UX Flow | N/A | Library API, no UX |
| 6 | Perf/Scale | Clear | NFR-1 (non-blocking, sub-microsecond), NFR-2 (~16 bytes/instance, zero allocs hot path) |
| 7 | Reliability | **Resolved (C1)** | Panic semantics codified — propagates per EC-6; no internal recover |
| 8 | Security | **Resolved (C2)** | Trust boundary at `engine.Config` construction; daemon-side validation responsibility |
| 9 | Integration | Clear | aimux `engram#178` downstream consumer; mcp-launcher non-adopting |
| 10 | Edge Cases | Clear | EC-1..EC-8 cover handshake, reconnect, saturation, close race, malformed frames, panic, concurrent injects, unknown method |
| 11 | Constraints & Tradeoffs | **Resolved (C3)** | Priority lane / second socket / new mutex rejection rationale codified |
| 12 | Terminology | Clear | OnInject / inject closure / msgFromCC consistent across all sections |
| 13 | Completion Signals | Clear | Success Criteria all binary measurable |
| 14 | Miscellaneous | Clear | 0 TODO/TBD/[NEEDS CLARIFICATION] markers |

## Clarification Summary

**Questions asked/answered:** 3/5 (auto-applied)
**Spec status:** Ready for planning

**Resolved:** Reliability (C1), Security (C2), Constraints (C3)
**Clear (no action):** 11 categories
**Outstanding:** 0

## Auto-applied Resolutions

### C1 (Reliability) — panic in OnInject callback

Recommended path: callback runs on proxy goroutine; panic propagates and tears down proxy (consistent with `RefreshToken` panic semantics). Consumer responsible for `recover()` if panic-tolerant. Codified in EC-6. **No internal recover wrapper** — keeps signature simple and behavior consistent with other config callbacks.

### C2 (Security) — attack surface widening

Recommended path: trust boundary is `engine.Config` construction itself. Setting `OnInject` requires same privilege as setting `Handler` or `SessionHandler` — anyone with that privilege already controls the process. Daemon-side handler (`LogIngester` for aimux) validates frame schema independently. **No new authn/authz primitive needed.**

### C3 (Constraints) — priority lane rejection rationale

Recommended path: codified rationale for FR-2's "no priority lane" decision:
- Priority lane breaks FIFO ordering — daemon handlers receive interleaved request/notification frames in non-deterministic order
- Second socket doubles fd count + reconnect bookkeeping for marginal gain
- Existing `msgFromCC` is the simplest correct path

Decision already in Out of Scope ("Priority lane / out-of-band frames"); rationale now explicit.

## Forward

Auto-forward to `nvmd-plan` for `muxcore-oninject-callback`.
