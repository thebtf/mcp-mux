# Tasks: upstream-survives-daemon-restart — CR-001 Initial scope

**Spec:** .agent/specs/upstream-survives-daemon-restart/spec.md
**Change Record:** .agent/specs/upstream-survives-daemon-restart/changes/CR-001-initial-scope/change.md
**Plan:** .agent/specs/upstream-survives-daemon-restart/plan.md
**Generated:** 2026-04-19

## Phase 0: Planning

- [x] P001 Confirm executor tiers for all tasks via 5-question Anti-Bypass gate
  AC: each task below has an [EXECUTOR] marker · MAIN-tier count ≤5 · aimux-tier tasks have explicit TDD note · swap body→return null ⇒ N/A (planning)
- [x] P002 Resolve risks R1 + R2 from plan.md (macOS launchd, Windows Job Object nested semantics)
  AC: CI matrix entries for macOS launchd + Windows Server 2022 + Windows 11 added to `.github/workflows/ci.yml` · mitigation documented in plan.md Unknowns & Risks · swap body→return null ⇒ N/A
- [x] P003 Verify FR-28 (`generateToken`, `SessionManager.PreRegister`) + FR-29 (`sockperm.Listen`) surfaces remain stable for reuse
  AC: no new imports of internal FR-28/FR-29 symbols required · reuse surface documented in plan.md Tech Stack · swap body→return null ⇒ N/A

## Phase 1: Foundation (platform-agnostic)

- [x] T001 [EXECUTOR: sonnet] Add `Detach() (pid int, stdin, stdout uintptr, err error)` to `muxcore/upstream/process.go`
  AC: method returns valid FDs without invoking `GracefulKill` · sets `p.detached = true` to prevent subsequent `Close()` from signaling · returns error if already closed OR already detached · 3 unit tests (happy path / double-detach / detach-after-close) in `process_test.go` · swap body→return null ⇒ tests MUST fail

- [x] T002 [EXECUTOR: sonnet] Add `ShutdownForHandoff() (HandoffPayload, error)` to `muxcore/owner/owner.go`
  AC: closes IPC listener + active sessions identically to `Shutdown` · calls `upstream.Detach()` NOT `upstream.Close()` · returns payload with ServerID+PID+FDs+Command+Cwd · `Owner.Shutdown()` after `ShutdownForHandoff()` is safe no-op · 2 unit tests in `owner_test.go` · swap body→return null ⇒ tests MUST fail

- [x] T003 [P] [EXECUTOR: sonnet] Define handoff protocol v1 message types in new file `muxcore/daemon/handoff_proto.go`
  AC: Go types `HelloMsg`, `ReadyMsg`, `FdTransferMsg`, `AckTransferMsg`, `DoneMsg`, `HandoffAckMsg` match plan.md JSON schema · `protocol_version: 1` field is mandatory on every type · MarshalJSON/UnmarshalJSON round-trips cleanly · `handoff_proto_test.go` covers all 6 types with roundtrip + version-mismatch reject · swap body→return null ⇒ tests MUST fail

- [x] T004 [P] [EXECUTOR: sonnet] Extend `OwnerSnapshot` + `DaemonSnapshot` in `muxcore/snapshot/snapshot.go` with `UpstreamPID`, `HandoffSocketPath`, `SpawnPgid`
  AC: new fields default to zero-values on absence (backwards-compat read) · existing v0.20.x snapshots deserialize without error · 2 unit tests: old snapshot round-trip + new snapshot round-trip · swap body→return null ⇒ tests MUST fail

- [x] T005 [P] [EXECUTOR: sonnet] Implement termination-cause classifier in `muxcore/daemon/termination.go`
  AC: `classifyTermination(suture.Event) TerminationCause` returns one of {PlannedHandoff, UpstreamCrash, OperatorStop, IdleEviction, DaemonPanic} · distinguishes clean exit (ErrDoNotRestart or nil err) from crash (non-nil non-ErrDoNotRestart err) · 5 unit tests covering each class · swap body→return null ⇒ tests MUST fail

