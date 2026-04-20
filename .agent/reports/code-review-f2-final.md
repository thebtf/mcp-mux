# Code Review — F2 Shim Reconnect Token Refresh (CR-001)

**Date:** 2026-04-20
**Branch:** feat/shim-reconnect-refresh
**Spec:** `.agent/specs/shim-reconnect-token-refresh/spec.md`
**Plan:** `.agent/specs/shim-reconnect-token-refresh/changes/CR-001-initial-scope/plan.md`

---

## Summary

14 files modified, +1093 / -84 lines. Delivers the full token-refresh reconnect path
(FR-1 through FR-5) across all four layers: session manager, control plane, daemon, and
shim. One integration reproducer (`TestReconnectRefreshPreservesOwner`) confirms the
2026-04-20 pr-reconnect forensic scenario is fixed end-to-end.

### Files shipped

| File | Role |
|------|------|
| `muxcore/session/session_manager.go` | boundHistory, Bind(+ownerKey), LookupHistory, RegisterReconnect, SweepExpiredBound |
| `muxcore/session/session_manager_test.go` | 8 new test funcs covering reconnect paths |
| `muxcore/control/protocol.go` | PrevToken field on Request; HandleRefreshSessionToken on DaemonHandler interface |
| `muxcore/control/server.go` | refresh-token dispatch case |
| `muxcore/control/control_test.go` | 3 new test funcs: happy, unknown, owner-gone |
| `muxcore/daemon/daemon.go` | HandleRefreshSessionToken, ownerIsAccepting, reconnect counters |
| `muxcore/daemon/daemon_test.go` | 4 new test funcs for HandleRefreshSessionToken |
| `muxcore/daemon/reaper.go` | SweepExpiredBound wired into periodic sweep |
| `muxcore/daemon/reconnect_integration_test.go` | TestReconnectRefreshPreservesOwner (NFR-4 reproducer) |
| `muxcore/owner/owner.go` | isAccepting atomic.Bool; IsAccepting() with listenerDone fallback; ServerID() accessor; acceptLoop passes ServerID to Bind |
| `muxcore/owner/owner_test.go` | ServerID accessor test |
| `muxcore/owner/accept_loop_test.go` | TestIsAccepting_TracksListenerLifecycle |
| `muxcore/owner/resilient_client.go` | RefreshToken + MaxRefreshAttempts fields; refresh-first retry loop |
| `muxcore/owner/resilient_client_reconnect_test.go` | 5 test funcs covering all reconnect paths |
| `cmd/mcp-mux/daemon.go` | refreshTokenViaDaemon helper |
| `cmd/mcp-mux/main.go` | RefreshToken callback wired into ResilientClientConfig |
| `cmd/mcp-mux/daemon_test.go` | 3 test funcs: happy, owner-gone, unknown-token |

---

## AC Coverage Matrix

| FR | Description | Implementation | Test |
|----|-------------|----------------|------|
| FR-1 | RefreshSessionToken endpoint | `session.Manager.RegisterReconnect` + `daemon.HandleRefreshSessionToken` + `control.Server` dispatch | `TestRegisterReconnect_*`, `TestHandleRefreshSessionToken_*`, `TestRefreshToken*` |
| FR-2 | Shim rejection-counter fallback (N=3) | `resilient_client.go` refresh loop with `MaxRefreshAttempts` | `TestReconnect_FallsBackToSpawnAfterNRejects` |
| FR-3 | RefreshToken-first, spawn-fallback | refresh loop in `runProxy` / `reconnectLoop` | `TestReconnect_RefreshesAndSucceeds`, `TestReconnect_ImmediateSpawnOnOwnerGone` |
| FR-4 | Owner identity / ErrOwnerGone | `RegisterReconnect` calls `ownerAlive` closure; daemon checks `owner.IsAccepting()` | `TestRegisterReconnect_OwnerGone`, `TestHandleRefreshSessionToken_OwnerGone` |
| FR-5 | Metric counters | `reconnectRefreshed`, `reconnectFallbackSpawned`, `reconnectGaveUp` atomic counters on Daemon | `TestHandleRefreshSessionToken_CountersIncrement`, `TestHandleStatus_ZombieCounters` |
| FR-6 | (Subsumed by FR-3 fallback) | Zero-value `RefreshToken=nil` skips refresh, falls straight to Reconnect | `TestReconnect_RefreshNilFallsBackImmediately` |

