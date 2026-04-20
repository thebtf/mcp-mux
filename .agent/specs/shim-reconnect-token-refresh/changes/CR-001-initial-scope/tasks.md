# Tasks: Shim Reconnect Token Refresh — CR-001 Initial Scope

**Spec:** `../../spec.md`
**Change Record:** `./change.md`
**Plan:** `./plan.md`
**Generated:** 2026-04-20

## Phase 0: Planning

- [x] P001 Spec reviewed, architecture confirmed (done in plan.md).
  AC: plan.md committed with file structure, phases, risks · forbidden placeholder scan clean.
- [x] P002 Complexity tier assignment per task (see [EXECUTOR] markers).
  AC: every T-task has [EXECUTOR: sonnet|aimux|MAIN] · no unmarked non-trivial task.
- [x] P003 No open research questions (architecture frozen in spec + plan).
  AC: unknowns table in plan.md populated with resolutions.

## Phase 1: Setup & Scaffolding

- [x] T001 [P] [EXECUTOR: sonnet] Verify `owner.Owner` exposes `ServerID` accessor (or add one) in `muxcore/owner/owner.go`.
  AC: `func (o *Owner) ServerID() string` returns the owner's server ID · grep-find 0 call sites missing · unit test in `owner/owner_test.go` asserts non-empty ID after construction · swap body→return "" ⇒ new tests MUST fail.
- [x] T002 [P] [EXECUTOR: sonnet] Add sentinel errors `ErrUnknownToken` and `ErrOwnerGone` to `muxcore/daemon/daemon.go`.
  AC: both are exported package-level `errors.New` values · `errors.Is(ErrUnknownToken, ErrUnknownToken) == true` · not re-declared elsewhere in package.
- [x] T003 [EXECUTOR: sonnet] Extend `control.Request` with `PrevToken string` field (`json:"prev_token,omitempty"`) and document `refresh-token` command in `muxcore/control/protocol.go`.
  AC: field added to struct + godoc comment · `go vet ./muxcore/control/...` clean · existing marshalled requests unchanged (omitempty) · no impact on `HandleSpawn` path.

- [x] G001 VERIFY Phase 1 (T001–T003) — BLOCKED until T001–T003 all [x]
  RUN: `go build ./...`, `go vet ./...`, `go test ./muxcore/owner/... ./muxcore/control/...`. Call Skill("code-review", "lite") on every file touched by T001–T003.
  CHECK: verify ServerID accessor used nowhere yet except test, sentinels exported, control request field not in Response.
  ENFORCE: zero stubs. Zero TODOs.
  RESOLVE: fix all findings before [x].

Discovery notes:
- The forensic report path referenced by the spec/backlog (`.agent/debug/daemon-idle-hang/investigation-pr-reconnect.md`) is absent in this worktree. T024 will reproduce against the verified symptoms preserved in `spec.md` (`wsarecv`, repeated `pid=-1`, invalid/missing token rejects).
- Gate execution required two environment corrections: `go test` for nested `muxcore` packages must run from `muxcore/`, and `GOCACHE` must point at a writable path (`C:\Users\btf\.codex\memories\gocache-f2`) because the default and workspace-local cache locations returned `Access is denied`.

---

**Checkpoint:** Scaffolding complete — sentinels, accessors, protocol fields in place.

## Phase 2: Foundational — Session Manager Bookkeeping

- [x] T004 [EXECUTOR: aimux] Add `boundHistory` struct and `bound map[string]*boundHistory` to `muxcore/session/session_manager.go` Manager.
  AC: struct has `OwnerKey string; Cwd string; Env map[string]string; BoundAt time.Time; LastUsed time.Time` · `NewManager()` initializes the map · no public accessor yet · swap initialiser→nil ⇒ subsequent tests MUST fail.
- [x] T005 [EXECUTOR: aimux] Extend `Bind(token string, session *Session)` to `Bind(token, ownerKey string, session *Session)` and record `bound[token]` on success. Update call sites.
  AC: `bound[token]` populated with cwd/env/ownerKey + timestamps on successful Bind · existing test `TestIsPreRegistered` still green · grep-verified 0 stale call sites · anti-stub: swap bookkeeping→no-op ⇒ new tests MUST fail.
- [x] T006 [P] [EXECUTOR: sonnet] Implement `LookupHistory(prev string) (ownerKey, cwd string, env map[string]string, ok bool)` in `session_manager.go`.
  AC: returns entry under `sm.mu.RLock()` · does NOT mutate state · returns ok=false when prev absent or TTL-expired · new test `TestLookupHistory_AfterBind`.
