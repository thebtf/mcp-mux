# Tasks: Session-Aware Handler API

**Spec:** .agent/specs/session-handler/spec.md
**Plan:** .agent/specs/session-handler/plan.md
**Generated:** 2026-04-14

## Phase 1: Types + ProjectContext ID

- [ ] T001 Define ProjectContext struct and SessionHandler interface in muxcore/handler.go
  AC: ProjectContext has ID (string), Cwd (string), Env (map[string]string) · SessionHandler has HandleRequest(ctx, ProjectContext, []byte) ([]byte, error) · compiles with `go build ./...` · swap body→return null ⇒ tests MUST fail

- [ ] T002 [P] Define optional interfaces in muxcore/handler.go: ProjectLifecycle, NotificationHandler, Notifier, NotifierAware
  AC: ProjectLifecycle has OnProjectConnect(ProjectContext) + OnProjectDisconnect(string) · NotificationHandler has HandleNotification(ctx, ProjectContext, []byte) · Notifier has Notify(string, []byte) error + Broadcast([]byte) · NotifierAware has SetNotifier(Notifier) · compiles · swap body→return null ⇒ tests MUST fail

- [ ] T003 Add WorktreeRoot(cwd string) string to muxcore/serverid/serverid.go
  AC: walks up from cwd to find .git · .git dir → returns that directory · .git file (linked worktree) → returns that directory (NOT resolved to main repo) · no .git → returns canonical cwd · swap body→return null ⇒ tests MUST fail

- [ ] T004 [P] Add ProjectContextID(cwd string) string to muxcore/handler.go using WorktreeRoot + sha256
  AC: same worktree root → same ID · different worktree → different ID · subdirectory of worktree → same ID as root · no .git → ID from canonical cwd · ID is hex string len 16 · swap body→return null ⇒ tests MUST fail

- [ ] T005 [P] Tests for WorktreeRoot and ProjectContextID in muxcore/serverid/serverid_test.go and muxcore/handler_test.go
  AC: TestWorktreeRoot: main checkout, linked worktree (mock .git file), no git, subdirectory · TestProjectContextID: determinism, different cwds, same worktree subdirs · 8+ test cases total · swap body→return null ⇒ tests MUST fail

- [ ] G001 VERIFY Phase 1 (T001–T005) — BLOCKED until T001–T005 all [x]
  RUN: `go test ./muxcore/... ./muxcore/serverid/...`. Code review on handler.go and serverid.go changes.
  CHECK: All types exported. ID generation deterministic. WorktreeRoot does NOT resolve linked worktrees.
  ENFORCE: Zero stubs. Zero TODOs. Every function has tests.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Types compile, ID generation tested. All subsequent phases depend on these types.

## Phase 2: Owner Dispatch — US1 (Session-grouped job tracking, P1)

**Goal:** SessionHandler receives HandleRequest with ProjectContext per request.
**Independent Test:** Two mock sessions with different CWDs → handler receives different ProjectContext.ID values. Concurrent requests don't block each other.

- [ ] T006 [US1] Add sessionHandler field to Owner struct and OwnerConfig in muxcore/owner/owner.go
  AC: OwnerConfig.SessionHandler field added · Owner stores it at construction (NewOwner + NewOwnerFromSnapshot) · nil = legacy path · compiles · swap body→return null ⇒ tests MUST fail

- [ ] T007 [US1] Implement dispatchToSessionHandler in muxcore/owner/owner.go
  AC: builds ProjectContext from session (ID via ProjectContextID(s.Cwd), Cwd, Env) · calls handler.HandleRequest in goroutine · passes original request bytes (no ID remap) · writes response to session · increments/decrements pendingRequests · swap body→return null ⇒ tests MUST fail

- [ ] T008 [US1] Add ctx deadline from toolTimeout in dispatchToSessionHandler
  AC: if toolTimeoutNs > 0 → ctx gets deadline · if handler exceeds deadline → JSON-RPC timeout error returned to session · ctx cancelled when session disconnects (s.Done()) · swap body→return null ⇒ tests MUST fail

- [ ] T009 [US1] Add panic recovery in dispatchToSessionHandler goroutine
  AC: handler panic → recovered → JSON-RPC internal error returned to session · daemon does not crash · panic logged with stack · swap body→return null ⇒ tests MUST fail

- [ ] T010 [US1] Wire dispatch in handleSessionRequest: if sessionHandler != nil → dispatchToSessionHandler
  AC: request branch checks sessionHandler before pipe path · notifications still go to pipe/NotificationHandler (not HandleRequest) · cached responses still replayed (initialize after first call, tools/list etc) · first initialize goes to HandleRequest, Owner caches response · swap body→return null ⇒ tests MUST fail

