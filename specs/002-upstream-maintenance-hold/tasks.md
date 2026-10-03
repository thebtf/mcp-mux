---
description: "Accepted ADR-015 implementation tasks for GitHub #135"
---

# Tasks: Upstream maintenance hold

**Input**: `specs/002-upstream-maintenance-hold/{spec.md,plan.md,research.md,data-model.md,contracts/maintenance.md,quickstart.md}`.

**Prerequisites**: Accepted root ADR-015, requirements/plan quality reviews, source base `3881f27b931f6b9d0467c0a15dd4e1824969e125`, and the exact candidate branch. All tasks are unimplemented. Core/adapter proof is not root acceptance.

**Tests**: Required by FR-016 and constitution. Use existing package conventions and `TestMaintenance` names for new behavior cases. Demonstrate the focused regression fails without its fix and passes with it. Keep permanent tests for observable safety/edges, not forwarding mocks, source text, or copied DTOs. Each owned focused-proven slice is committed before the next slice; do not run global gates while sibling changes are incomplete.

**Path conventions and ownership**: Paths are candidate-root-relative. Core owns `muxcore/control`, `muxcore/daemon`, `muxcore/owner`, `muxcore/upstream`, and `muxcore/engine`. Adapter owns `cmd/mcp-mux`, `internal/mcpserver`, consumer docs, fixture, and new live-smoke runner. Root owns integration, evidence, review, and release. Do not write linked-worktree `.agent`, mutate other specs/config, or perform foreign-repo implementation. Public exported changes require source reference analysis before editing.

## Phase 1: Setup, exact failing behavior

No project initialization or new dependencies.

- [ ] T001 Core add the failing-before connected-session respawn-suppression scenario in `muxcore/daemon/lifecycle_test.go` using existing process fixtures, report actual original-ID/new-generation focused evidence to root, and preserve the legacy parity baseline without treating the historic timing as a current measurement.

## Phase 2: Foundational public contract and identity/storage prerequisites

Adapters cannot begin API use before the T004 contract commit. The ledger/start machinery is one authority, not an independent shipped feature.

- [ ] T002 Core add focused `TestMaintenance` control cases in `muxcore/control/control_test.go` for capability absence, invalid durations/results, typed error wrapping/classification, safe fields, exact lease IDs, and terminal unsupported behavior using the contract in `specs/002-upstream-maintenance-hold/contracts/maintenance.md`.
- [ ] T003 Core implement the exact additive Request/Response/OwnerInfo fields, result/state/error/sentinels, `Response.Err`, `SendMaintenance`, optional `MaintenanceHandler`, `ShutdownWithErrorHandler`, and `OwnerRestartHandler` contracts in `muxcore/control/protocol.go`, `muxcore/control/maintenance.go`, `muxcore/control/server.go`, and `muxcore/control/client.go`, retaining existing interfaces and omitted-field wire behavior.
- [ ] T004 Core run the focused `TestMaintenance` control cases and commit the working neutral contract slice in `muxcore/control/`; provide that exact SHA as the prerequisite to the adapter maker, without claiming daemon hold behavior exists yet.
- [ ] T005 Core add focused exact-context and durable-mutation failure cases in `muxcore/daemon/maintenance_test.go` and `muxcore/daemon/maintenance_store_test.go` for strict normalized environment/CWD/era/endpoint identity, argv boundaries, full digests, nonce/retry exclusion, finite shared contexts, corrupt schema, and mandatory paired-authority validation. Cover PREPARE/PUBLISH/FINALIZE errors before and after publication: errors retain conservative live fencing, while recovery either refuses pending/invalid authority or verifies a matching COMMITTED certificate of earlier acknowledged durable publication. Prove acknowledged release cannot resurrect its predecessor; do not assume unchanged storage on every error.
- [ ] T006 Core implement immutable finite-context identities before placeholder publication and one restrictive schema-2 logical ledger with mandatory `ledger.json` and `transaction.json` in `muxcore/daemon/maintenance.go`, `muxcore/daemon/maintenance_store.go`, `muxcore/daemon/maintenance_store_windows.go`, and `muxcore/daemon/maintenance_store_unix.go`, reusing `muxcore/internal/envidentity/identity.go` policy and platform replace-file patterns without importing package main. PREPARE retains predecessor leases and target digest; PUBLISH follows acknowledged durable preparation; FINALIZE certifies only acknowledged durable target publication. Aggregate lookup fails closed unless the complete committed pair validates. Individual member replacement is not atomic two-file replacement; finalize errors remain typed failures even when recovery verifies the completed certificate. Run focused storage/identity cases and commit the proven slice.