- [x] T006 [EXECUTOR: sonnet] Platform-agnostic handoff state machine skeleton in `muxcore/daemon/handoff.go`
  AC: `performHandoff(ctx, socketPath, tokenPath) (HandoffResult, error)` + `receiveHandoff(ctx, socketPath, tokenPath) ([]string, error)` compile with platform FD-send/recv stubbed via interface `fdConn` · interface methods: `SendFDs`, `RecvFDs`, `WriteJSON`, `ReadJSON` · stub impl returns `errors.New("not implemented on this platform")` · 3 unit tests using mock fdConn: hello/ready/transfer sequence, version reject, token reject · swap body→return null ⇒ tests MUST fail

- [x] T007 [EXECUTOR: sonnet] Handoff token handshake in `muxcore/daemon/handoff.go` reusing FR-28 primitives
  AC: `writeHandoffToken(dir string) (token, path string, err error)` uses `generateToken()` + writes file with 0600 perms via existing `sockperm` pattern · `verifyHandoffToken(conn net.Conn, expected string)` reads Hello msg, constant-time compare, returns error on mismatch · token file deleted after handoff window · 3 unit tests covering write/verify/mismatch · swap body→return null ⇒ tests MUST fail

- [x] G001 VERIFY Phase 1 (T001–T007) — BLOCKED until T001–T007 all [x]
  RUN: `go test ./muxcore/upstream/... ./muxcore/owner/... ./muxcore/daemon/... ./muxcore/snapshot/...`. Call Skill("nvmd-platform:code-reviewer") on every file touched.
  CHECK: Every new method has anti-stub coverage (tests fail if body returns zero-value). Protocol v1 is wire-format-stable.
  ENFORCE: Zero stubs. Zero TODOs. Zero "platform not yet implemented" comments in non-platform files.
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** Foundation ready — platform-agnostic skeleton compiled and unit-tested; platform impls stubbed behind `fdConn` interface.

## Phase 2: Unix implementation

**Independent Test:** on a Linux runner, spawn a daemon with a mock upstream, invoke `performHandoff` via in-process test harness, verify successor daemon receives the upstream's stdin/stdout FDs and can write/read JSON-RPC through them.

- [ ] T008 [EXECUTOR: sonnet] Add setpgid at spawn in new file `muxcore/upstream/spawn_unix.go` (`//go:build unix`)
  AC: `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` so upstream starts in its own process group · existing `Start` code paths unaffected on non-Unix · 1 integration test in `spawn_unix_test.go` verifying child PGID differs from daemon PGID · swap body→return null ⇒ tests MUST fail

- [ ] T009 [EXECUTOR: sonnet] SCM_RIGHTS sender implementation in new file `muxcore/daemon/handoff_unix.go` (`//go:build unix`)
  AC: implements `fdConn.SendFDs(fds []uintptr, header []byte) error` using `syscall.UnixRights` + `unix.Sendmsg` · handles short writes via retry loop · 2 unit tests using `socketpair`: roundtrip 1 FD + roundtrip 3 FDs · swap body→return null ⇒ tests MUST fail

- [ ] T010 [EXECUTOR: sonnet] SCM_RIGHTS receiver implementation in `muxcore/daemon/handoff_unix.go`
  AC: implements `fdConn.RecvFDs() ([]uintptr, []byte, error)` using `syscall.ParseSocketControlMessage` + `syscall.ParseUnixRights` · handles truncated ancillary buffers gracefully · 2 unit tests paired with T009 sender · swap body→return null ⇒ tests MUST fail

- [ ] T011 [P] [EXECUTOR: sonnet] PID ownership verification in `muxcore/daemon/handoff_unix.go`
  AC: `verifyPIDOwner(pid int) error` checks Linux `/proc/{pid}/status` for matching UID · on macOS: uses `proc_pidinfo` via cgo-free `golang.org/x/sys/unix` · on BSD: `kill(pid, 0) + sysctl` fallback · rejects cross-user PIDs with typed error · 3 unit tests (own process accepted, PID 1 rejected on non-root, invalid PID rejected) · swap body→return null ⇒ tests MUST fail

- [x] T012 [P] [EXECUTOR: sonnet] Integration test: two-process handoff roundtrip on Linux in new file `muxcore/daemon/handoff_integration_linux_test.go`
  AC: test spawns real mock upstream · forks "old" + "new" daemon goroutines in same test binary · full protocol roundtrip: Hello→Ready→FdTransfer→Ack→Done→HandoffAck · new daemon writes to transferred stdin, reads from transferred stdout, asserts JSON-RPC response · swap body→return null ⇒ tests MUST fail

