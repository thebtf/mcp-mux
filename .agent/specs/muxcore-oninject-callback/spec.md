# Feature: muxcore engine.Config.OnInject — fire-and-forget IPC frame injection

**Slug:** muxcore-oninject-callback
**Created:** 2026-04-28
**Status:** Draft
**Author:** AI Agent (Opus 4.7)
**Source:** [thebtf/mcp-mux#107](https://github.com/thebtf/mcp-mux/issues/107)
**Reference patch:** [gist af3b003539f22e3d9c4c552a9faf92bb](https://gist.github.com/thebtf/af3b003539f22e3d9c4c552a9faf92bb) (3 files, +260 LOC, additive)

> **Provenance:** Specified by Opus 4.7 on 2026-04-28.
> Evidence from: GitHub #107 issue body, AGENTS.md v0.21.x/v0.22.x lineage, codebase exploration of `muxcore/owner/resilient_client.go` + `muxcore/engine/engine.go`.
> Confidence: VERIFIED (issue body, gist patch, codebase search results all anchored to file:line).
> Phase 0 skipped — technical API request with ready diff, downstream consumer (aimux) explicit, no UX ambiguity.

## Overview

Expose a single-fire callback through `engine.Config.OnInject` (forwarded to `ResilientClientConfig.OnInject`) that lets muxcore consumers push raw JSON-RPC frames into the daemon-bound IPC channel without exposing internal proxy state, opening a second socket, or competing with the request/response cycle. Primary motivation: aimux centralized logging (downstream `engram#178`) — shim subprocess emits log entries via its own logger; entries need to land in the daemon's central `aimux.log` (sole-writer invariant).

## Context

The current `ResilientClient` proxy implements a strict request/response model: CC stdin → `msgFromCC` chan → `runIPCWriter` → daemon IPC. There is no public path for a consumer wrapping the engine to push notifications outside the request cycle.

Aimux centralized logging needs exactly this: shim subprocess produces log entries (`IPCSink` in aimux logger) that must reach the daemon's `LogIngester` (already wired aimux-side). Without a callback, shim's `IPCSink` falls back to stderr — log entries from N concurrent shims are interleaved on the operator's terminal instead of converging in `aimux.log`.

The same primitive helps any consumer pushing notifications outside request scope (telemetry, progress hints, sidecar metrics).

> **Evidence anchor (FM-10 guard):** Issue #107 body verbatim — *"shim subprocess emits log entries via its own logger; entries need to land in the daemon's central `aimux.log` (sole-writer invariant). Today shim's `IPCSink` has nowhere to send — falls back to stderr."*

The patch is feature-complete + tested locally, attached as gist `af3b003`. Earlier today (2026-04-28) the author mistakenly pushed a feature branch + alpha tag (`muxcore/v0.23.0-alpha.1`) directly to this repo — both deleted. Filing as issue is the corrected path.

## Functional Requirements

### FR-1: ResilientClientConfig exposes OnInject callback field

Add `OnInject func(inject func([]byte) error)` to `muxcore/owner/resilient_client.go::ResilientClientConfig`. When non-nil, the muxcore proxy invokes it exactly once after the initial IPC handshake completes, passing a closure that pushes raw JSON-RPC frames into `msgFromCC`. Zero value (`nil`) preserves pre-v0.23 behavior — no callback, no extra goroutines, no allocations beyond the existing struct field.

Trace: gist patch file `muxcore/owner/resilient_client.go`; consumer use case `engram#178` (aimux centralized logging).

### FR-2: Inject closure pushes through existing msgFromCC channel

The closure provided to `OnInject` writes through `select { case msgFromCC <- frame: default: return ErrInjectFull }`. Frames travel the same path as proxy stdin → IPC writes — no priority lane, no second mutex, no separate goroutine. In `CONNECTED` state frames go through `runIPCWriter`; in `RECONNECTING` state they sit in `flushBuffer` until reconnect succeeds, identical to the existing buffered-during-reconnect contract.

Trace: existing `msgFromCC chan []byte` (buffered 1000) in `resilient_client.go:88`; existing flush-on-reconnect path in `flushBuffer` at `resilient_client.go:681`.

### FR-3: Sentinel errors for closed/full conditions

Export `ErrInjectFull` (msgFromCC buffer saturated — transient backpressure) and `ErrInjectClosed` (proxy exited — further injects are no-ops). Closure returns these directly so the caller decides drop-vs-retry policy. No internal retry, no internal drop.

Trace: gist patch `var ( ErrInjectFull = errors.New(...); ErrInjectClosed = errors.New(...) )`.

### FR-4: Single-fire semantics across reconnects

`OnInject` fires exactly once per `ResilientClient` lifecycle, after the FIRST successful handshake. Subsequent reconnects do NOT re-fire — the buffer is per-client, not per-connection, so frames queued during disconnect survive into the next CONNECTED state without re-arming the callback.

Trace: gist enforces via internal `sync.Once` or boolean flag; ensures the consumer's logger registers its `SendFunc` once and stops worrying about reconnect lifecycle.

### FR-5: Lifecycle-aware closure

The closure honors proxy shutdown — once `Close()` has been called or the proxy has exited (`runProxy` returned), every subsequent `inject(b)` returns `ErrInjectClosed` without blocking and without panicking. Concurrent invocation is safe — multiple goroutines can call the closure without external synchronization.

Trace: gist patch handles via a guard channel or atomic flag tied to proxy lifecycle.

### FR-6: engine.Config.OnInject passthrough

Add `OnInject func(inject func([]byte) error)` to `muxcore/engine/engine.go::Config`. The engine forwards this field to `ResilientClientConfig.OnInject` when constructing the resilient client. Zero value (`nil`) means no-op — engine consumers that don't set the field see byte-identical pre-v0.23 behavior. Mirrors the existing passthrough pattern for `StdinEOFPolicy` (v0.21.6) and `Persistent` (v0.22.0).

Trace: AGENTS.md v0.21.6 — *"engine.Config.StdinEOFPolicy passthrough"*; v0.22.0 — *"daemon.Config.Persistent bool (additive). The engine layer now passes cfg.Persistent through."*

### FR-7: Backward compatibility — zero source change for existing consumers

Existing consumers (aimux current `v0.22.0`, engram, mcp-mux daemon, mcp-launcher) require zero source change to upgrade to the version shipping this feature. `OnInject == nil` is the zero value and is the only valid state for non-adopting consumers.

Trace: API design is purely additive — no field renamed, no signature changed, no error return added to existing methods.

## Non-Functional Requirements

### NFR-1: Performance — non-blocking inject

`inject(b)` MUST NOT block. Channel send uses `select { case ch <- b: default: return ErrInjectFull }`. Worst-case latency = single channel-send attempt with no contention path = sub-microsecond on Go runtime. No goroutine creation per inject.

### NFR-2: Memory — no extra allocations beyond struct fields

Adding `OnInject` to two struct types adds 2× `unsafe.Sizeof(func)` ≈ 16 bytes per instance. No allocations during proxy hot path when `OnInject == nil`. No allocations beyond input frame bytes when `OnInject != nil`.

### NFR-3: Observability — log markers for inject lifecycle

Emit structured log markers at `Logger.Printf` level: `proxy.inject.armed` (post-handshake, single-fire), `proxy.inject.delivered` (after `msgFromCC <-` succeeds), `proxy.inject.dropped reason={full|closed}` (sentinel return path). Counter exposure via `HandleStatus` is a stretch goal — log markers cover the v1 acceptance.

### NFR-4: Concurrency — closure safe for concurrent use

Multiple goroutines MAY call `inject(b)` concurrently without external synchronization. The select-default channel send is atomic relative to other senders.

### NFR-5: Test coverage — minimum 2 regression tests

Two tests required pre-merge: `TestResilientClient_OnInject_DeliversFrames` (fires inject, asserts frame reaches IPC reader; subsequent CC request proxies normally) + `TestResilientClient_OnInject_BufferFull` (saturates `msgFromCC`, asserts `ErrInjectFull` returned without blocking). Both run on Linux+macOS+Windows CI matrix.

### NFR-6: Documentation — AGENTS.md entry

Add v0.23.0 release entry to AGENTS.md ## muxcore Library API section, mirroring v0.21.6/v0.22.0 entry style: breaking-changes table (none), migration notes for engram/aimux, regression test names, link to GitHub release.

## User Stories

### US1: aimux logger publishes via IPC channel without owning a separate transport (P1)

**As an** aimux daemon developer integrating centralized logging,
**I want** to wire shim's `IPCSink.SendFunc` once at engine construction,
**so that** log entries flow through the existing daemon IPC channel into `aimux.log` without me opening a second socket, managing reconnects, or violating the sole-writer invariant.

**Acceptance Criteria:**
- [ ] aimux v5.x `cmd/aimux/shim.go` sets `engine.Config.OnInject` in ~20 LOC (template ready in issue body)
- [ ] Shim subprocess log entries reach daemon `LogIngester` as `notifications/aimux/log_forward` JSON-RPC frames
- [ ] Reconnect after daemon hard-kill does NOT re-arm `OnInject` (FR-4) and does NOT lose buffered log entries (FR-2)
- [ ] No new socket file under `os.TempDir()`; only existing `*.sock` IPC path used

### US2: muxcore consumer pushes telemetry without violating proxy state machine (P2)

**As a** muxcore engine consumer that wants to push periodic telemetry/progress notifications,
**I want** a single fire-and-forget callback that lives inside the proxy lifecycle,
**so that** I do not need to reach into `runProxy` internals, fork the engine, or maintain my own shadow proxy.

**Acceptance Criteria:**
- [ ] Consumer can construct `engine.New(Config{... OnInject: myCallback})` with the callback receiving an `inject func([]byte) error` after handshake
- [ ] Calling `inject(frame)` returns `ErrInjectFull` under buffer pressure rather than blocking the consumer's goroutine
- [ ] Calling `inject(frame)` after engine shutdown returns `ErrInjectClosed` without panic

### US3: existing consumer upgrades muxcore version with zero source change (P1)

**As a** maintainer of mcp-mux/engram/mcp-launcher who is NOT yet adopting OnInject,
**I want** to bump `muxcore@v0.23.0` and rebuild without touching my engine.New invocation,
**so that** I can pick up unrelated bug fixes and improvements without scope creep.

**Acceptance Criteria:**
- [ ] `go get github.com/thebtf/mcp-mux/muxcore@v0.23.0` + `go build ./...` succeeds with zero source diff in the consumer repo
- [ ] Runtime behavior is byte-identical to v0.22.1 when `OnInject == nil`
- [ ] No new log lines, no new goroutines, no new allocations on the proxy hot path

## Edge Cases

- **EC-1: Inject before handshake.** Calling `inject(b)` before `OnInject` fires (i.e., before initial handshake completes) is impossible by API contract — the consumer never holds the closure until `OnInject` is invoked. No error path needed.
- **EC-2: Inject during RECONNECTING.** Frames go into `msgFromCC` buffer (1000-deep) and flush via `flushBuffer` after reconnect. Same path as buffered CC requests during reconnect — verified via existing `TestResilientClient_BufferDuringReconnect`.
- **EC-3: Buffer saturation under sustained injection rate > drain rate.** `select-default` returns `ErrInjectFull` immediately. Consumer decides drop-vs-retry. No internal queue grows unbounded.
- **EC-4: Proxy `Close()` called mid-inject.** Concurrent `inject(b)` may race with `Close()`. Closure must read close-flag atomically before channel send to return `ErrInjectClosed` cleanly.
- **EC-5: Malformed frame bytes (not valid JSON-RPC).** Out of scope for inject path — daemon's `LogIngester` (or whoever consumes the frame) is responsible for validation. muxcore proxies bytes opaquely.
- **EC-6: Consumer panics inside `OnInject` callback.** Callback runs on the proxy's goroutine. Panic propagates and tears down the proxy — same behavior as panic inside any other config callback (e.g., `RefreshToken`). Consumer is responsible for `recover()` if panic-tolerant.
- **EC-7: Concurrent injects from multiple goroutines saturating the buffer.** All concurrent senders see `ErrInjectFull` deterministically (no goroutine-starvation pathology) — Go channels are FIFO across senders.
- **EC-8: Daemon-side handler does not implement `notifications/aimux/log_forward` method.** Out of scope for muxcore. Frame delivery to daemon is best-effort; daemon-side handling is consumer responsibility.

## Out of Scope

- **Daemon-side `LogIngester` implementation** — lives downstream in aimux (`engram#178`), not muxcore.
- **Frame validation, schema enforcement, or rate limiting** inside muxcore — muxcore proxies opaque bytes; validation belongs to the consumer's daemon-side handler.
- **Counter exposure via `HandleStatus`** — log markers cover v1 acceptance; counters are a v0.23.x patch follow-up if operator demand emerges.
- **Multi-fire `OnInject`** (re-arming on reconnect) — single-fire is the documented contract per FR-4.
- **Priority lane / out-of-band frames** that bypass `msgFromCC` ordering — explicitly rejected per FR-2 to keep the proxy state machine unchanged.
- **Bidirectional injection** (daemon → consumer push) — not part of this feature; consumer subscribes via existing `notifications/*` consumption path.

## Dependencies

- **muxcore master at `eebee00`** (v0.22.1) — base for the change.
- **Reference patch:** gist `af3b003539f22e3d9c4c552a9faf92bb` — 3-file diff, applied via `git am`.
- **Downstream consumer:** aimux centralized logging spec (`engram#178`) — defines the frame format `notifications/aimux/log_forward` and daemon-side `LogIngester`. Not blocking on this side; aimux side is ready.
- **No new third-party Go dependencies.** All changes use stdlib (`errors`, `sync`, existing channels).

## Success Criteria

- [ ] `go test ./muxcore/owner/...` passes both new tests on Linux + macOS + Windows
- [ ] `go test ./muxcore/...` full suite green (zero regressions)
- [ ] `go vet ./muxcore/...` clean
- [ ] AGENTS.md v0.23.0 entry merged describing API addition + migration notes
- [ ] GitHub release `muxcore/v0.23.0` published with release notes pulled from AGENTS.md entry
- [ ] aimux PR (downstream) wires `engine.Config.OnInject` ≤ 20 LOC and centralized logging confirmed in `aimux.log`
- [ ] `go get github.com/thebtf/mcp-mux/muxcore@v0.23.0` + `go build ./...` in mcp-launcher (no OnInject usage) succeeds with zero source change

## Open Questions

(none — patch is feature-complete in gist `af3b003`; clarifications during plan/tasks if regression coverage gaps surface)

## Clarifications

### Session 2026-04-28 (auto)

| # | Category | Question | Resolution | Date |
|---|----------|----------|------------|------|
| C1 | Reliability | Behavior when consumer's `OnInject` callback panics or leaks goroutines? | Callback runs on proxy goroutine. Panic propagates and tears down proxy (same as `RefreshToken` panic). Consumer responsible for `recover()`. Codified in EC-6. NO internal recover wrapper — keeps callback signature simple and panic semantics consistent with other config callbacks. | 2026-04-28 |
| C2 | Security | Does `OnInject` widen attack surface — can unauthorized code spam daemon via injected frames? | NO. Trust boundary is `engine.Config` construction — anyone setting `OnInject` already has full process control (same trust level as setting `Handler` or `SessionHandler`). Daemon-side handler validates frame schema independently. No new authn/authz primitive needed. | 2026-04-28 |
| C3 | Constraints | Why reject priority lane / second socket / mutex (FR-2 enforces `msgFromCC`)? | Priority lane breaks FIFO ordering — daemon-side handlers receiving interleaved request/notification frames in non-deterministic order. Second socket doubles fd count + reconnect bookkeeping for marginal gain. Single mutex on existing channel is the simplest correct path. Decision codified in FR-2 + Out of Scope ("Priority lane / out-of-band frames"). | 2026-04-28 |

## Domain Modeling

DDD evaluated — not needed. Rationale: feature is a callback API addition with no entity relationships, no state machines beyond existing proxy CONNECTED/RECONNECTING states (already modeled), no business rules. FR-D3 detection patterns (entity relationships / state machines / business rules regex) do not match this spec's Context section.

## Strangler Fig

Not applicable — greenfield additive API. No legacy system being replaced or wrapped. Skipping per NFR-4.