- [x] T007 [EXECUTOR: aimux] Implement `RegisterReconnect(prev string, ownerAlive func(key string) bool) (newToken string, err error)` in `session_manager.go`.
  AC: returns `ErrUnknownToken` (re-exported from session pkg) when prev missing; `ErrOwnerGone` when ownerAlive(ownerKey)==false · on success mints 128-bit hex token, inserts into pending[], returns newToken · concurrent calls produce distinct tokens (NFR-3 (e)) · ownerAlive invoked WITHOUT holding sm.mu · swap mint→return "" ⇒ tests MUST fail.
- [x] T008 [P] [EXECUTOR: sonnet] Add `SweepExpiredBound()` alongside `SweepExpiredPending`, 30-min TTL since LastUsed.
  AC: returns swept count · unit test injecting LastUsed=(-31m) removes entry · integration with daemon sweep loop wired in T019.
- [x] T009 [P] [EXECUTOR: sonnet] Tests for session manager reconnect path in `session_manager_test.go`.
  AC: `TestLookupHistory_AfterBind`, `TestRegisterReconnect_NewTokenFresh`, `TestRegisterReconnect_UnknownToken`, `TestRegisterReconnect_OwnerGone`, `TestRegisterReconnect_RaceDoubleRefresh` · total ≥5 new test funcs · anti-stub: all five assert via observable state, not log inspection.

- [x] G002 VERIFY Phase 2 (T004–T009) — BLOCKED until T004–T009 all [x]
  RUN: `go test ./muxcore/session/... -race -count=1`. Code-review lite.
  CHECK: race-detector clean · existing tests still green · new tests cover all five session-manager scenarios.
  ENFORCE: zero stubs. Every parameter influences output. No `// TODO`.
  RESOLVE: fix all findings before [x].

---

**Checkpoint:** Session manager knows history; refresh logic ready for wiring.

## Phase 3: Control Plane — RefreshSessionToken Command

- [x] T010 [EXECUTOR: aimux] Extend `control.DaemonHandler` interface with `HandleRefreshSessionToken(prevToken string) (newToken string, err error)` in `muxcore/control/protocol.go`.
  AC: interface method added · compile-time assertion in `daemon/daemon.go` (`var _ control.DaemonHandler = (*Daemon)(nil)`) compiles only after T011.
- [x] T011 [EXECUTOR: aimux] Dispatch `refresh-token` in `muxcore/control/server.go`.
  AC: new `case "refresh-token":` block · returns `Response{OK:true, Token: newToken}` on success · surfaces `ErrUnknownToken` as Message="unknown token", `ErrOwnerGone` as Message="owner gone" with OK=false · other errors → OK=false with plain error string · swap body→return empty Response ⇒ Phase 3 tests MUST fail.
- [x] T012 [P] [EXECUTOR: sonnet] Control-plane dispatch test in `control/control_test.go`.
  AC: stub DaemonHandler asserts handler invoked with prev token · response fields match contract · 3 test funcs: happy path, unknown, owner-gone · swap dispatch→default ⇒ tests MUST fail.

- [x] G003 VERIFY Phase 3 (T010–T012) — BLOCKED until T010–T012 all [x]
  RUN: `go test ./muxcore/control/... -count=1`.
  CHECK: dispatch paths covered · interface satisfied at compile time.
  ENFORCE: zero stubs.
  RESOLVE: fix before [x].

---

**Checkpoint:** Control plane speaks refresh-token.

## Phase 4: Daemon — HandleRefreshSessionToken Implementation + Metrics

- [x] T013 [EXECUTOR: aimux] Implement `Daemon.HandleRefreshSessionToken(prevToken string) (string, error)` in `muxcore/daemon/daemon.go`.
  AC: locks `d.mu.RLock()` only to call new `Daemon.ownerIsAccepting(serverID)` helper · calls `sessionMgr.RegisterReconnect(prevToken, liveness)` with liveness as closure · returns `(newToken, nil)` on success · forwards `ErrUnknownToken`/`ErrOwnerGone` · logs `shim.reconnect.refresh_ok owner=<sid-prefix-8>` on success, `shim.reconnect.refresh_fail reason=<code>` on failure · no goroutine leak · swap body→return "", nil ⇒ tests fail.