## Phase 3: User Story 1, hold and retire exact trees (Priority: P1)

**Goal**: Successful hold implies durable suppression and actual full-tree death, so the installer can replace the executable.

**Independent Test**: An exact selected upstream with connected hosts and a descendant reaches HELD only after all scoped trees die, with no start/install/placeholder gap. Actual file replacement is completed by root T029 after the whole vertical slice is integrated.

- [ ] T007 [US1] Core add RED start-through-install/failed-start, placeholder, stale-generation, actual drain, blocked-tree-death, and expired-acquisition-window cases in `muxcore/daemon/maintenance_test.go`, `muxcore/owner/materialization_controller_test.go`, and `muxcore/upstream/start_transaction_test.go`; use paused deterministic transactions rather than sleep-only race assertions.
- [ ] T008 [US1] Core implement the daemon-owned gate and complete context admission across fresh/template/persistent/restored owner starts and every materialization retry in `muxcore/daemon/daemon.go`, `muxcore/owner/materialization.go`, and `muxcore/owner/owner.go`; acquire before owner locks and release only after every nonnil process, including failed starts, is installed into authority; recheck after placeholder/classification waits.
- [ ] T009 [US1] Core implement fence-before-ingress/cache admission, real bounded already-forwarded request draining, and exact-current-generation retirement in `muxcore/daemon/maintenance.go`, `muxcore/daemon/owner_lifecycle.go`, `muxcore/owner/owner.go`, and `muxcore/upstream/process.go`; distinguish dead trees from committed handoff, retain blocked authority, and never reset the accepted drain deadline.
- [ ] T010 [US1] Core wire `HandleMaintenance` acquisition, durable usable HELD publication, redacted active-lease status/list, and refusal/error results in `muxcore/daemon/maintenance.go`, `muxcore/daemon/daemon.go`, and `muxcore/control/server.go`; no successful grant on failed persistence, unsettled placeholder, incomplete retirement, or elapsed TTL.
- [ ] T011 [P] [US1] Adapter after T004 implement exact-target CLI hold with flags after the identifier and MCP `mux_hold` through the shared `SendMaintenance` API in `cmd/mcp-mux/main.go` and `internal/mcpserver/server.go`, with invalid/unsupported/typed-error boundary cases in `cmd/mcp-mux/maintenance_test.go` and `internal/mcpserver/server_test.go`; never add substring or direct-socket/exec fallback.
- [ ] T012 [P] [US1] Adapter extend `scripts/lifecycle-smoke-upstream/main.go` with a link-time version marker, opt-in shared mode preserving the existing isolated default, bounded in-flight operation, and capture sufficient to prove held/no-replay behavior; reuse `testdata/mock_modern_server.go` unchanged where possible.
- [ ] T013 [US1] Core run and commit the focused acquisition/start/drain/tree-death cases in `muxcore/daemon/maintenance_test.go`, `muxcore/owner/materialization_controller_test.go`, and `muxcore/upstream/start_transaction_test.go`, recording actual deadline, blocked retirement, and zero survivors before declaring this slice proven.

## Phase 4: User Story 2, aware-host held errors without replay (Priority: P1)