- [x] T013 [P] [EXECUTOR: sonnet] Integration test: macOS launchd-style successor spawn in `muxcore/daemon/handoff_integration_darwin_test.go` (`//go:build darwin`)
  AC: successor daemon started via `exec.Command` with clean env (simulating launchd plist) · handoff succeeds · asserts successor is NOT in old daemon's process group · swap body→return null ⇒ tests MUST fail

- [x] T014 [EXECUTOR: sonnet] Wire Unix platform impl into `performHandoff` / `receiveHandoff` (remove stubs from T006)
  AC: `//go:build unix` build tag resolution picks `handoff_unix.go` implementations · T012 integration test green · swap body→return null ⇒ T012 fails

- [x] G002 VERIFY Phase 2 (T008–T014) — BLOCKED until T008–T014 all [x]
  RUN: `go test ./muxcore/upstream/ ./muxcore/daemon/ -run Handoff -tags unix -v`. Call Skill("nvmd-platform:code-reviewer") on `handoff_unix.go` + `spawn_unix.go`.
  CHECK: SCM_RIGHTS roundtrip works on Linux runner and macOS runner. Circuit breaker does NOT increment on successful handoff.
  ENFORCE: Zero stubs. Setpgid fires on every upstream spawn (verified by PGID check).
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** Unix handoff works end-to-end on Linux + macOS; existing behavior unchanged for non-Unix builds.

## Phase 3: Windows implementation

**Independent Test:** on a Windows runner, spawn a daemon with a mock upstream, invoke handoff protocol over a named pipe, verify DuplicateHandle'd stdin/stdout pipes function in the successor process.