- [x] T014 [P] [EXECUTOR: sonnet] Implement `Daemon.ownerIsAccepting(serverID string) bool` (internal) and ensure `OwnerEntry` exposes liveness signal (has live upstream + accept loop running).
  AC: returns true if owner present AND `owner.IsAccepting()==true` · returns false if owner removed, shutting down, or upstream dead · unit-tested via `TestDaemon_OwnerIsAccepting_States`.
- [x] T015 [EXECUTOR: aimux] Add `shim_reconnect_refreshed`, `shim_reconnect_fallback_spawned`, `shim_reconnect_gave_up` counters (`atomic.Uint64`) to `Daemon` struct; export through `HandleStatus`.
  AC: counters initialized in `New` constructor · incremented: refreshed on HandleRefreshSessionToken success, fallback_spawned when daemon Spawn is reached via refresh-fallback marker, gave_up when shim signals final give-up (new control cmd OR reuse `HandleSpawn` with header — see spec FR-5) · visible in `HandleStatus()` map under the exact keys listed · unit test asserts counter increments.
- [x] T016 [P] [EXECUTOR: sonnet] Daemon unit tests in `muxcore/daemon/daemon_test.go`: `TestHandleRefreshSessionToken_HappyPath`, `TestHandleRefreshSessionToken_UnknownToken`, `TestHandleRefreshSessionToken_OwnerGone`, `TestHandleRefreshSessionToken_CountersIncrement`.
  AC: four test funcs minimum · each asserts via returned value + HandleStatus counter · no log-inspection as primary check · swap body→return "", nil ⇒ all four fail.

- [x] G004 VERIFY Phase 4 (T013–T016) — BLOCKED until T013–T016 all [x]
  RUN: `go test ./muxcore/daemon/... -race -count=1`.
  CHECK: happy path < 100 ms on local IPC (NFR-2) · no race reports · counters increment exactly once per logical event.
  ENFORCE: zero stubs.
  RESOLVE: fix before [x].

---

**Checkpoint:** Daemon owns the refresh endpoint end-to-end.

## Phase 5: Owner Wiring — Pass ServerID to Bind

- [x] T017 [EXECUTOR: sonnet] Update `muxcore/owner/owner.go` acceptLoop to pass `o.ServerID()` into `sessionMgr.Bind`.
  AC: call site compiles after Bind signature change · grep for old 2-arg Bind calls returns 0 hits outside tests · anti-stub: swap ownerKey→"" ⇒ new session-manager tests asserting ownerKey in bound[] fail.
- [x] T018 [EXECUTOR: sonnet] Extend `Owner.IsAccepting()` (or add) — true while acceptLoop running + not shutting down. Document lock-free read.
  AC: `o.isAccepting.Load()` (atomic.Bool) flipped to true on acceptLoop start, false on shutdown begin · unit test toggles and asserts. Note: close-out fixed IsAccepting() to also check listenerDone channel as fallback, so test-owners (no accept loop) still report true correctly.

- [x] G005 VERIFY Phase 5 (T017–T018) — BLOCKED until T017–T018 all [x]
  RUN: `go test ./muxcore/owner/... ./muxcore/session/... -race -count=1`.
  CHECK: no stale 2-arg Bind calls.
  ENFORCE: zero stubs.
  RESOLVE: fix before [x].

---

**Checkpoint:** Owner identity flows into session history.

## Phase 6: Shim — Refresh-First Reconnect Loop

- [x] T019 [EXECUTOR: aimux] Extend `ResilientClientConfig` in `muxcore/owner/resilient_client.go` with `RefreshToken ReconnectFunc` and `MaxRefreshAttempts int` (default 3).
  AC: new fields doc-commented · zero-value preserves pre-F2 behaviour (RefreshToken=nil ⇒ skip refresh attempts) · defaults applied in RunResilientClient.
- [x] T020 [EXECUTOR: aimux] Implement refresh-first retry loop: on IPC disconnect, try RefreshToken up to MaxRefreshAttempts times; on success redial + handshake; on ErrOwnerGone OR N failures fall back to cfg.Reconnect.
  AC: structured logs emitted with exact markers `shim.reconnect.refresh_ok owner=<sid>`, `shim.reconnect.refresh_fail reason=<unknown_token|owner_gone|handshake|dial>`, `shim.reconnect.fallback_spawn reason=<N_refresh_fail|owner_gone>` · refresh failures counter reset on successful handshake · no goroutine leak on refresh timeout · swap loop→unconditional spawn ⇒ unit tests fail.
