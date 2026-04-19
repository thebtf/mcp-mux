---
feature_id: NVMD-140
title: Shim reconnect path — token refresh and fallback to spawn handshake
status: Draft
open_crs: [CR-001]
engram_issue: 137
---

# Spec: Shim reconnect token refresh (NVMD-140)

## Summary

FR-28 (multi-user hardening, shipped in muxcore/v0.20.4) requires the shim to
present a **pre-registered session token** on the owner IPC socket accept.
Pre-registration happens through the daemon's control plane at
`spawn`-time. The `acceptLoop` in `muxcore/owner/owner.go:1636-1642` rejects
any connection whose token is empty OR not in `sm.pending[]`.

**Reconnect is not covered by this design.** When a shim loses its IPC
connection to a still-alive session-aware owner (e.g., Windows
`wsarecv: forcibly closed by remote host` during CC session bounce) and
attempts to redial the same socket, it has no valid pre-registered token —
the one used at spawn was already consumed by the first successful Bind.
The accept rejects with `pid=-1 (invalid/missing token)`, CC retries, same
result, until the owner is reaped at the 10-minute grace boundary.

Forensic evidence: `.agent/debug/daemon-idle-hang/investigation-pr-reconnect.md`.

## Why this matters

Breaks the "reconnect works while the owner is still alive" contract in
practice. Cached init/tools templates + live upstream are thrown away after
10 minutes even though the underlying upstream is perfectly healthy and a
shim was actively trying to re-attach. User-observable symptom:
`/mcp reconnect` never recovers a failed server — user must wait for the
owner to reap and a fresh spawn to kick in, or manually restart the whole
MCP stack.

Impact scales with session-aware owners (pr-review-mcp is the known one;
more may appear as more MCP servers advertise `x-mux.session-aware`
capability).

## User Stories

- **US1:** As a CC session whose shim transport dropped transiently
  (network hiccup, Windows pipe close, CC UI reload), I want `/mcp
  reconnect` to re-attach my shim to the still-alive session-aware owner
  within 1-2 seconds — not wait 10+ minutes for reap.
- **US2:** As a daemon operator, I want failed reconnect attempts to
  trigger a clean fresh spawn handshake rather than retry the same
  socket+token forever, so that after at most `N` rejections the session
  recovers via a new owner + cached templates.
- **US3:** As a muxcore library consumer (aimux, engram), reconnect
  behavior should be identical across transports (Unix socket, Windows
  named pipe). No platform-specific reconnect semantics.

## Functional Requirements

- **FR-1 (RefreshSessionToken endpoint):** New daemon control-plane method
  `RefreshSessionToken(prev_token string) → new_token string`. Valid if
  `prev_token` was observed in `sm.pending[]` or `sm.bound[]` at any point
  (a weak "we know you" check, not cryptographic — the shim still needs
  the prev_token which is secret). Returns a freshly minted 128-bit token
  added to `sm.pending[]`. Old token remains consumed (no re-bind).
- **FR-2 (Shim rejection-counter fallback):**
  `muxcore/owner/resilient_client.go` tracks consecutive `invalid/missing
  token` rejections per owner. When count reaches `N=3` (configurable),
  the shim purges its cached owner address + token and issues a fresh
  `spawn` request via control plane — same code path as first-time startup.
- **FR-3 (RefreshToken-first, spawn-fallback):** On first reject, shim
  tries `RefreshSessionToken` to get a new token and redials. Only after
  N=3 refreshes fail does it fall back to full spawn. This preserves
  session-aware state when the owner is healthy but token lifecycle drifted.
- **FR-4 (Owner identity check):** `RefreshSessionToken` must validate
  that the previous token was bound to an owner that is **still alive**.
  If the owner shut down between prev-consume and refresh-request, the
  call returns `ErrOwnerGone` — shim then goes straight to full spawn
  handshake.
- **FR-5 (Metric counters):** `HandleStatus` grows new counters
  `shim_reconnect_refreshed`, `shim_reconnect_fallback_spawned`,
  `shim_reconnect_gave_up` for operator visibility.
- **FR-6 (Structured logs):** `shim.reconnect.refresh_ok`,
  `shim.reconnect.refresh_fail reason=<>`,
  `shim.reconnect.fallback_spawn owner=<sid>` log markers. Similar style
  to existing `handoff.*` markers from v0.21.0.

## Non-Functional Requirements

- **NFR-1:** No new third-party dependencies. Stdlib + existing
  muxcore packages only.
- **NFR-2:** `RefreshSessionToken` completes in under 100 ms on local
  IPC. Backed by existing `SessionManager.mu` critical section; no new
  global locks.
- **NFR-3:** Unit tests cover: (a) refresh happy path, (b) refresh with
  unknown prev_token → reject, (c) refresh with owner-gone → `ErrOwnerGone`,
  (d) fallback after N=3 refresh failures, (e) concurrent refreshes on
  the same owner do not double-spawn.
- **NFR-4:** Integration test reproducing the 2026-04-20 pr-reconnect
  scenario: spawn owner, consume token, disconnect session, attempt
  reconnect — assert refresh succeeds, shim re-attaches, owner NOT reaped.
- **NFR-5:** Back-compat: old shims (v0.20.4 and earlier) that don't know
  about RefreshSessionToken still work — accept still rejects them after
  first consume, daemon doesn't break. New feature is additive on the
  shim side.

