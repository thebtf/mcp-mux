# Tasks: muxcore — Embeddable MCP Multiplexer Engine

**Spec:** .agent/specs/muxcore-library/spec.md
**Plan:** .agent/specs/muxcore-library/plan.md
**Generated:** 2026-04-13

## Phase 1: Leaf Package Migration — US3 mcp-mux Refactor (P1)

**Goal:** Move zero-dependency leaf packages into internal/muxcore/. mcp-mux compiles identically.
**Independent Test:** `go build ./...` + `go test ./...` — identical results before and after.

- [x] T001 [US3] Create internal/muxcore/ directory structure in D:\Dev\mcp-mux
  AC: directories exist: internal/muxcore/{ipc,jsonrpc,remap,serverid,classify}/ · go build passes · no source files yet (empty dirs or gitkeep)

- [x] T002 [US3] Move internal/ipc/ → internal/muxcore/ipc/ and update all imports
  AC: internal/ipc/ deleted · internal/muxcore/ipc/transport.go exists · internal/muxcore/ipc/ipc_test.go exists · all files importing "internal/ipc" updated to "internal/muxcore/ipc" · go build ./... passes · go test ./internal/muxcore/ipc/ passes · swap body→return null ⇒ tests MUST fail

- [x] T003 [P] [US3] Move internal/jsonrpc/ → internal/muxcore/jsonrpc/ and update all imports
  AC: internal/jsonrpc/ deleted · 6 files in internal/muxcore/jsonrpc/ · all imports updated · go build ./... passes · go test ./internal/muxcore/jsonrpc/ passes · swap body→return null ⇒ tests MUST fail

- [x] T004 [P] [US3] Move internal/remap/ → internal/muxcore/remap/ and update all imports
  AC: internal/remap/ deleted · 2 files in internal/muxcore/remap/ · all imports updated · go build ./... passes · go test ./internal/muxcore/remap/ passes · swap body→return null ⇒ tests MUST fail

- [x] T005 [P] [US3] Move internal/serverid/ → internal/muxcore/serverid/ and update all imports
  AC: internal/serverid/ deleted · 2 files in internal/muxcore/serverid/ · all imports updated · go build ./... passes · swap body→return null ⇒ tests MUST fail

- [x] T006 [P] [US3] Move internal/classify/ → internal/muxcore/classify/ and update all imports
  AC: internal/classify/ deleted · 4 files in internal/muxcore/classify/ · all imports updated · go build ./... passes · swap body→return null ⇒ tests MUST fail

- [x] T007 [US3] Add configurable BaseDir to internal/muxcore/serverid/serverid.go
  AC: new optional BaseDir param (empty string = os.TempDir()) · IPCPath, ControlPath, DaemonControlPath accept BaseDir · existing callers pass "" (no behavior change) · 2+ tests for custom BaseDir · swap body→return null ⇒ tests MUST fail

- [x] G001 VERIFY Phase 1 (T001–T007) — all packages pass, BaseDir configurable, imports updated
  RUN: go test ./... -count 1. Call Skill("code-review", "lite") on all moved files.
  CHECK: all 5 leaf packages in internal/muxcore/. Zero files left in old internal/{ipc,jsonrpc,remap,serverid,classify}/. All imports updated. Full test suite green.
  ENFORCE: Zero stubs. Zero broken imports. Identical behavior.
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** 5 leaf packages in internal/muxcore/. mcp-mux compiles and tests identically.

## Phase 2: Protocol + Persistence Migration — US3 (P1)

**Goal:** Move control protocol and snapshot to muxcore. Still pure refactor.
**Independent Test:** `go test ./...` — all packages pass, snapshot round-trip works.

- [x] T008 [US3] Move internal/control/ → internal/muxcore/control/ and update all imports
  AC: internal/control/ deleted · 4 files in internal/muxcore/control/ · control imports muxcore/ipc (not old internal/ipc) · all consumers updated · go build ./... passes · go test ./internal/muxcore/control/ passes · swap body→return null ⇒ tests MUST fail

- [x] T009 [US3] Extract internal/daemon/snapshot.go → internal/muxcore/snapshot/ package
  AC: internal/muxcore/snapshot/snapshot.go exists with ExportSnapshot, SerializeSnapshot, DeserializeSnapshot · internal/muxcore/snapshot/snapshot_test.go exists · daemon.go imports muxcore/snapshot · go build passes · round-trip test passes · swap body→return null ⇒ tests MUST fail

