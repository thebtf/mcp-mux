# Implementation Plan: Shim Reconnect Token Refresh (CR-001)

**Spec:** `../../spec.md` (NVMD-140)
**Change Record:** `./change.md`
**Created:** 2026-04-20
**Status:** Draft

> **Provenance:** Planned by Claude (Opus 4.7) on 2026-04-20.
> Evidence from: spec.md, muxcore/session/session_manager.go, muxcore/daemon/daemon.go,
> muxcore/control/{protocol,server}.go, muxcore/owner/{owner,resilient_client}.go,
> forensic report `.agent/debug/daemon-idle-hang/investigation-pr-reconnect.md`.
> Confidence: VERIFIED (spec + code reviewed file:line).

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go (stdlib only) | NFR-1 — no third-party deps |
| Control transport | Existing NDJSON over IPC (`control.Server`) | Extension of shipped control plane |
| Token storage | Extend `session.Manager` with `bound` history map | Minimal diff; keeps `pending` behavior intact |
| Tests | `testing` stdlib (existing style) | NFR-3 / NFR-4 |

## Architecture

### Control plane extension

Add one command `refresh-token` to `control.Request/Response` and matching
`DaemonHandler.HandleRefreshSessionToken`:

- **Input:** previous token string (sent in `Request.Token` — new field)
- **Output:** new token string (returned in `Response.Token`), `ErrOwnerGone`
  surfaces as `Response{OK:false, Message: "owner gone"}`; `ErrUnknownToken`
  surfaces as `Response{OK:false, Message: "unknown token"}`.

### Session manager extension

`session.Manager` grows:

- `bound map[string]*boundHistory` — records tokens successfully consumed by `Bind`.
  `boundHistory{ OwnerKey string; Cwd string; Env map[string]string; BoundAt time.Time; LastUsed time.Time }`.
- `LookupHistory(prev string) (OwnerKey string, cwd string, env map[string]string, ok bool)` — side-effect-free.
- `RegisterReconnect(prev string, ownerStillAlive func(key string) bool) (newToken string, err error)` — atomic:
  - Reads `bound[prev]` under `sm.mu`.
  - Calls the liveness predicate (without holding `sm.mu`, then re-locks) to
    avoid a daemon↔session deadlock.
  - Mints a fresh 128-bit hex token, inserts into `pending[newToken]` with the
    same cwd+env. Refreshes `bound[prev].LastUsed`. Returns `newToken`.
  - Concurrent refresh on same prev → two independent new tokens (NFR-3 (e)).

Bind bookkeeping: on successful `Bind`, record entry in `bound` (owner key TBD — see
"Owner identity" below). TTL = 30 min since last use; swept alongside
`SweepExpiredPending` or via new `SweepExpiredBound`.

### Owner identity

An owner key is needed to (a) tie a bound token to a specific owner and (b) check
liveness. `OwnerEntry.ServerID` (8-char + hex) is the natural key. Bind is invoked
from `Owner.acceptLoop`, which knows its own `ServerID`. Wire path:

1. `owner.Owner` gains a `ServerID()` accessor (likely exists; check).
2. `session.Manager.Bind` signature extended to accept `ownerKey string`.
3. `Daemon` exposes `OwnerIsAccepting(serverID string) bool` used by refresh path.

### Shim fallback path

`muxcore/owner/resilient_client.go` currently calls `cfg.Reconnect()` which
produces a fresh spawn unconditionally. Extend contract:

- New optional `cfg.RefreshToken ReconnectFunc` (returns ipcPath, token, err).
  Semantics: shim prefers refresh when current owner is presumed alive; on
  `ErrOwnerGone` or N consecutive failures, falls through to `cfg.Reconnect`
  (spawn path).
- Rejection counter: `resilientClient` tracks `refreshFailures int`; resets on
  successful handshake. `N=3` (constant; configurable via
  `ResilientClientConfig.MaxRefreshAttempts`, default 3).
- Structured markers emitted via `rc.log`:
  - `shim.reconnect.refresh_ok owner=<sid-prefix>`
  - `shim.reconnect.refresh_fail reason=<unknown_token|owner_gone|handshake|dial>`
  - `shim.reconnect.fallback_spawn reason=<N_refresh_fail|owner_gone>`

Shim side is stateless across CC-process restarts. Windows `wsarecv: forcibly
closed` path already triggers `RunResilientClient` state machine — refresh hook
is the first attempt inside its existing retry loop.

### Metrics (FR-5)

`daemon.Daemon.HandleStatus` currently returns a `map[string]interface{}`.
Append three uint64 counters wrapped in `atomic.Uint64`:

- `shim_reconnect_refreshed`
- `shim_reconnect_fallback_spawned`
- `shim_reconnect_gave_up`

Incremented at the respective control-plane / daemon reconnection paths. Exposed
through the existing `mux_status` key path.

## Reversibility