- [ ] T011 [US1] Tests for dispatch path in muxcore/owner/owner_test.go or new dispatch_test.go
  AC: TestDispatchToSessionHandler_BasicEcho · TestDispatchToSessionHandler_ConcurrentSessions (2 sessions, different CWDs, concurrent requests) · TestDispatchToSessionHandler_Timeout (handler blocks, deadline fires) · TestDispatchToSessionHandler_PanicRecovery · TestDispatchToSessionHandler_CacheReplay (second session gets cached init) · 5+ tests · swap body→return null ⇒ tests MUST fail

- [ ] G002 VERIFY Phase 2 (T006–T011) — BLOCKED until T006–T011 all [x]
  RUN: `go test -count=1 ./muxcore/owner/...`. Code review on owner.go dispatch changes.
  CHECK: Concurrent requests work. Timeout fires correctly. Panic doesn't crash daemon. Cache works.
  ENFORCE: Zero stubs. Zero TODOs. Independent test passes.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** SessionHandler dispatch works end-to-end. Handler receives ProjectContext per request.

## Phase 3: Lifecycle + Notifications — US2 + US3 (P1 + P2)

**Goal:** OnProjectConnect/Disconnect hooks, NotificationHandler, Notifier for targeted delivery.
**Independent Test:** Handler logs connect/disconnect events in order. Handler sends notification to one session, only that session receives it.

- [ ] T012 [US2] Implement OnProjectConnect call in Owner session accept path
  AC: after IPC handshake in session accept loop → if handler.(ProjectLifecycle) → call OnProjectConnect(ProjectContext) · called once per session · swap body→return null ⇒ tests MUST fail

- [ ] T013 [US2] Implement OnProjectDisconnect call in Owner removeSession path
  AC: in removeSession (existing cleanup) → if handler.(ProjectLifecycle) → call OnProjectDisconnect(projectID) · called with same ID as OnProjectConnect · called exactly once per session · swap body→return null ⇒ tests MUST fail

- [ ] T014 [US3] Implement ownerNotifier struct in muxcore/owner/owner.go
  AC: implements Notifier interface · Notify(projectID, data): lookup session by project ID → write to IPC, return error if not found · Broadcast(data): write to all sessions · swap body→return null ⇒ tests MUST fail

- [ ] T015 [US3] Wire SetNotifier in Owner construction
  AC: in NewOwner/NewOwnerFromSnapshot: if handler.(NotifierAware) → call SetNotifier(ownerNotifier) · called once at startup · swap body→return null ⇒ tests MUST fail

- [ ] T016 [US2] Implement NotificationHandler dispatch in Owner
  AC: in handleSessionRequest notification branch: if handler.(NotificationHandler) → call HandleNotification(ctx, ProjectContext, notification) · handlers that don't implement it → Owner handles cancelled internally, drops others · swap body→return null ⇒ tests MUST fail