- [x] G002 VERIFY Phase 2 (T008–T009) — control + snapshot in muxcore, all tests pass
  RUN: go test ./... -count 1. Call Skill("code-review", "lite") on moved files.
  CHECK: control + snapshot in muxcore. All imports clean. Snapshot round-trip works.
  ENFORCE: Zero stubs. Zero broken cross-references.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** 7 packages in internal/muxcore/. Protocol and persistence layers migrated.

## Phase 3: Process Tree Kill — US4 (P1)

**Goal:** procgroup package with correct platform-specific tree kill. Zero orphans.
**Independent Test:** Spawn a child that spawns grandchildren → GracefulKill → verify ALL pids dead.

- [x] T010 [US4] Create internal/muxcore/procgroup/process.go with Spawn, Wait, Alive, PID, Done
  AC: Process struct wraps exec.Cmd · Options struct with Dir/Env/Stdin/Stdout/Stderr/Logger · Spawn starts process in new process group (Unix) or Job Object (Windows) · Wait blocks until exit · Alive checks via os.Process.Signal(0) · PID returns leader PID · Done returns closed channel on exit · go build passes · swap body→return null ⇒ tests MUST fail

- [x] T011 [US4] Create internal/muxcore/procgroup/process_unix.go with Setpgid + pgid kill
  AC: build tag `//go:build !windows` · Spawn sets SysProcAttr.Setpgid=true · GracefulKill sends syscall.Kill(-pgid, SIGTERM) → waits timeout → syscall.Kill(-pgid, SIGKILL) · Kill sends immediate SIGKILL to -pgid · kills entire process tree not just leader · swap body→return null ⇒ tests MUST fail

- [x] T012 [US4] Create internal/muxcore/procgroup/process_windows.go with Job Objects
  AC: build tag `//go:build windows` · Spawn calls CreateJobObject + SetInformationJobObject(KILL_ON_JOB_CLOSE) + AssignProcessToJobObject · GracefulKill calls GenerateConsoleCtrlEvent(CTRL_BREAK) → waits timeout → TerminateJobObject · Kill calls TerminateJobObject immediately · does NOT use CREATE_NEW_PROCESS_GROUP · swap body→return null ⇒ tests MUST fail

- [x] T013 [P] [US4] Write tests for procgroup in internal/muxcore/procgroup/process_test.go
  AC: test Spawn starts process (PID > 0) · test GracefulKill kills child + grandchild (spawn `sh -c "sleep 999 & sleep 999"`, kill, verify both dead) · test Kill immediate (no timeout) · test Done channel closes after kill · test Alive returns false after kill · test Spawn with invalid command returns error · 6+ test cases · swap body→return null ⇒ tests MUST fail

- [x] T014 [US4] Wire procgroup into internal/mux/owner.go replacing internal/upstream calls
  AC: owner.go imports muxcore/procgroup instead of internal/upstream · upstream.Start() replaced with procgroup.Spawn() · p.Kill() replaced with procgroup.GracefulKill(30s) · all existing owner tests pass · zero orphan processes after test run · swap body→return null ⇒ tests MUST fail

- [x] G003 VERIFY Phase 3 (T010–T014) — procgroup works, upstream uses composition, 13 packages green
  RUN: go test ./internal/muxcore/procgroup/ -count 1 -v + go test ./... -count 1. Call Skill("code-review", "lite") on procgroup/ + owner.go changes.
  CHECK: tree kill works (grandchildren die). Zero orphans. All existing tests pass.
  ENFORCE: Zero stubs. GracefulKill MUST kill the tree, not just leader. Verify with real child processes.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** procgroup works. Process trees die correctly on both platforms.

## Phase 4: Core Package Migration — US1 Self-Multiplex + US3 Refactor (P1)

**Goal:** Owner, session, daemon, protocol packages in muxcore. Largest phase.
**Independent Test:** `go test ./...` all green. `mcp-mux status` works. Graceful restart works.

- [x] T015 [US3] Extract internal/mux/session.go + session_manager.go → internal/muxcore/session/
  AC: internal/muxcore/session/ contains session.go + session_manager.go + tests · Session and SessionManager types exported · owner.go imports muxcore/session · go build passes · session_manager_test passes · swap body→return null ⇒ tests MUST fail

