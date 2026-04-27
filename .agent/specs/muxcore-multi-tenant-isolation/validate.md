# Validate — muxcore Multi-Tenant FS Isolation

**Date:** 2026-04-27
**Method:** M1 structural parse + M3 cross-artifact map (spec ↔ plan ↔ tasks ↔ ADR).

## FR → Task Coverage Matrix

| FR | Description | Implementing Tasks | Test Coverage Tasks |
|----|-------------|---------------------|---------------------|
| FR-1 | Engine-name-scoped owner socket paths | T001, T002 | T001 (4-case unit), T002 (cleanStaleSockets test) |
| FR-2 | Empty engine name rejected | T003 | T003 (TestNewRejectsEmptyName) |
| FR-3 | `Persistent` propagates to OwnerEntry | T003, T004 | T003 (TestPersistentPropagatesToDaemonConfig), T004 (TestSessionHandlerOwnerInheritsPersistent), T005 (R2 reaper) |
| FR-4 | Subprocess Persistent path preserved | T004 (additive logic) | T004 (existing classify path untouched, asserted in test fixture) |
| FR-5 | `mux_list` reads from daemon | T010 | T010 (rewritten server_test.go), T012 (cross-engine integration) |
| FR-6 | `mux_stop`/`mux_restart` resolve via daemon | T011 | T011 (TestMuxRestartRefusesForeignID), T012 |
| FR-7 | `cleanStaleSockets` daemon-name-scoped | T002 | T002 (cross-prefix isolation test) |
| FR-8 | `HandleListOwners` RPC | T006 | T006 (TestHandleListOwners) |
| FR-9 | `muxcore/v0.22.0` breaking tag | T016 | T016 acceptance (tag pushed, verified) |
| FR-10 | `mcp-mux v0.22.0` lockstep adoption | T008, T016, T017 | T013 (Phase 2 gate green) |
| FR-11 | Cross-version coexistence | T002 (cleanStaleSockets liveness gate) | T002 test asserts foreign-prefix preservation |
| FR-12 | Internal call sites updated atomically | T001, T002, T008 | T013 gate (`grep '"mcp-mux-"'` zero outside allowlist) |

**Verdict:** every FR has at least one implementing task and at least one test coverage task. **PASS.**

## NFR → Verification Path

| NFR | Description | Verification | Task |
|-----|-------------|--------------|------|
| NFR-1 | mux_list latency < 50 ms p99 | Manual smoke during T013 gate; daemon RPC is in-process IPC, dominant cost is JSON marshal of ≤200 owners | T013 |
| NFR-2 | R1/R2/R3 regressions | T003 (R1), T005 (R2), T019 (R3 via mcp-launcher persist) | T003, T005, T019 |
| NFR-3 | Disjoint FS namespace per engine | T012 (cross-engine integration test asserts disjoint sets) | T012 |
| NFR-4 | No silent defaults — hard error on empty Name | T003 (TestNewRejectsEmptyName) | T003 |
| NFR-5 | AGENTS.md + README documentation | T014, T015 | T014, T015 |
| NFR-6 | Build cleanliness — no `"mcp-mux-"` outside allowlist | T013 acceptance grep | T013 |

**Verdict:** every NFR has at least one verification path. **PASS.**

## ADR → Spec Traceback

| ADR | Decision | Spec Location |
|-----|----------|--------------|
| ADR-010 | Breaking `serverid` signatures | FR-1, FR-9, FR-12 |
| ADR-011 | `daemon.Config.Name` additive | FR-1 (implementation mechanism), Edge Cases |
| ADR-012 | mux_list/stop/restart query daemon | FR-5, FR-6, FR-8 |
| ADR-013 | `OwnerEntry.Persistent` hydrated | FR-3, FR-4 |
| ADR-014 | One arc, two issues, single release | FR-9, FR-10, US3 |

**Verdict:** every architecture decision is reflected in at least one FR. No orphan ADRs. **PASS.**

## User Story → FR Mapping

| User Story | Linked FRs | Acceptance Tied To |
|-----------|------------|--------------------|
| US1 (operator mux_list) | FR-5, FR-6, FR-7, FR-8 | T010, T011, T012, T013, T019 |
| US2 (aimux Persistent) | FR-3, FR-4 | T003, T004, T005, T019 |
| US3 (atomic release) | FR-9, FR-10, NFR-5 | T014, T015, T016, T017 |
| US4 (cross-version coexistence) | FR-7, FR-11 | T002, T013 |

**Verdict:** every user story maps to at least 1 FR. No orphan stories. **PASS.**

## Phase Gate Soundness

| Gate | Conditions | Blocking? |
|------|-----------|-----------|
| T007 (Phase 1) | `go test ./muxcore/...` green; vet + lint clean; smoke binary | YES — Phase 2 cannot start without |
| T013 (Phase 2) | Full repo `go test ./...` green; manual mux_list smoke | YES — Phase 3 cannot start without |
| T020 (Arc) | Success Criteria checklist 6/6, CONTINUITY.md updated | YES — arc not complete until ticked |

**Verdict:** dependency graph (`T001→T002→T003→T004→T005`, `T006` parallel-after-T002, fan-in to `T007`) is acyclic. Each gate observes only direct upstream dependencies. **PASS.**

## Open Question Closure

| Question | Resolution Source | Spec Status |
|----------|-------------------|-------------|
| Q1 (empty Name policy) | clarifications/2026-04-27-auto.md → HARD-ERROR | FR-2 finalized |
| Q2 (cleanStaleSockets cross-prefix) | clarifications/2026-04-27-auto.md → liveness gate | FR-7 + Edge Cases |
| Q3 (mux_list daemon-down UX) | clarifications/2026-04-27-auto.md → empty + note | US1 AC#5 |

**Verdict:** all open questions have explicit resolutions. spec.md `Open Questions` section is empty post-clarify. **PASS.**

## Overall Validation

| Gate | Status |
|------|--------|
| FR coverage | PASS |
| NFR verification | PASS |
| ADR traceback | PASS |
| User story mapping | PASS |
| Phase gate soundness | PASS |
| Open question closure | PASS |

**Overall:** **PASS.** Pipeline ready for `/nvmd-implement muxcore-multi-tenant-isolation`.

## Anomalies / Risks Worth Flagging

- **T012 cross-engine integration test** assumes `internal/mcpserver` can be instantiated against an arbitrary daemon control socket. Currently `mcpserver` reaches into `s.socketDir()` indirectly. Phase 2 refactor (T010) must surface a config injection point — captured implicitly in T010's "calls control.Send(daemonCtl, ...)". Worth one explicit task acceptance criterion: `internal/mcpserver.Server` accepts daemon control path via constructor argument or env var override. **Add to T010 acceptance.**
- **Snapshot version stamp bump** (mentioned in plan.md Risk Analysis) is not its own task. Folded into T002 via "snapshot.go same call-site updates". If the schema needs an explicit version bump field (`SnapshotVersion = 2`), that is a 1-line edit; called out here as a sub-step of T002.
- **`upgrade --restart` operator-side stale owners** — existing 6 stale owners on workstation (CONTINUITY.md session 5 leftover) should be cleared during T019 manually if they survive the upgrade. Documented as part of T019's expected behaviour.

## Pipeline Forward

`/nvmd-implement muxcore-multi-tenant-isolation` — executes T001 onward, gated by T007/T013/T020.
