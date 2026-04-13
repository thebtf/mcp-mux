# Tasks: Graceful Daemon Restart

**Spec:** .agent/specs/graceful-restart/spec.md
**Plan:** .agent/specs/graceful-restart/plan.md
**Generated:** 2026-04-07

## Phase 1: Snapshot Types + Serialization

- [x] T001 [P] Define OwnerSnapshot and SessionSnapshot types in internal/mux/snapshot.go
  AC: types have all fields from plan.md data model · JSON tags on all fields · base64 string fields for cached responses (including cached_resource_templates) · go build passes

- [x] T002 Implement Owner.ExportSnapshot() in internal/mux/owner.go
  AC: returns OwnerSnapshot with all cached responses base64-encoded · thread-safe (RLock) · includes classification, cwd_set, persistent · swap body→return null ⇒ tests MUST fail

- [x] T003 [P] Define DaemonSnapshot type in internal/daemon/snapshot.go
  AC: has version (int), mux_version (string), timestamp (RFC3339), owners ([]OwnerSnapshot), sessions ([]SessionSnapshot) · JSON tags

- [x] T004 Implement daemon.SerializeSnapshot() in internal/daemon/snapshot.go
  AC: walks all owners calling ExportSnapshot() · collects session metadata · writes atomic JSON (temp file + rename) · snapshot path = os.TempDir()/mcp-muxd-snapshot.json · swap body→return null ⇒ tests MUST fail

- [x] T005 Implement daemon.DeserializeSnapshot() in internal/daemon/snapshot.go
  AC: reads JSON file · validates version field = 1 · rejects stale snapshots (> 5 min) · returns error on corrupt JSON · deletes snapshot file after successful load · swap body→return null ⇒ tests MUST fail

- [x] T006 Write tests for snapshot serialization in internal/daemon/snapshot_test.go
  AC: round-trip test (serialize → deserialize → compare) · corrupt JSON test → error · stale timestamp test → error + file deleted · empty owners test → valid snapshot · atomic write test (temp file exists during write, final path appears atomically) · 6+ test cases

- [x] G001 VERIFY Phase 1 (T001–T006) — BLOCKED until T001–T006 all [x]
  RUN: go test ./internal/mux/ ./internal/daemon/ -count 1. Call Skill("code-review", "lite") on snapshot.go files.
  CHECK: Confirm AC met for each task. Anti-stub check on ExportSnapshot and SerializeSnapshot.
  ENFORCE: Zero stubs. Zero TODOs. Every field influences output.
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** Snapshot types defined, serialization round-trip works, tests green.

## Phase 2: Cache-First Owner + Snapshot Loading

**Goal:** New daemon loads snapshot and serves cached responses instantly
**Independent Test:** Create snapshot JSON manually, start daemon, connect shim, verify instant init response

- [x] T007 [US1] Implement NewOwnerFromSnapshot() in internal/mux/owner.go
  AC: creates Owner with pre-populated initResp, toolList, promptList, resourceList · sets classification from snapshot · starts IPC listener · does NOT start upstream process · initReady channel closed immediately (cache already populated) · swap body→return null ⇒ tests MUST fail

- [x] T008 [US1] Implement background upstream spawn for snapshot owners in internal/mux/owner.go
  AC: after NewOwnerFromSnapshot, goroutine spawns upstream + runs proactive init · when fresh init response arrives, cache is updated · if upstream fails, owner continues serving stale cache · tools/call requests while upstream not ready → forwarded when upstream connects (queued in handleDownstreamMessage) · swap body→return null ⇒ tests MUST fail

- [x] T009 [US1] Implement daemon.loadSnapshot() in internal/daemon/daemon.go
  AC: called on daemon startup · checks SnapshotPath() for file · calls DeserializeSnapshot · for each owner: creates NewOwnerFromSnapshot with correct OwnerConfig · registers in d.owners map · logs "loaded N owners from snapshot" · swap body→return null ⇒ tests MUST fail

- [x] T010 [US2] Implement snapshot staleness + cleanup in internal/daemon/snapshot.go
  AC: DeserializeSnapshot rejects > 5 min old · deletes file on successful load · deletes stale file with warning log · ignores missing file (cold start) · swap body→return null ⇒ tests MUST fail

- [x] T011 [US1] Write tests for cache-first owner in internal/mux/owner_test.go or mux_test.go
  AC: test NewOwnerFromSnapshot creates owner with cached init · test session connects and gets instant cached replay · test upstream spawn in background updates cache · 3+ test cases

- [x] G002 VERIFY Phase 2 (T007–T011) — BLOCKED until T007–T011 all [x]
  RUN: go test ./... -count 1 -timeout 120s. Call Skill("code-review", "lite") on changed files.
  CHECK: Confirm AC met. Anti-stub check on NewOwnerFromSnapshot and loadSnapshot.
  ENFORCE: Zero stubs. Cache-first mode independently testable.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Daemon loads snapshot, owners serve cached responses instantly, upstream spawns in background.