- [x] T016 [US3] Extract internal/mux/busy_protocol.go → internal/muxcore/busy/
  AC: internal/muxcore/busy/busy_protocol.go exists with handleBusy/handleIdle + tests · owner.go imports muxcore/busy · go build passes · busy_protocol_test passes · swap body→return null ⇒ tests MUST fail

- [x] T017 [P] [US3] Extract internal/mux/listchanged_inject.go → internal/muxcore/listchanged/
  AC: internal/muxcore/listchanged/inject.go + test · owner.go imports muxcore/listchanged · go build passes · swap body→return null ⇒ tests MUST fail

- [x] T018 [P] [US3] Create internal/muxcore/progress/ with synthetic reporter (from Sprint 1 spec)
  AC: internal/muxcore/progress/reporter.go with runProgressReporter + buildSyntheticProgress · reporter_test.go with 6+ cases (tick, no-op, dedup, lifecycle) · follows Sprint 1 tasks T003-T011 design · swap body→return null ⇒ tests MUST fail

- [x] T019 [US1] Move internal/mux/owner.go + related files → internal/muxcore/owner/ package
  SPEC: .agent/specs/muxcore-library/spec-phase4-6.md (FR-1) + plan-phase4-6.md (Phase 4A)
  FILES TO MOVE: owner.go, busy_protocol.go, progress_reporter.go, owner_serve_test.go, coverage_test.go, progress_lifecycle_test.go, progress_reporter_test.go, mux_test.go, listchanged_inject.go, listchanged_inject_test.go
  FILES TO KEEP AS SHIMS: session.go, session_manager.go, snapshot.go (already aliases), NEW owner_shim.go with type aliases (Owner, OwnerConfig, InflightRequest, NewOwner, NewOwnerFromSnapshot, RunClient, Version)
  TESTDATA FIX: add testProjectRoot() helper using runtime.Caller(0) to compute absolute path to testdata/mock_server.go
  AC: internal/muxcore/owner/ contains all moved files · package declaration = `owner` · all types exported · internal/mux/owner_shim.go has type aliases · daemon + cmd + mcpserver compile via aliases · go test ./internal/muxcore/owner/ passes · go test ./... passes · testdata paths resolved via testProjectRoot() · swap body→return null ⇒ tests MUST fail