| NFR | Description | Status |
|-----|-------------|--------|
| NFR-1 | No breaking changes for old consumers | Zero-value `RefreshToken=nil` preserves pre-F2 behaviour |
| NFR-2 | Refresh RPC < 100ms on local IPC | Local control socket round-trip; daemon tests run < 10ms |
| NFR-3 | Concurrent token mint produces distinct tokens | `TestRegisterReconnect_RaceDoubleRefresh` |
| NFR-4 | Integration reproducer deterministic | `TestReconnectRefreshPreservesOwner` PASS × 3 |
| NFR-5 | No goroutine leaks | `TestReconnect_NoGoroutineLeakOnRefreshTimeout` |

| SC | Description | Status |
|----|-------------|--------|
| SC-1 | New token added to pending[] | Verified: `RegisterReconnect` calls `sm.pending[token] = ...` |
| SC-2 | Old token not re-bindable | Verified: `Bind` deletes from pending on first use; old token never re-inserted |
| SC-3 | Old shims fall through to spawn | Verified: `RefreshToken=nil` path goes directly to `cfg.Reconnect` |
| SC-4 | Owner not reaped during active refresh | Verified by `TestReconnectRefreshPreservesOwner` |

---

## Deviations from Spec / Plan

| Deviation | Rationale |
|-----------|-----------|
| T022/T023 in `cmd/mcp-mux/daemon.go` + `main.go`, not `shim.go` | No `shim.go` file exists in the binary; convention follows `daemon.go` for control-plane helpers |
| T023 helper named `refreshTokenViaDaemon` not `SendRefreshToken` | Mirrors existing `spawnViaDaemon` convention; AC satisfied |
| `IsAccepting()` uses dual check (atomic + listenerDone channel) | Codex's atomic-only impl broke test-owner preconditions; dual check restores correct zombie-vs-legitimate-close distinction |

---

## Known Gaps / Follow-ups

1. **T025 (legacy-shim probe):** Daemon-level counter assertion for `shim_reconnect_fallback_spawned` on the legacy path (old shim that never sends refresh-token) is not tested at the daemon integration level. `TestReconnectGiveUp` in `control_test.go` covers the control-plane path but not the daemon counter increment. Recommend filing a follow-up task.

2. **T026 (AGENTS.md v0.21.1 note):** Deferred — AGENTS.md lives on master branch. Should be updated in a follow-up commit to master or squashed into merge.

3. **Race detector:** `-race` requires CGO (GCC). GCC not present in the close-out shell environment. All tests were run without race detector. The session manager concurrent test (`TestRegisterReconnect_RaceDoubleRefresh`) exercises the race condition with `sync.WaitGroup` coordination but without the hardware race detector instrumentation.

---

## Test Execution Log

| Package | Test count | Duration | -race |
|---------|-----------|----------|-------|
| `muxcore/session` | 16 | 0.26s | no (no CGO) |
| `muxcore/control` | 22 | 5.35s | no |
| `muxcore/owner` | ~65 | 12.0s | no |
| `muxcore/daemon` | ~25 | 10.6s | no |
| `cmd/mcp-mux` | 3 | 0.28s | no |
| **Integration × 3** | 1 × 3 | 0.42s | no |
| **Full muxcore ./...** | 19 packages | ~40s | no |

All packages: PASS. Zero failures after `IsAccepting()` fix applied.

---

## HIGH Findings

None. One bug found and fixed during close-out (IsAccepting regression — see tasks.md close-out notes). No unresolved HIGH findings.