- [ ] T017 [US2][US3] Tests for lifecycle and notifications
  AC: TestOnProjectConnect_CalledOnSessionJoin · TestOnProjectDisconnect_CalledOnSessionLeave · TestOnProjectDisconnect_SameIDAConnect · TestNotifier_TargetedDelivery (notify session A, session B doesn't receive) · TestNotifier_InvalidProjectID_ReturnsError · TestNotifier_Broadcast · TestNotificationHandler_Cancelled · 7+ tests · swap body→return null ⇒ tests MUST fail

- [ ] G003 VERIFY Phase 3 (T012–T017) — BLOCKED until T012–T017 all [x]
  RUN: `go test -count=1 ./muxcore/owner/...`. Code review on lifecycle and notification changes.
  CHECK: Connect before first request. Disconnect after last. Notify to disconnected → error. Concurrent safe.
  ENFORCE: Zero stubs. Zero TODOs. Independent test passes.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Full lifecycle API working. Handler can track sessions, send targeted notifications.

## Phase 4: Engine + Daemon Wiring — US4 (Legacy unchanged, P1)

**Goal:** engine.Config accepts SessionHandler. Legacy Handler path unbroken.
**Independent Test:** engine.Config{Handler: legacyFunc} works. engine.Config{SessionHandler: x} works. Both set → SessionHandler wins.

- [ ] T018 [US4] Add SessionHandler field to engine.Config in muxcore/engine/engine.go
  AC: field added · passed to daemon.Config · if both Handler and SessionHandler set → log warning, SessionHandler wins · swap body→return null ⇒ tests MUST fail

- [ ] T019 [US4] Add SessionHandler field to daemon.Config and wire to OwnerConfig in muxcore/daemon/daemon.go
  AC: daemon.Config.SessionHandler stored · passed to OwnerConfig in Spawn() · swap body→return null ⇒ tests MUST fail

- [ ] T020 [US4] Backward compatibility regression tests
  AC: TestEngineConfig_LegacyHandler (Handler only → pipe path, _meta injection works) · TestEngineConfig_SessionHandler (SessionHandler only → structured path) · TestEngineConfig_BothSet (SessionHandler wins, Handler ignored, warning logged) · TestEngineConfig_NeitherSet (error) · 4 tests · swap body→return null ⇒ tests MUST fail

- [ ] G004 VERIFY Phase 4 (T018–T020) — BLOCKED until T018–T020 all [x]
  RUN: `go test -count=1 ./muxcore/...`. Full test suite — no regressions.
  CHECK: Legacy path unchanged. SessionHandler path works through engine.
  ENFORCE: Zero stubs. Zero breaking changes to existing API.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Engine consumers can use SessionHandler. Existing consumers unaffected.

## Phase 5: Upgrade Mechanism — FR-9 (issue #45)

**Goal:** Export upgrade/restart mechanism as library API.
**Independent Test:** upgrade.Upgrade renames binary atomically. Cleanup removes stale files.

- [ ] T021 Extract upgrade logic from cmd/mcp-mux/main.go into muxcore/upgrade/upgrade.go
  AC: Upgrade(currentExe, newExe string, restart bool) error exported · atomic rename (rename current → .old.{pid}, rename new → current) · if restart: serialize snapshot, signal daemon, wait for exit, start new · swap body→return null ⇒ tests MUST fail

- [ ] T022 [P] Platform-specific files: muxcore/upgrade/upgrade_windows.go and upgrade_unix.go
  AC: Windows: rename semantics handle locked exe (rename-over works) · Unix: standard rename + SIGTERM · both compile on respective platforms · swap body→return null ⇒ tests MUST fail

- [ ] T023 [P] Refactor cmd/mcp-mux/main.go runUpgrade() to call upgrade.Upgrade()
  AC: runUpgrade() delegates to upgrade.Upgrade() · all existing behavior preserved · stale binary cleanup preserved · swap body→return null ⇒ tests MUST fail

- [ ] T024 Tests for upgrade in muxcore/upgrade/upgrade_test.go
  AC: TestUpgrade_AtomicRename (temp files, verify swap) · TestUpgrade_CleanupStaleFiles · TestUpgrade_NewBinaryNotFound_Error · 3+ tests · swap body→return null ⇒ tests MUST fail

- [ ] G005 VERIFY Phase 5 (T021–T024) — BLOCKED until T021–T024 all [x]
  RUN: `go test -count=1 ./muxcore/upgrade/...` + `go build ./cmd/mcp-mux/`.
  CHECK: Existing mcp-mux upgrade --restart still works. Library API callable from external consumers.
  ENFORCE: Zero stubs. No behavior change in cmd/mcp-mux.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Upgrade mechanism exported. aimux can call upgrade.Upgrade().

## Phase 6: Polish

- [ ] T025 Benchmark SessionHandler dispatch vs pipe path in muxcore/owner/bench_test.go
  AC: BenchmarkDispatchPipe (legacy Handler) · BenchmarkDispatchSessionHandler (new path) · SessionHandler path no slower than pipe · results documented in PR description · swap body→return null ⇒ tests MUST fail

- [ ] T026 Update AGENTS.md with SessionHandler API documentation
  AC: ProjectContext, SessionHandler, optional interfaces documented · migration guide from Handler to SessionHandler · consumer example code

- [ ] T027 Update .agent/CONTINUITY.md with new version state
  AC: muxcore consumer API section updated with SessionHandler example · version bumped

- [ ] G006 VERIFY Phase 6 (T025–T027) — BLOCKED until T025–T027 all [x]
  RUN: Read docs, verify accuracy against implemented code.
  CHECK: Examples compile. Migration guide is correct.
  RESOLVE: Fix inaccuracies.

---

**Checkpoint:** Documentation complete. Ready for release.

## Dependencies

```
Phase 1 (types) ← blocks everything
  ↓
Phase 2 (dispatch) ← blocks Phase 3, 4
  ↓
Phase 3 (lifecycle) ← independent after Phase 2
Phase 4 (wiring)    ← independent after Phase 2
Phase 5 (upgrade)   ← independent (no deps on Phase 2)
  ↓
Phase 6 (polish) ← after all phases
```

- G001 blocks T006–T011
- G002 blocks T012–T020
- G003, G004, G005 block T025–T026
- Phase 5 can run in parallel with Phases 3-4

## Execution Strategy

- **MVP scope:** Phase 1–2 (types + dispatch = US1 working)
- **Parallel opportunities:** T002||T004||T005, T012||T014, T021||T022||T023, Phase 3||Phase 4||Phase 5
- **Commit strategy:** One commit per completed task
- **Version:** v0.18.0 after Phase 4, v0.19.0 after Phase 5