## Success Criteria

- **SC-1:** Forensic reproducer from 2026-04-20 (shim loses session 5
  via `wsarecv forcibly closed`, retries 6× with `pid=-1` rejections)
  no longer loses the owner — after 1-3 refresh attempts the shim
  re-attaches successfully.
- **SC-2:** `mux_status` shows `shim_reconnect_refreshed > 0` after
  user runs the reproducer and `shim_reconnect_gave_up == 0`.
- **SC-3:** Legacy shim binary (v0.20.4) connecting to a new daemon with
  this feature still rejected cleanly — no crashes, no leaks. Log marker
  `shim.reconnect.legacy_client` emitted (optional).
- **SC-4:** No regression in existing spawn handshake path — all
  `muxcore/daemon/*` and `muxcore/owner/*` tests green.

## Architecture

### Control plane API

Extend the existing daemon control message set (similar to
`daemon.HandleGracefulRestart`):

```go
type DaemonHandler interface {
    // ... existing methods ...
    HandleRefreshSessionToken(prevToken string) (newToken string, err error)
}
```

Daemon-side logic (rough):

```go
func (d *Daemon) HandleRefreshSessionToken(prevToken string) (string, error) {
    d.mu.RLock()
    owner, prevKnown := d.sessionMgr.LookupHistory(prevToken) // see NEW method
    d.mu.RUnlock()
    if !prevKnown {
        return "", ErrUnknownToken
    }
    if owner == nil || !owner.IsAccepting() {
        return "", ErrOwnerGone
    }
    newToken, err := d.sessionMgr.RegisterReconnect(owner, prevToken)
    if err != nil {
        return "", err
    }
    d.logger.Printf("shim.reconnect.refresh_ok owner=%s", owner.ServerID[:8])
    return newToken, nil
}
```

`SessionManager` grows a `bound` snapshot field (prev tokens that were
successfully consumed by `Bind`) — bounded in size, TTL'd to 30 min since
last use. `RegisterReconnect` looks up the owner via the prev token,
creates a new pending entry bound to the same owner+cwd+env, and returns
the new token.

### Shim side

`resilient_client.go` reconnect flow:

```
func (rc *ResilientClient) reconnect() error {
    for attempt := 0; attempt < maxRefreshAttempts; attempt++ {
        newToken, err := rc.daemonCtl.RefreshSessionToken(rc.lastToken)
        if err == ErrOwnerGone {
            return rc.fallbackSpawn()
        }
        if err != nil {
            continue
        }
        rc.lastToken = newToken
        if err := rc.dialAndHandshake(); err == nil {
            return nil
        }
        // handshake still failed — log, loop
    }
    return rc.fallbackSpawn()
}
```

`fallbackSpawn` hits the existing `HandleSpawn` path, getting a fresh
owner + fresh token. Cached templates at owner level are preserved if
the command fingerprint matches (existing template-cache instant-init path).

## Out of Scope

- Reconnect for SHARED owners (non-session-aware). They already work via
  dedup — shim can redial any living owner for the same command. Only
  session-aware owners have strict token binding that needs this fix.
- Reconnect across daemon restarts (that's the v0.21.0 FD handoff arc,
  already shipped). This spec is for same-daemon transient reconnects.
- Client-side auth/authorization policy (who may refresh a token). FR-28
  threat model has the daemon in the client's TCB; the refresh check
  (know-the-prev-token) is sufficient for the current threat model.

## Test Plan (sketch)

1. **Unit** (`muxcore/session/session_manager_test.go`):
   - `TestLookupHistory_AfterBind` — token previously consumed by Bind is
     findable via LookupHistory until TTL expiry.
   - `TestRegisterReconnect_NewTokenFresh` — returned token is distinct
     from prev, is in pending[], owner bind succeeds.
   - `TestRegisterReconnect_RaceDoubleRefresh` — two concurrent
     RegisterReconnect calls on same prev token produce two distinct
     new tokens, both valid independently.

2. **Unit** (`muxcore/daemon/control_refresh_test.go`):
   - `TestHandleRefreshSessionToken_HappyPath`
   - `TestHandleRefreshSessionToken_UnknownToken` → `ErrUnknownToken`
   - `TestHandleRefreshSessionToken_OwnerGone` → `ErrOwnerGone`

3. **Unit** (`muxcore/owner/resilient_client_reconnect_test.go`):
   - `TestReconnect_RefreshesAndSucceeds` — simulates reject → refresh → ok.
   - `TestReconnect_FallsBackToSpawnAfterNRejects` — 3 refreshes fail, fall back.
   - `TestReconnect_ImmediateSpawnOnOwnerGone` — daemon replies ErrOwnerGone,
     shim skips to spawn.

4. **Integration** (`muxcore/daemon/reconnect_integration_test.go`):
   - Spawn owner → consume token → force session close → reconnect via
     `RefreshSessionToken` → verify re-attach succeeds, owner not reaped.

## References

- Forensic investigation: `.agent/debug/daemon-idle-hang/investigation-pr-reconnect.md`
- FR-28 original design: multi-user-hardening spec (v0.20.4).
- Engram issue: `engram://mcp-mux#137` (F2 portion).
- Cross-ref: `engram://aimux#136` (orthogonal bug, separate repo).