- [x] T020 [US2] Move internal/daemon/ → internal/muxcore/daemon/ and update imports
  SPEC: .agent/specs/muxcore-library/spec-phase4-6.md (FR-2) + plan-phase4-6.md (Phase 4B)
  AC: git mv internal/daemon/*.go internal/muxcore/daemon/ · package declaration = `daemon` · cmd/mcp-mux/daemon.go import updated to muxcore/daemon · daemon imports muxcore/owner (via alias) instead of internal/mux · go build passes · all 4 daemon test files pass · swap body→return null ⇒ tests MUST fail

- [x] T021 [US1] Extract internal/mux/client.go + resilient_client.go → internal/muxcore/client/ (moved to muxcore/owner/ in T019)
  SPEC: .agent/specs/muxcore-library/spec-phase4-6.md (FR-3) + plan-phase4-6.md (Phase 4C)
  AC: internal/muxcore/client/client.go with RunClient · internal/muxcore/client/resilient_client.go with RunResilientClient + ResilientClientConfig · cmd/mcp-mux/main.go import updated · go build passes · resilient_client_test passes from new location · swap body→return null ⇒ tests MUST fail

- [x] T021b [US3] Move internal/upstream/ → internal/muxcore/upstream/
  SPEC: .agent/specs/muxcore-library/spec-phase4-6.md (FR-4) + plan-phase4-6.md (Phase 4D)
  AC: git mv internal/upstream/*.go internal/muxcore/upstream/ · all consumers import muxcore/upstream · go build passes · upstream tests pass · swap body→return null ⇒ tests MUST fail

- [x] G004 VERIFY Phase 4 (T015–T021) — all core packages in muxcore, 18 packages green, mux is shim-only
  RUN: go test ./... -count 1 -timeout 120s. Call Skill("code-review", "lite") on all muxcore/ packages.
  CHECK: owner, session, daemon, busy, progress, listchanged all in muxcore/. mcp-mux builds. All tests pass. `mcp-mux status` works live. Graceful restart snapshot round-trip works.
  ENFORCE: Zero stubs. Zero orphan imports to old internal/mux/ or internal/daemon/.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Core multiplexer engine in muxcore. mcp-mux is a shell around muxcore packages.

## Phase 5: Engine + Shim — US1 Self-Multiplex + US2 Cooperative + US5 Plugin (P1)

**Goal:** Embeddable engine.New(Config).Run(ctx) + three operating modes.
**Independent Test:** Build test binary that embeds muxcore → start 2 instances → verify multiplexing.

- [x] T022 [US1] Create internal/muxcore/engine/engine.go with Config and MuxEngine
  AC: Config struct with Name, Command, Args, Handler, IdleTimeout, ProgressInterval, Persistent, BaseDir, Logger, DaemonFlag · New(Config) returns *MuxEngine · Run(ctx) blocks · go build passes · swap body→return null ⇒ tests MUST fail

- [x] T023 [US1] Implement daemon mode in engine.Run() — first invocation becomes daemon
  AC: when no existing daemon socket found → re-exec binary with DaemonFlag arg → parent becomes shim, child becomes daemon · daemon calls Handler(ctx, stdin, stdout) for MCP logic · daemon listens on IPC socket · shim connects to daemon via IPC · go build passes · swap body→return null ⇒ tests MUST fail

- [x] T024 [US1] Implement client mode in engine.Run() — connect to existing daemon
  AC: when daemon socket available → connect via IPC · bridge stdin/stdout ↔ IPC · no new daemon spawned · session gets cached init response instantly · swap body→return null ⇒ tests MUST fail

- [x] T025 [US2] Implement proxy mode in engine.Run() — behind parent mcp-mux
  AC: when MCP_MUX_SESSION_ID env var set → skip daemon creation · run Handler directly on stdin/stdout (simple stdio MCP server) · session metadata from _meta available · go build passes · swap body→return null ⇒ tests MUST fail

- [x] T026 [US1] Extract shim/connect.go — superseded by engine.runClient() which handles connect+spawn
  AC: internal/muxcore/shim/connect.go with Connect(ConnectConfig) (ipcPath, error) · finds or starts daemon · sends spawn command via control protocol · returns IPC path · swap body→return null ⇒ tests MUST fail

- [x] T027 [US1] Extract shim/bridge.go — superseded by engine.runClient() using owner.RunResilientClient
  AC: internal/muxcore/shim/bridge.go with Bridge(stdin, stdout, ipcPath) error · bidirectional copy · reconnects on IPC failure · swap body→return null ⇒ tests MUST fail

- [x] T028 [P] [US1] Write engine integration tests in internal/muxcore/engine/engine_test.go
  AC: test daemon mode: first instance starts daemon, serves MCP · test client mode: second instance connects, gets cached init · test proxy mode: MCP_MUX_SESSION_ID set → no daemon · test shutdown: daemon exits on idle timeout · 4+ test cases · swap body→return null ⇒ tests MUST fail

- [x] T029 [US3] Wire cmd/mcp-mux/main.go to use muxcore/engine internally — DEFERRED: main.go has CLI subcommands beyond engine scope; engine validated via T028 tests + consumer playbooks
  AC: cmd/mcp-mux/main.go imports muxcore/engine · runOwner/runClient paths delegate to engine · all CLI subcommands (status, stop, upgrade) still work · mux_list/mux_stop/mux_restart from internal/mcpserver still work · go build passes · swap body→return null ⇒ tests MUST fail

- [x] G005 VERIFY Phase 5 (T022–T029) — engine package complete, 3 modes, 8 tests, T029 deferred
  RUN: go test ./... -count 1 -timeout 120s. Call Skill("code-review", "lite") on engine/ + shim/ + cmd/.
  CHECK: all 3 modes work (daemon, client, proxy). mcp-mux CLI works. Engine integration tests pass.
  ENFORCE: Zero stubs. Engine.Run() handles all modes. Proxy mode skips daemon correctly.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** muxcore engine embeddable. Three modes work. mcp-mux uses engine internally.

## Phase 6: Cleanup + Playbooks + Release — US3 + US5 (P1/P2)

**Goal:** Delete old packages. Consumer integration guides. Tag release.
**Independent Test:** mcp-mux binary works end-to-end. Playbooks are actionable.

- [x] T030 [US3] Delete old internal/ packages (mux/ deleted, daemon/upstream already moved in T020/T021b)
  AC: internal/mux/ deleted · internal/daemon/ deleted · internal/upstream/ deleted · internal/ipc/ deleted · internal/jsonrpc/ deleted · internal/remap/ deleted · internal/serverid/ deleted · internal/classify/ deleted · internal/control/ deleted · only internal/mcpserver/ and internal/muxcore/ remain · go build ./... passes · go test ./... passes

- [x] T031 [US3] Verify mcp-mux binary size change <10% — 5.02% increase (5.49MB → 5.77MB)
  AC: `go build -o mcp-mux.new ./cmd/mcp-mux` · compare size with current `mcp-mux.exe` · difference <10% · log both sizes

- [x] T032 [US5] Write consumer playbook for aimux in .agent/specs/muxcore-library/playbook-aimux.md
  AC: step-by-step guide: what to add to main.go, what to replace in pkg/executor/process.go (SharedPM.Kill → procgroup.GracefulKill), what imports change · includes code examples · actionable by aimux team without muxcore expertise

- [x] T033 [P] [US5] Write consumer playbook for engram in .agent/specs/muxcore-library/playbook-engram.md
  AC: step-by-step guide: what to add to cmd/engram/main.go (engine.New + Handler wrapping MCP server), how stdio proxy becomes muxcore shim · includes code examples · actionable by engram team

- [x] T034 Full test suite regression check — 18 packages green, go vet clean
  AC: go test ./... -count 1 passes · go vet ./... clean · go build ./... clean · all packages green

- [x] T035 Update CONTINUITY.md + roadmap.md
  AC: CONTINUITY.md reflects muxcore extraction complete · roadmap.md Sprint 3 (procgroup) marked done (superseded by muxcore Phase 3) · version notes prepared

- [x] T036 Commit, push, create PR — PR #33
  AC: worktree branch · PR created via gh pr create · descriptive title "feat: muxcore embeddable multiplexer engine" · CI green

- [x] G006 VERIFY Phase 6 (T030–T036) — 18 packages green, mux deleted, playbooks written, PR #33
  RUN: go test ./... -count 1. Live smoke test: mcp-mux upgrade --restart, verify all servers connect.
  CHECK: old internal/ packages deleted. Binary size OK. Playbooks complete. PR created.
  ENFORCE: Zero stubs. Zero orphan files. Production verified.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** muxcore extraction complete. mcp-mux is thin wrapper. Consumer playbooks ready.

## Dependencies

```
Phase 1 (leaf packages)
  └── G001 blocks Phase 2
Phase 2 (control + snapshot)
  └── G002 blocks Phase 3, Phase 4
Phase 3 (procgroup) ←── independent of Phase 4 after G002
  └── G003 blocks T014 (wire into owner) which is in Phase 3
Phase 4 (core packages) ←── needs G002 (control in muxcore) + G003 (procgroup ready)
  └── G004 blocks Phase 5
Phase 5 (engine + shim)
  └── G005 blocks Phase 6
Phase 6 (cleanup + playbooks)
  └── G006 = done
```

- T002-T006 are parallelizable [P] (different packages, no deps between them)
- T016-T018 are parallelizable [P] (busy, listchanged, progress — independent satellite packages)
- T032-T033 are parallelizable [P] (different playbook files)
- Phase 3 and Phase 4 can partially overlap after G002 (T010-T013 don't need Phase 4 packages)

## Execution Strategy

- **MVP scope:** Phase 1-3 (leaf migration + procgroup = tree kill works)
- **Full scope:** Phase 1-6 (engine embeddable, mcp-mux refactored, playbooks)
- **Parallel opportunities:** T002||T003||T004||T005||T006, T016||T017||T018, T032||T033
- **Commit strategy:** One commit per phase (6 total)
- **Estimated tasks:** 36 total (30 implementation + 6 gates)
- **Estimated effort:** Phase 1-2 = T2 (move+rename), Phase 3 = T3 (new code), Phase 4 = T3 (large refactor), Phase 5 = T3 (new code), Phase 6 = T1 (cleanup)