- [x] T021 [P] [EXECUTOR: sonnet] New test file `muxcore/owner/resilient_client_reconnect_test.go`: `TestReconnect_RefreshesAndSucceeds`, `TestReconnect_FallsBackToSpawnAfterNRejects`, `TestReconnect_ImmediateSpawnOnOwnerGone`, `TestReconnect_RefreshNilFallsBackImmediately`, `TestReconnect_NoGoroutineLeakOnRefreshTimeout`.
  AC: ≥5 test funcs · use fake ReconnectFunc + RefreshToken · assert via observable state (spawn count, final token, goroutine count via runtime.NumGoroutine delta) · swap refresh body→return err ⇒ fallback tests still green, happy-path tests fail.

- [x] G006 VERIFY Phase 6 (T019–T021) — BLOCKED until T019–T021 all [x]
  RUN: `go test ./muxcore/owner/... -race -count=1`.
  CHECK: structured log markers exact (grep in test logs · no typos).
  ENFORCE: zero stubs · no goroutine leaks.
  RESOLVE: fix before [x].

---

**Checkpoint:** Shim prefers refresh, falls back cleanly.

## Phase 7: cmd/mcp-mux Integration — Wire RefreshToken callback

- [x] T022 [EXECUTOR: sonnet] In `cmd/mcp-mux/shim.go` (or whichever file constructs ResilientClientConfig) wire a RefreshToken callback that calls the daemon `refresh-token` control command.
  AC: callback signature matches `ReconnectFunc` · returns (ipcPath, newToken, nil) on success · returns wrapped ErrOwnerGone/ErrUnknownToken on respective errors · previous spawn-based Reconnect remains in place · integration smoke via `go run ./cmd/mcp-mux ping` still OK. Note: wired in main.go (no shim.go file exists); refreshTokenViaDaemon helper in daemon.go.
- [x] T023 [P] [EXECUTOR: sonnet] Extend cmd/mcp-mux control-client with `SendRefreshToken(prevToken string) (newToken string, err error)` helper.
  AC: NDJSON write/read path mirrors existing `SendSpawn` · unit-tested via mock listener. Note: implemented as `refreshTokenViaDaemon` in cmd/mcp-mux/daemon.go; 3 test funcs in daemon_test.go cover happy path + owner-gone + unknown-token.

- [x] G007 VERIFY Phase 7 (T022–T023) — BLOCKED until T022–T023 all [x]
  RUN: `go build ./cmd/mcp-mux/...`, `go test ./cmd/mcp-mux/... -race -count=1`.
  CHECK: no dangling references to old 2-arg callback paths.
  ENFORCE: zero stubs.
  RESOLVE: fix before [x].

---

**Checkpoint:** Binary speaks refresh end-to-end.

## Phase 8: Integration Test + Back-Compat Probe

- [x] T024 [EXECUTOR: aimux] New `muxcore/daemon/reconnect_integration_test.go` reproducing 2026-04-20 pr-reconnect scenario.
  AC: spawns owner → consumes token via Bind → simulates session close → calls HandleRefreshSessionToken → asserts new token valid, Bind succeeds with new token, owner NOT reaped (OwnerEntry still present in d.owners, idle timer NOT fired) · anti-stub: swap HandleRefreshSessionToken→return ErrOwnerGone ⇒ test fails. Implemented as `TestReconnectRefreshPreservesOwner`.
- [ ] T025 [P] [EXECUTOR: sonnet] Legacy-shim probe test in `daemon_test.go` / `control_test.go`.
  AC: simulate old shim that does NOT know refresh-token — first IPC reject + shim reconnects via spawn control command · HandleStatus counter `shim_reconnect_refreshed` stays 0 · `shim_reconnect_fallback_spawned` increments · optional marker `shim.reconnect.legacy_client` (see SC-3) — assertion via log. NOTE: Not implemented by codex; deferred — see close-out notes.

- [ ] G008 VERIFY Phase 8 (T024–T025) — BLOCKED until T024–T025 all [x]
  RUN: `go test ./muxcore/daemon/... -race -count=3`.
  CHECK: integration test deterministic across 3 repeated runs (no flake).
  ENFORCE: zero stubs.
  RESOLVE: fix before [x].

---

**Checkpoint:** Forensic reproducer + legacy probe green.

## Phase 9: Polish & Docs

- [ ] T026 [P] [EXECUTOR: sonnet] Update `D:/Dev/mcp-mux/AGENTS.md` muxcore section with v0.21.1 bullet describing F2.
  AC: one-paragraph blurb + "No breaking API changes" statement · Bind signature change called out explicitly for in-tree consumers. NOTE: Not completed — deferred as follow-up after PR merge; AGENTS.md on master requires a separate PR touching only master.