| Decision | Tag | Notes |
|----------|-----|-------|
| New control command `refresh-token` | REVERSIBLE | Additive — old shims skip it |
| `session.Manager.bound` history map | REVERSIBLE | In-memory, TTL-swept |
| `Bind` signature adds `ownerKey` | PARTIALLY REVERSIBLE | Internal API; one consumer (owner.acceptLoop). v0.21.x consumers re-compile. |
| `ResilientClientConfig.RefreshToken` | REVERSIBLE | Optional field; unset ⇒ pre-F2 behaviour |

## File Structure

```text
muxcore/
  control/
    protocol.go          MOD  + Request.Token input field doc; Response reused
    server.go            MOD  + dispatch("refresh-token") → DaemonHandler.HandleRefreshSessionToken
  session/
    session_manager.go   MOD  + bound history, LookupHistory, RegisterReconnect, extended Bind
    session_manager_test.go MOD  + coverage tests (NFR-3)
  daemon/
    daemon.go            MOD  + HandleRefreshSessionToken, ErrOwnerGone, ErrUnknownToken, counters
    daemon_test.go       MOD  + HandleRefreshSessionToken_* tests
    reconnect_integration_test.go NEW  + NFR-4 integration reproducer
  owner/
    owner.go             MOD  + Bind call signature (passes ServerID as ownerKey)
    resilient_client.go  MOD  + RefreshToken field, refresh-first retry loop, structured markers
    resilient_client_reconnect_test.go NEW  + FR-2/FR-3 unit tests
cmd/
  mcp-mux/
    shim.go              MOD  + wire RefreshToken callback to daemon control client
```

(Exact file names verified against current tree; any New/Mod mismatch flagged
during T001 scaffolding.)

## Phases

### Phase 0: Planning & Task Assignment

Drafting this plan + tasks.md.

### Phase 1: Setup & Scaffolding

Generate API skeletons in `session`, `control`, `daemon` packages. No behaviour
changes yet; just unexported additions + failing tests.

### Phase 2: Foundational — Session Manager Bookkeeping

Implement `bound` map, `LookupHistory`, `RegisterReconnect`, extend `Bind` to
record the owner key. Unit-test in `session_manager_test.go`.

### Phase 3: Control Plane — RefreshSessionToken Command

Add `refresh-token` dispatch in `control/server.go`, extend
`DaemonHandler` interface, implement `Daemon.HandleRefreshSessionToken`,
add `ErrOwnerGone`/`ErrUnknownToken` sentinels, add metric counters.

### Phase 4: Owner Integration

Pipe `ServerID` through to `Bind`. Add `Daemon.OwnerIsAccepting`. Wire
liveness check to `RegisterReconnect`.

### Phase 5: Shim — Refresh-First Reconnect

Extend `ResilientClientConfig`, implement refresh-first retry loop with
rejection counter, structured logs, and fallback to existing spawn path.

### Phase 6: Metric Counters + Structured Logs Wiring

Export `HandleStatus` keys, ensure log markers match FR-6.

### Phase 7: Integration Test + Back-Compat Probe

Add `reconnect_integration_test.go` (NFR-4 reproducer, simulates the
2026-04-20 pr-reconnect trace). Add legacy-shim probe test (SC-3).

### Phase 8: Polish

Update `AGENTS.md` release notes stub for v0.21.1. Ensure
`resilient_client_reconnect_test.go` covers goroutine-leak invariants
(no leaked handshake goroutines on refresh timeout).

## Library Decisions

| Component | Library | Version | Rationale |
|-----------|---------|---------|-----------|
| Control NDJSON | stdlib `encoding/json` | — | Existing |
| Tokens | stdlib `crypto/rand` + `encoding/hex` | — | Same as daemon spawn path |
| Locks | stdlib `sync.RWMutex`, `sync.Mutex` | — | Match existing code |
| Tests | stdlib `testing` | — | Match existing code |

No new third-party deps (NFR-1).

## Unknowns and Risks

| Unknown | Impact | Resolution |
|---------|--------|------------|
| Does `owner.Owner` expose `ServerID()`? | LOW | Grep during T005; add accessor if missing. |
| Interaction with in-flight `SweepExpiredPending` when refresh races with TTL expiry | MEDIUM | Refresh-vs-sweep: if `bound` entry TTL-expired, refresh returns `ErrUnknownToken`, shim falls back to spawn. Covered by NFR-3 (b). |
| Per-owner lock ordering (`d.mu` → `sm.mu` vs reverse) | MEDIUM | Use `d.mu.RLock` only for `OwnerIsAccepting`; release before acquiring `sm.mu` for `RegisterReconnect`. Order documented in T018. |

## Constitution Compliance

- **Multi-user ready:** The token refresh preserves the FR-28 pre-registration
  contract — rejection still requires registry presence; refresh only
  re-issues tokens for already-known owners. No cross-tenant leakage.
- **Stdlib-only:** NFR-1.
- **Production-grade:** Unit + integration tests; metrics; structured logs.
- **Reversible per-owner:** If refresh causes pain, operators can set
  `MaxRefreshAttempts=0` to force immediate spawn fallback.