- [ ] T015 [EXECUTOR: sonnet] Per-upstream Job Object spawn in new file `muxcore/upstream/spawn_windows.go` (`//go:build windows`)
  AC: each upstream's `cmd` gets `CreateJobObject` + `AssignProcessToJobObject` with `JOB_OBJECT_LIMIT_BREAKAWAY_OK | JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` on the upstream's OWN job (not daemon's) · daemon process does NOT hold the Job Object handle — only the process handle returned by `cmd.Process.Handle` · 1 test in `spawn_windows_test.go` kills daemon process, asserts upstream still alive · swap body→return null ⇒ tests MUST fail

- [ ] T016 [EXECUTOR: sonnet] Named pipe listener + dialer in new file `muxcore/daemon/handoff_windows.go` (`//go:build windows`) using `github.com/Microsoft/go-winio`
  AC: `listenHandoffPipe(name string) (net.Listener, error)` uses `winio.ListenPipe` with DACL restricting to current user SID · `dialHandoffPipe(name string, timeout time.Duration)` uses `winio.DialPipe` · 2 unit tests: listen+accept+dial loop, denied-ACL rejection · swap body→return null ⇒ tests MUST fail

- [ ] T017 [EXECUTOR: sonnet] DuplicateHandle sender + receiver in `muxcore/daemon/handoff_windows.go`
  AC: `fdConn.SendFDs` sends JSON header announcing handle count + sequentially `DuplicateHandle`s each handle into successor process (uses successor PID from Hello msg) · receiver uses PID from successor-local view — no round-trip needed on reverse · 2 unit tests using paired processes · swap body→return null ⇒ tests MUST fail

- [ ] T018 [P] [EXECUTOR: sonnet] Logon session ownership check in `muxcore/daemon/handoff_windows.go`
  AC: `verifyPIDOwner(pid uint32) error` uses `OpenProcess` + `GetTokenInformation` to retrieve logon SID · rejects cross-session PIDs · 3 unit tests (own SID accepted, SYSTEM rejected if not SYSTEM, invalid PID rejected) · swap body→return null ⇒ tests MUST fail

- [x] T019 [P] [EXECUTOR: sonnet] Integration test: two-process handoff on Windows in `muxcore/daemon/handoff_integration_windows_test.go`
  AC: spawns mock upstream · runs full protocol over named pipe · asserts DuplicateHandle'd stdin writes reach upstream, stdout reads return upstream's response · swap body→return null ⇒ tests MUST fail

- [x] T020 [EXECUTOR: sonnet] Wire Windows platform impl into `performHandoff` / `receiveHandoff`
  AC: `//go:build windows` tag resolution picks `handoff_windows.go` · T019 green · swap body→return null ⇒ T019 fails

- [ ] G003 VERIFY Phase 3 (T015–T020) — BLOCKED until T015–T020 all [x]
  RUN: `go test ./muxcore/upstream/ ./muxcore/daemon/ -run Handoff -tags windows -v` on Windows runner. Call Skill("nvmd-platform:code-reviewer") on `handoff_windows.go` + `spawn_windows.go`.
  CHECK: Job Object BREAKAWAY_OK verified (daemon kill leaves upstream alive). DuplicateHandle roundtrip works.
  ENFORCE: Zero stubs. Windows-specific error codes surfaced clearly to caller.
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** Windows handoff works end-to-end on Windows Server 2022 + Windows 11 CI matrix.

## Phase 4: Orchestration and degraded fallback

**Independent Test:** on the primary dev platform, invoke `mcp-mux upgrade --restart` against a live daemon with 3 real upstreams (1 shared, 1 isolated, 1 persistent). Assert all 3 upstreams retain their PIDs post-restart and shim reconnects succeed within 3s. Also test: kill the handoff socket mid-transfer — verify FR-8 fallback kicks in for affected upstream without breaking others.

- [ ] T021 [EXECUTOR: sonnet] Wire `performHandoff` into `HandleGracefulRestart` in `muxcore/daemon/daemon.go`
  AC: before calling `d.Shutdown()`, daemon (a) writes handoff token, (b) binds `handoff_control.sock`, (c) waits up to 10s for in-flight `backgroundSpawnCh` signals (C6), (d) calls `performHandoff` · on handoff result Transferred: calls `Owner.ShutdownForHandoff` · on Aborted: calls existing `Owner.Shutdown` · socket unbind + token delete in deferred cleanup · 3 unit tests: all-transferred / mixed / all-aborted · swap body→return null ⇒ tests MUST fail

- [ ] T022 [EXECUTOR: sonnet] Reattach branch in `loadSnapshot` at `muxcore/daemon/snapshot.go`
  AC: when `ownerSnap.UpstreamPID > 0` AND `ownerSnap.HandoffSocketPath != ""`: call `receiveHandoff` once per daemon startup · received upstreams create Owner via new `NewOwnerFromHandoff(cfg, pid, stdinFD, stdoutFD)` · remaining snapshot owners with zero PID fall through to existing `SpawnUpstreamBackground` · 2 integration tests: all-handed-off / mixed · swap body→return null ⇒ tests MUST fail

- [ ] T023 [P] [EXECUTOR: sonnet] Add `NewOwnerFromHandoff` constructor in `muxcore/owner/owner.go`
  AC: same shape as `NewOwnerFromSnapshot` but accepts an already-running `*upstream.Process` reattached via PID+FDs · skips `SpawnUpstreamBackground` · init caches still populated from snapshot · 2 unit tests + 1 integration test covering session pre-register · swap body→return null ⇒ tests MUST fail

- [ ] T024 [P] [EXECUTOR: sonnet] Implement `SoftRemove` path in `muxcore/daemon/reaper.go`
  AC: `SoftRemove(sid)` closes upstream stdin + waits up to 30s on `Process.Done` · only escalates to `upstream.Close()` (SIGTERM) on timeout · reaper `sweep` routes idle eviction to `SoftRemove` · operator `Remove` + crash paths still use hard `Shutdown` · 3 unit tests: clean-exit-before-timeout / stdin-close-timeout / already-dead-upstream · swap body→return null ⇒ tests MUST fail

- [ ] T025 [EXECUTOR: sonnet] Structured handoff logging in `muxcore/daemon/handoff.go` + metric counters in `HandleStatus`
  AC: phases `handoff_begin`, `fd_serialized`, `fd_transferred`, `upstream_reattached`, `handoff_complete` logged with duration_ms · `HandleStatus` exposes counters per termination class (`planned_handoff_count`, `upstream_crash_count`, `idle_eviction_count`, `operator_stop_count`, `daemon_panic_count`) · `mux_list --verbose` surfaces last-handoff-duration · 2 unit tests + 1 integration test asserting log lines emitted · swap body→return null ⇒ tests MUST fail

- [ ] T026 [EXECUTOR: sonnet] FR-8 degraded fallback integration test in `muxcore/daemon/handoff_fallback_test.go`
  AC: test blocks `handoff_control.sock` binding (port collision) · `HandleGracefulRestart` detects bind failure · falls back to existing `Shutdown → SpawnUpstreamBackground` path · shims see `-32603 upstream restarted` as today · log line `handoff_fallback_reason=socket_bind_failed` emitted · swap body→return null ⇒ test MUST fail

- [ ] G004 VERIFY Phase 4 (T021–T026) — BLOCKED until T021–T026 all [x]
  RUN: `go test ./muxcore/daemon/... -v`. Call Skill("nvmd-platform:code-reviewer") on all modified files. Execute independent test from phase header on dev box.
  CHECK: All 3 termination paths produce distinct log classification. FR-8 fallback fires transparently.
  ENFORCE: Zero stubs. Termination classifier wired into supervisor `EventServiceTerminate` handler.
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** Orchestration complete — `mcp-mux upgrade --restart` uses handoff by default, FR-8 fallback covers all failure paths.

## Phase 5: Integration, benchmarks, migration

**Independent Test:** production-parity smoke — run 15 simultaneous upstreams of mixed kinds (aimux, engram, pr, serena, etc.), invoke `mcp-mux upgrade --restart`, assert zero `-32603` errors across all shim journals.

- [ ] T027 [EXECUTOR: sonnet] Benchmark: 15-upstream handoff latency in `muxcore/daemon/handoff_benchmark_test.go`
  AC: `BenchmarkHandoff15Upstreams` completes in <2000ms on CI reference hardware (4-core, 16GB) · asserts NFR-1 via `if elapsed > 2*time.Second { t.Fatalf(...) }` · reports duration in benchmark output · swap body→return null ⇒ benchmark fails

- [ ] T028 [P] [EXECUTOR: sonnet] Benchmark: 50 + 100 upstream linear scaling validation
  AC: `BenchmarkHandoff50Upstreams` ≤ 3.25 × `BenchmarkHandoff15Upstreams` · `BenchmarkHandoff100Upstreams` ≤ 6.5× baseline · linear-scaling assertion via ratio check · swap body→return null ⇒ benchmark fails

- [ ] T029 [P] [EXECUTOR: sonnet] Integration test: daemon SIGKILL + successor reattach in `muxcore/daemon/daemon_crash_test.go`
  AC: test forks helper daemon process · `kill -9 <pid>` (or `TerminateProcess` on Windows) · new daemon started against same snapshot · reattaches to upstreams by PID · JSON-RPC roundtrip succeeds post-reattach (US4-AC1..AC3) · swap body→return null ⇒ tests MUST fail

- [ ] T030 [P] [EXECUTOR: sonnet] Integration test: mixed crash + handoff in `muxcore/daemon/handoff_mixed_test.go`
  AC: 3 upstreams configured · 1 crashes mid-handoff (simulated via Ack `ok: false`) · 2 successfully transferred · FR-7 per-upstream atomicity verified · crashed upstream re-spawns on successor · others retain PID · swap body→return null ⇒ tests MUST fail

- [ ] T031 [P] [EXECUTOR: sonnet] Integration test: handoff protocol version-skew in `muxcore/daemon/handoff_version_test.go`
  AC: successor sends Hello with `protocol_version: 2` · old daemon rejects with typed error · falls back to FR-8 · alarm-level log `handoff_rejected_version` emitted · swap body→return null ⇒ tests MUST fail

- [ ] T032 [EXECUTOR: sonnet] Integration test: token mismatch in `muxcore/daemon/handoff_auth_test.go`
  AC: successor presents wrong token · old daemon rejects · FR-8 fires · constant-time compare used (benchmark-detectable via `testing/timing`) · swap body→return null ⇒ tests MUST fail

- [ ] T033 [EXECUTOR: sonnet] README.md + README.ru.md update for lifecycle semantics in `README.md`, `README.ru.md`
  AC: new "Upstream Lifecycle" section after "Resilient Shim" explains handoff model · both files maintain ru-score ≥7.0 · constitution #9 realization noted · examples of before/after for `upgrade --restart` · swap body→return null ⇒ N/A (docs)

- [ ] T034 [P] [EXECUTOR: MAIN] Update `.agent/CONTINUITY.md` with v0.21.0/v0.10.0 status
  AC: current state section replaced with handoff arc summary · engram #109 marked "resolved in v0.21.0" · swap body→return null ⇒ N/A (docs)

- [ ] T035 [P] [EXECUTOR: MAIN] Update muxcore release notes in new file `.agent/data/release-notes-muxcore-v0.21.0.md` + mcp-mux v0.10.0 notes
  AC: breaking-change section documents `Owner.ShutdownForHandoff` + `Process.Detach` additive API · migration guide for aimux/engram consumers (no action required for drop-in bump) · swap body→return null ⇒ N/A (docs)

- [ ] T036 [EXECUTOR: sonnet] Post-deploy verification script `scripts/verify-handoff.sh` (POSIX) + `.ps1` (Windows)
  AC: scripts connect to local daemon, record 3 upstream PIDs, invoke `upgrade --restart`, wait 10s, assert all PIDs unchanged, assert zero -32603 in daemon log since restart timestamp · exit 0 on pass, exit 1 + diagnostic on fail · swap body→return null ⇒ script exits non-zero

- [ ] G005 VERIFY Phase 5 (T027–T036) — BLOCKED until T027–T036 all [x]
  RUN: `go test ./muxcore/... -bench=Handoff -v` + `bash scripts/verify-handoff.sh` on Linux + `pwsh scripts/verify-handoff.ps1` on Windows. Call Skill("nvmd-platform:code-reviewer") on docs + scripts.
  CHECK: NFR-1 <2s for 15 upstreams enforced via test assertion. Linear scaling validated. Daemon-crash recovery green. Docs render correctly.
  ENFORCE: Zero stubs. All shipped scripts idempotent.
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** v0.21.0 / v0.10.0 release-ready.

## Phase 6: Polish and cross-cutting

- [ ] T037 [EXECUTOR: sonnet] Update engram issue #109 with resolution + release target
  AC: comment added noting resolution via handoff mechanism · status transitioned to `acknowledged` pending v0.21.0 release · mentions deferred follow-on retry spec · swap body→return null ⇒ N/A (tracker)

- [ ] T038 [EXECUTOR: MAIN] Draft follow-on spec `inflight-idempotent-retry` as CR-002 or sibling feature folder
  AC: skeleton spec.md + change.md created · scope: retry only as defense-in-depth for FR-8 fallback path · target v0.21.1 · linked from this spec's Out of Scope section · swap body→return null ⇒ N/A (docs)

- [ ] G006 VERIFY Phase 6 (T037–T038) — BLOCKED until T037–T038 all [x]
  RUN: Manual review of engram comment + follow-on spec skeleton.
  CHECK: Cross-references between specs are bidirectional. Release notes reflect final state.
  ENFORCE: No loose ends.
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** CR-001 complete — spec status can transition Draft → Active → Implemented.

## Dependencies

- Phase 0 (P001–P003) blocks Phase 1 — executor tiers + CI matrix must exist before work starts
- Phase 1 (T001–T007 + G001) blocks Phase 2 AND Phase 3 (platform-agnostic skeleton)
- Phase 2 (T008–T014 + G002) and Phase 3 (T015–T020 + G003) are INDEPENDENT — parallelizable across platform specialists
- Phase 4 (T021–T026 + G004) requires Phase 2 OR Phase 3 complete; both required for final integration
- Phase 5 (T027–T036 + G005) requires Phase 4 complete
- Phase 6 (T037–T038 + G006) requires Phase 5 complete

## Parallelization Plan

Within Phase 1: T003, T004, T005 are [P] — no shared files. T006 depends on T003. T007 reuses FR-28 surfaces, independent of others.

Within Phase 2: T011, T012, T013 are [P] — different test files / concerns.

Within Phase 3: T018, T019 are [P] — different test files.

Within Phase 4: T023, T024 are [P].

Within Phase 5: T028, T029, T030, T031 are [P] — all in distinct test files. T034, T035 are [P] — different docs files.

**Cross-phase parallelism:** After G001, Phase 2 and Phase 3 work can proceed in parallel worktrees (platform specialists).

## Execution Strategy

- **MVP scope:** Phases 0–4 (Unix + Windows + orchestration). Phase 5 benchmarks are release gates, not MVP gates.
- **Release blocker:** G005 must pass before `mcp-mux/v0.10.0` tag.
- **Commit strategy:** one commit per completed T-task. GATE marks aggregate phase commit with full review pass.
- **Platform owners:** Phase 2 → Unix-focused agent. Phase 3 → Windows-focused agent (ideally via `aimux` agent if Opus orchestrator cannot reach Windows CI from main session).