**Goal**: Aware hosts stay connected and receive immediate original-ID maintenance errors; no held frame can cross reconnect into a new upstream.

**Independent Test**: Send numeric/string IDs and notifications while owner ingress or shim reconnect is held, race a successful reconnect against queued frames, and prove exactly-one terminal disposition and no upstream/cache/replay delivery.

- [ ] T014 [US2] Core add RED aware-host cases in `muxcore/owner/resilient_client_test.go` and `muxcore/engine/engine_test.go` for numeric/string IDs, buffered-before-confirmation frames, ingress/connected transition race, notifications/responses, queue capacity/backpressure, terminated inflight work, typed refresh/spawn errors, and native modern no-bootstrap/no-replay.
- [ ] T015 [US2] Core implement immediate typed maintenance mode and serialized ingress/maintenance-to-connected disposition in `muxcore/owner/resilient_client.go` and owner ingress in `muxcore/owner/owner.go`, emitting product -32005 by original raw ID before cache/queue/start/forwarding and never silently dropping a promised request or replaying ended work.
- [ ] T016 [US2] Core preserve typed maintenance errors through both refresh and fallback-spawn wiring in `muxcore/engine/engine.go`, retain modern exact-era fresh-admission/refusal policy, run the focused aware-host cases, and commit the proven core transport slice.

## Phase 5: User Story 4, safe recovery and compatibility (Priority: P1)

**Goal**: Retry/context changes cannot bypass the hold; controlled lifecycle refuses terminally; aware unplanned recovery loads authority before activation.

**Independent Test**: Exercise startup/SkipSnapshot ordering, invalid/incomplete authority, context variants, stale callbacks, lifecycle fallback, unsupported endpoints, and coordinated-versus-unsupported standalone launch.

- [ ] T017 [US4] Core add RED startup/SkipSnapshot, persisted HELD versus incomplete/corrupt ledger, restore/template/token suppression, controlled restart/handoff/shutdown/idle-exit refusal, and update-helper no-fallback cases in `muxcore/daemon/maintenance_test.go`, `muxcore/daemon/snapshot_test.go`, `muxcore/daemon/handoff_test.go`, and `muxcore/engine/update_test.go`.
- [ ] T018 [US4] Core load/validate maintenance authority before control listener or any persistent/snapshot/handoff activation in `muxcore/daemon/daemon.go`, `muxcore/daemon/snapshot.go`, and `muxcore/daemon/handoff.go`; retain incomplete state as blocked and keep fenced entries/templates/tokens unpublished independently of SkipSnapshot.
- [ ] T019 [US4] Core serialize lifecycle refusal against fence commitment in `muxcore/daemon/daemon.go`, `muxcore/daemon/reaper.go`, `muxcore/daemon/handoff.go`, and `muxcore/engine/update.go`; implement typed optional shutdown and exact-context `restart_owner`, inhibit empty-daemon exit, and make update-helper maintenance refusal terminal without shutdown fallback, force bypass, or successor start.
- [ ] T020 [US4] Adapter add typed spawn/refresh and terminal launcher/update refusal plus managed-only standalone/restart behavior in `cmd/mcp-mux/daemon.go`, `cmd/mcp-mux/main.go`, `cmd/mcp-mux/launcher.go`, and `internal/mcpserver/server.go`; use daemon-selected `restart_owner` context/era instead of OwnerInfo plus ambient environment, and prove no stop/exec fallback in `cmd/mcp-mux/maintenance_test.go` and `internal/mcpserver/server_test.go`.
- [ ] T021 [US4] Core prove modern nonce and isolated retry matching, finite shared CWD sets, strict credential/config/era/namespace partition, late callback/lease exclusion, safe readbacks, and coordinated or explicitly unsupported direct starts in `muxcore/daemon/maintenance_test.go`, `muxcore/owner/materialization_controller_test.go`, and `muxcore/engine/engine_test.go`; run the focused recovery/lifecycle cases and commit the proven slice without changing R1 modern snapshot payloads.