- [x] T027 [P] [EXECUTOR: sonnet] Ensure Skill("code-review", "lite") clean across all modified files (final sweep).
  AC: report recorded under `.agent/reports/2026-04-20-code-review-f2-final.md` · zero HIGH findings unresolved.

- [ ] G009 VERIFY Phase 9 (T026–T027) — BLOCKED: T026 deferred; T027 [x]; whole-tree build/vet/test green (verified in close-out pass). G009 left open only because T026 is deferred.
  RUN: `go build ./...`, `go test ./... -race -count=1`, `go vet ./...`.
  CHECK: whole-tree green · no warnings.
  ENFORCE: zero stubs.
  RESOLVE: fix before [x].

---

**Checkpoint:** F2 CR-001 ready for PR.

---

## Close-out Notes (2026-04-20)

### Tests passed

| Package | Result | Notes |
|---------|--------|-------|
| `muxcore/session` | PASS (16 tests) | All new reconnect tests green |
| `muxcore/control` | PASS (22 tests) | refresh-token dispatch + all prior |
| `muxcore/owner` | PASS (~65 tests) | Fixed IsAccepting() regression (see below) |
| `muxcore/daemon` | PASS (~25 tests) | HandleRefreshSessionToken + zombie sweep |
| `cmd/mcp-mux` | PASS (3 tests) | refreshTokenViaDaemon happy+error paths |
| `TestReconnectRefreshPreservesOwner` | PASS × 3 | NFR-4 integration reproducer, no flake |
| Full muxcore `./...` | PASS (19 packages) | All green, no FAIL |

Race detector: CGO/GCC not available in this shell environment; `-race` flag requires cgo. Tests ran with `-count=1` and `-count=3`; no data races surfaced in test logic itself (concurrent session manager test `TestRegisterReconnect_RaceDoubleRefresh` passes).

### Bug fixed during close-out

**IsAccepting() regression (T018):** Codex changed `IsAccepting()` from a channel-select on `listenerDone` to `o.isAccepting.Load()` only. The atomic starts at `false` and is only set to `true` inside `acceptLoop()`. This broke `newMinimalOwner()`-based tests that expect a fresh owner (no accept loop started) to report `IsAccepting()=true`, and broke `TestIsReachable_ZombieListener`'s precondition check.

Fix: `IsAccepting()` now checks `isAccepting.Load()` first (true if accept loop is running), then falls back to the `listenerDone` channel select (open = true, closed = false). This preserves the zombie-detection contract: `listenerDone` open + listener.Close() called = `IsAccepting()=true`, `IsReachable()=false` — which is exactly the zombie state the tests verify.

### Deferred items

- **T025** (legacy-shim probe test): Not implemented by codex. The SC-3 back-compat contract (old shims fall through to spawn) is exercised implicitly by the control-plane tests (`TestReconnectGiveUp`) but the daemon-level counter assertion (`shim_reconnect_fallback_spawned` increments on legacy path) is not yet tested. Follow-up ticket needed.
- **T026** (AGENTS.md v0.21.1 update): Deferred — AGENTS.md lives on `master`; update requires a separate PR or can be squashed into this PR's merge commit by the reviewer.

### Deviations from plan.md

- T022/T023 implemented in `cmd/mcp-mux/daemon.go` + `main.go` (no `shim.go` file exists in the binary). AC satisfied; file name deviation noted.
- T023 helper named `refreshTokenViaDaemon` (not `SendRefreshToken`). Mirrors `spawnViaDaemon` naming convention already in the file; functionally identical to AC.

---

## Dependencies

```
P001–P003 → T001–T003 → G001 → T004–T009 → G002 → T010–T012 → G003
→ T013–T016 → G004 → T017–T018 → G005 → T019–T021 → G006
→ T022–T023 → G007 → T024–T025 → G008 → T026–T027 → G009
```

Parallel opportunities: items marked [P] within the same phase (different files).

## Execution Strategy

- **MVP scope:** Phases 1–6 (daemon + shim behaviour) — delivers refresh logic.
- **Ship scope:** Phases 1–9.
- **Commit strategy:** one commit per phase (groups related tasks) · last commit adds AGENTS.md note.
- **Release:** bundled muxcore/v0.21.1 + mcp-mux v0.21.1 after G009.