## Phase 3: Control Command + Upgrade Integration

**Goal:** `mcp-mux upgrade --restart` triggers graceful restart with snapshot
**Independent Test:** Run mcp-mux upgrade --restart with live servers, verify all reconnect < 2s

- [x] T012 [US1] Add "graceful-restart" command to control protocol in internal/control/protocol.go and internal/control/server.go
  AC: new command recognized in server.go switch · calls handler.HandleGracefulRestart() · returns snapshot_path in response · swap body→return null ⇒ tests MUST fail

- [x] T013 [US1] Implement Daemon.HandleGracefulRestart() in internal/daemon/daemon.go
  AC: calls SerializeSnapshot() · on success: calls HandleShutdown(drainTimeoutMs) · pending requests get error responses during drain · returns snapshot path in response · on serialization error: returns error, does NOT shutdown · swap body→return null ⇒ tests MUST fail

- [x] T014 [US1] Modify runUpgrade in cmd/mcp-mux/main.go to send "graceful-restart"
  AC: when --restart flag: sends "graceful-restart" instead of "shutdown" · prints "Graceful restart: snapshot written, daemon stopping" · falls back to "shutdown" if "graceful-restart" not supported (backward compat) · swap body→return null ⇒ tests MUST fail

- [x] T015 [US3] Write integration test for full graceful restart cycle in internal/daemon/daemon_test.go
  AC: test creates daemon with mock owners → graceful-restart → new daemon loads snapshot → shim connects → instant cached init · verifies shared owners dedup correctly · 2+ test cases

- [x] G003 VERIFY Phase 3 (T012–T015) — BLOCKED until T012–T015 all [x]
  RUN: go test ./... -count 1 -timeout 120s. Call Skill("code-review", "lite") on all changed files.
  CHECK: Confirm AC met. Full cycle test passes. Anti-stub check.
  ENFORCE: Zero stubs. Upgrade --restart produces correct output.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Full graceful restart cycle works end-to-end.

## Phase 4: Edge Cases + Verification

**Goal:** Production readiness with all edge cases handled
**Independent Test:** Deploy with 15+ real servers, run upgrade --restart, verify via mux_list

- [x] T016 [US2] Handle version mismatch in snapshot loading in internal/daemon/snapshot.go
  AC: snapshot with version != 1 → rejected with warning · snapshot with different mux_version → loaded with warning (cache might be stale but still useful) · swap body→return null ⇒ tests MUST fail

- [x] T017 [US2] Handle spawn failure for snapshot owners in internal/daemon/daemon.go
  AC: if command from snapshot no longer exists → owner removed, log warning · if upstream crashes immediately → owner serves stale cache until shim reconnects to fresh spawn · swap body→return null ⇒ tests MUST fail

- [x] T018 Full test suite regression check
  AC: go test ./... -count 1 passes · all 10 packages green · no test regressions · go test -cover daemon and mux packages >= 70%

- [x] T019 Build, deploy, and smoke test with real servers
  AC: go build succeeds · mcp-mux upgrade --restart with 15 servers · all servers show connected in mux_list within 2s · snapshot file < 1 MB · snapshot file deleted after load

- [x] G004 VERIFY Phase 4 (T016–T019) — BLOCKED until T016–T019 all [x]
  RUN: go test ./... -count 1. Live smoke test with real servers.
  CHECK: All edge cases handled. Regression test clean. Smoke test passes.
  ENFORCE: Zero stubs. Zero deferred findings.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Graceful restart production-ready.

## Phase 5: Release

- [x] T020 Update CONTINUITY.md and TECHNICAL_DEBT.md
  AC: version updated · done items listed · architecture section updated

- [x] T021 Commit, push, tag release
  AC: git push passes · CI green · release with detailed annotations

## Dependencies

- Phase 1 (types) blocks Phase 2 (loading) — snapshot types needed for NewOwnerFromSnapshot
- Phase 2 (loading) blocks Phase 3 (wiring) — loadSnapshot needed before control command
- Phase 3 (wiring) blocks Phase 4 (edge cases) — full cycle needed for edge case testing
- T001 and T003 are parallelizable [P] (mux/snapshot.go and daemon/snapshot.go are separate files)
- T007 and T008 are sequential (NewOwnerFromSnapshot before background spawn)

## Execution Strategy

- **MVP scope:** Phase 1–3 (snapshot works, upgrade triggers it)
- **Parallel opportunities:** T001||T003 (separate packages), T011||T012 (test + protocol)
- **Commit strategy:** One commit per phase (types, loading, wiring, edge cases)
- **Estimated tasks:** 21 total (15 implementation + 4 gates + 2 release)