## Phase 6: User Story 3, exact resume/renew/expiry (Priority: P2)

**Goal**: The exact lease resumes or renews deterministically; only proven HELD may expire into admission.

**Independent Test**: With controlled clocks/deadlines, exercise renewal acceptance, stale/conflicting IDs, resume versus expiry, failed release persistence, blocked retirement, and fresh-demand single-generation behavior.

- [ ] T022 [US3] Core add RED exact-lease resume/renew/default/boundary/expiry and durable-release-failure cases in `muxcore/daemon/maintenance_test.go`, including expired acquisition, zero-force versus positive single drain deadline, blocked retirement despite TTL, renewal versus resume/expiry races, and no stale-lease effects.
- [ ] T023 [US3] Core implement serialized resume, renewal from acceptance time, safe HELD expiry, and persistence-before-opening admission in `muxcore/daemon/maintenance.go` and `muxcore/daemon/maintenance_store.go`; reject incomplete/expired/stale leases as specified, run focused cases, and commit the proven lease slice.
- [ ] T024 [US3] Adapter implement CLI resume/renew and MCP `mux_resume`/`mux_renew` via the same neutral API in `cmd/mcp-mux/main.go` and `internal/mcpserver/server.go`, with duration/identity/blocked/conflict/unsupported errors and safe JSON/tool readback proved in `cmd/mcp-mux/maintenance_test.go` and `internal/mcpserver/server_test.go`; commit the proven adapter command slice.

## Final phase: Integrated proof, consumer docs, release

Root alone runs global gates after both maker surfaces land. No generic refactor, telemetry system, unrelated future feature, or active-lease transfer task is added.

- [ ] T025 Adapter update `README.md`, `README.ru.md`, `AGENTS.md`, `muxcore/README.md`, and `docs/mux-protocol.md` with exact hold/resume/renew usage, public optional API/error contract, finite context scope, request/drain/TTL outcomes, terminal lifecycle refusal, managed restart, safe rollback, modern native policy, and explicit old-daemon/old-shim/standalone/old-binary limitations.
- [ ] T026 Adapter implement cross-platform `scripts/smoke-upstream-maintenance.ps1` with exactly the quickstart parameters and real-process scenarios in `specs/002-upstream-maintenance-hold/quickstart.md`, reusing T012's fixture; require two unchanged legacy host pipes, descendant death, actual executable overwrite, original IDs/no replay, resume and expiry new-version demand, aware recovery, one native modern case, and safe redaction evidence.
- [ ] T027 Adapter update `docs/PRODUCTION-TESTING-PLAYBOOK.md` and `docs/RELEASE-PROTOCOL.md` to own the runnable maintenance proof and existing required Scenario 5b/8 gates; do not silently auto-enroll a long smoke into unrelated test runs or replace existing native/R1 parity proof.
- [ ] T028 Adapter run only focused `TestMaintenance` command/MCP tests and validate fixture compilation and runner PowerShell parsing in `cmd/mcp-mux/maintenance_test.go`, `internal/mcpserver/server_test.go`, `scripts/lifecycle-smoke-upstream/main.go`, and `scripts/smoke-upstream-maintenance.ps1`, then commit the proven owned docs/fixture/runner slice; actual integrated Windows/Unix live acceptance remains root-owned.
- [ ] T029 Root integrate the exact core/adapter commits, run all quickstart focused maintenance cases plus actual `scripts/smoke-upstream-maintenance.ps1` executable-replacement proof on Windows and Unix and R1 parity runners, and record exact SHA/binary hashes, denominators, commands, original-pipe/tree/version evidence, and findings in `specs/002-upstream-maintenance-hold/release-evidence.md`.
- [ ] T030 Root on the admitted integrated head run root/muxcore tests and vet, applicable focused race/tree gates, `tests/critical/run-all.ps1 -TimeoutSeconds 120`, native-update Scenario 5b, and applicable Scenario 8 from `docs/PRODUCTION-TESTING-PLAYBOOK.md`; retain results in `specs/002-upstream-maintenance-hold/release-evidence.md`, with no zero-case or one-platform-only success claims.
- [ ] T031 Root review the exact candidate and frozen evidence against `specs/002-upstream-maintenance-hold/{spec.md,contracts/maintenance.md,quickstart.md}`, disposition any remaining finding, and verify coherent optional public APIs and migration/rollback docs before implementation acceptance.
- [ ] T032 Root select the release versions and update `CHANGELOG.md`, `RELEASE_NOTES.md`, and consumer-target docs, then execute the authorized exact-SHA merge/tag-last/module-resolution/binary delivery flow in `docs/RELEASE-PROTOCOL.md`; source integration alone is not delivery.
- [ ] T033 Root complete authorized current-version aimux/engram/other consumer handoffs and readback, fresh-clone/module/binary canary and fresh-session delivered hold/replace/resume proof, recording `PROJECT_RELEASE_PROTOCOL_PASS` and `CONSUMER_HANDOFF_PASS` or the exact incomplete boundary in `specs/002-upstream-maintenance-hold/release-evidence.md`.

## Dependencies & Execution Order

- T001 -> T002 -> T003 -> T004. No adapter API use before T004's exact contract commit.
- Core: T005/T006 -> T007-T010/T013 -> T014-T016 -> T017-T019/T021 -> T022/T023. Core sequence is serialized because daemon/owner/control files overlap.
- Adapter: after T004, T011 and T012 own disjoint files and may run beside the core chain. T020 follows T011 and uses committed restart/error contract plus core T019 behavior. T024 follows T020. T025-T028 follow the resolved complete boundaries; fixture T012 precedes runner T026.
- Full vertical integration T029 waits for core T023 and adapter T028. T030 follows integrated smoke; T031 reviews exact evidence; T032/T033 follow root acceptance and effect-specific release authority.
- Stories are independently verifiable outcomes, not independently releasable safety fragments. US1 requires US2/US4 protection and US3 release semantics before shipping. The first shippable increment is all four stories; no unsafe hold-only MVP is authorized.

## Parallel examples

- US1: after T004, adapter T011 CLI/MCP exact-target work and T012 fixture extension can run independently while core owns gate/ledger/drain work. T011 and T012 do not touch muxcore.
- US2: adapter may prepare T025 transport-limit docs while core T014-T016 completes; final docs require core readback. No second writer touches resilient-client/engine files.
- US4: adapter T020 cmd/internal boundary work can proceed after its prerequisites alongside core's disjoint daemon/engine recovery changes; integration still waits for both.
- US3: adapter T024 uses the committed neutral contract while core T022/T023 owns lease semantics; the command smoke waits for real core behavior.

## Traceability

| Requirements / accepted correction | Tasks | Root oracle |
| --- | --- | --- |
| FR-001..004; F1/F2/F3 scope/start/drain/tree death | T001-T013 | T029 SC-001 and focused races. |
| FR-005/006/014; F5 ingress/native modern | T014-T016, T020, T026 | T029 SC-002/006. |
| FR-007..009 exact lease/TTL | T022-T024 | T029 SC-003/004. |
| FR-010..013; F4 durable startup and terminal refusal | T005/T006, T017-T021 | T029 SC-005/006. |
| FR-012/015 safe readbacks and compatibility | T010/T020/T021/T025 | T029 SC-007. |
| FR-016 regression/live/release evidence | T026-T033 | All quickstart criteria and release gates. |

## Implementation strategy

Commit the neutral public contract first, then develop core and adapters only on their named surfaces. Preserve exact-generation and tree authority at every stage. Focused proof precedes each owned atomic commit. Integrate and prove the complete hold/replace/resume journey before root accepts it, then deliver through the existing release flow. No maker changes Product/Flow/tracker/lane/seat state.
