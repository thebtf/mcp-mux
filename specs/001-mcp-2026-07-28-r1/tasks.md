---
description: "Dependency-ordered implementation tasks for MCP 2026-07-28 R1 Native Isolation"
---

# Tasks: MCP 2026-07-28 R1 Native Isolation

**Input**: Design documents in `specs/001-mcp-2026-07-28-r1/`

**Prerequisites**: `plan.md`, `spec.md`, `research.md`, `data-model.md`, `contracts/`, `quickstart.md`, `consumer-handoff.md`, and the feature checklists.

**Tests**: Required. Every source-code task has a prior RED task. Each focused run precedes the atomic commit for its slice. Legacy parity remains an independent regression baseline. Test success never substitutes for built-deliverable customer proof.

**Scope lock**: R1 adds one explicit pinned modern route only. Do not add modern sharing/correlation, cache/template/replay, semantic translation, automatic discovery fallback, persisted modern snapshot/handoff, registry schema/capability, `mux_engines` or topology contract, lifecycle taxonomy, or counter model.

**Path convention**: All paths below are relative to the assigned candidate worktree root. Each commit task stages every source, test, fixture, support, and evidence path named for its slice, and no unrelated path.

## Phase 1: Setup (Shared Fixture and Acceptance Corpus)

**Purpose**: Establish the isolated same-era fixture and deterministic SC-001 corpus without changing the released legacy fixture.

- [x] T001 Add a RED fixture-contract test in `testdata/mock_modern_server_test.go` that requires `testdata/modern_opening_corpus.ndjson` to contain at least 100 valid newline-delimited modern opening frames spanning direct and host-sent `server/discover` openers, absent and present `clientInfo`, JSON property-order variants, and whitespace variants; require byte-exact capture plus native result behavior.
- [x] T002 Implement and extend `testdata/mock_modern_server.go` and the deterministic `testdata/modern_opening_corpus.ndjson` support corpus so the fixture captures direct/discovery opening bytes and provides controlled `input_required`, request-scoped log, contained-server-request, and loss modes.
- [x] T003 Run the focused fixture-contract test in `testdata/mock_modern_server_test.go` against the corpus and fixture.
- [x] T004 Stage and commit the proven fixture slice: `testdata/mock_modern_server.go`, `testdata/mock_modern_server_test.go`, and `testdata/modern_opening_corpus.ndjson`.

**Checkpoint**: The committed fixture and corpus are available before the foundational era contract begins. `testdata/mock_server.go` remains the released legacy-parity fixture.

---

## Phase 2: Foundational (Blocking Shared Era Contract)

**Purpose**: Define the narrow typed era and additive local-control vocabulary before any user-story implementation.

**Dependency**: Start only after T004 commits the fixture slice. The public zero value remains legacy.

- [x] T005 Add RED tests for zero-valued legacy policy, immutable supported modern era, raw opening-frame ownership, and local admission-error categories in `muxcore/era/era_test.go`.
- [x] T006 Implement `ProtocolEra`, `ProtocolPolicy`, `OpeningFrame`, and `AdmissionError` with a zero-valued legacy default and fixed `Modern20260728` value in `muxcore/era/era.go`.
- [x] T007 Add RED wire-compatibility tests for absent legacy control fields and explicit modern control-era echo serialization in `muxcore/control/control_test.go`.
- [x] T008 Add additive `protocol_era` request/response fields while retaining omitted-field legacy compatibility in `muxcore/control/protocol.go`.
- [x] T009 Run the focused shared-era contract tests in `muxcore/era/era_test.go` and `muxcore/control/control_test.go`.
- [x] T010 Stage and commit the proven shared-era contract slice: `muxcore/era/era.go`, `muxcore/era/era_test.go`, `muxcore/control/protocol.go`, and `muxcore/control/control_test.go`.

**Checkpoint**: Typed policy and control vocabulary exist, but no caller receives modern behavior until the story phases implement it.

---

## Phase 3: User Story 1 — Use a Known Modern Host Natively (Priority: P1)

**Goal**: A known MCP 2026-07-28 host explicitly selects one isolated same-era route, forwards its original opening bytes exactly once, and leaves released legacy behavior unchanged.

**Independent Test**: Exercise every frame in the committed deterministic corpus against `testdata/mock_modern_server.go`. Confirm that every accepted frame reaches one forced-isolated same-era upstream byte-identically, returns a native result, emits no mux-authored legacy traffic, and leaves the legacy baseline unchanged.

### RED Tests for User Story 1

- [x] T011 [P] [US1] Add RED CLI tests for additive `--mcp-protocol=2026-07-28` selection and the no-selector legacy default in `cmd/mcp-mux/modern_policy_test.go`.
- [x] T012 [P] [US1] Add RED shared-reader and engine tests that drive the committed SC-001 corpus through one-frame pre-election buffering, retain prefetched tail bytes, and assert direct/discovery byte preservation, exactly-one upstream delivery, and absent optional `clientInfo` acceptance in `muxcore/era/opening_test.go` and `muxcore/engine/engine_test.go`.
- [x] T013 [P] [US1] Add RED daemon and control-dispatch tests for an exact modern control echo, forced isolation, refusal to reuse a legacy owner under matching launch inputs, and omitted-field legacy response compatibility in `muxcore/daemon/daemon_test.go` and `muxcore/control/control_test.go`.
- [x] T014 [P] [US1] Add RED identity tests that prove legacy identity is byte-identical to the released baseline, create an actual modern endpoint/file component safely on Windows, and reject Unix `/` segments and NUL in the modern component in `muxcore/serverid/serverid_test.go`.
- [x] T015 [P] [US1] Add RED owner tests for native no-bootstrap/no-cache/no-template/no-replay readiness, opaque MRTR pass-through, contained upstream requests, and this logging contract: after request opt-in, a valid request-scoped standard log on the isolated route reaches its sole downstream once and is never synthesized or broadcast in `muxcore/owner/owner_test.go`.

### Implementation for User Story 1

- [x] T016 [US1] Implement the shared bounded persistent-reader opener in `muxcore/era/opening.go`; parse the additive explicit policy, buffer one raw opening frame before `runOwner` owner election, and carry the selected era through the private `spawnViaDaemon*` request/echo path while preserving the no-selector legacy invocation in `cmd/mcp-mux/main.go` and `cmd/mcp-mux/daemon.go`.
- [x] T017 [US1] Add public policy configuration and pre-election selection through `(*MuxEngine).Run` and `runClient`, prepending the one-time accepted raw frame to the persistent remainder reader for one downstream-to-upstream write in `muxcore/engine/engine.go`.
- [x] T018 [US1] Validate the era before side effects in `Spawn` and `spawnOnce`, force every modern owner isolated, reject cross-era reuse, and return the validated era through successful control dispatch in `muxcore/daemon/daemon.go` and `muxcore/control/server.go`.
- [x] T019 [US1] Domain-separate the modern physical identity while keeping every legacy identity byte-identical to the released baseline and making the modern component valid on Windows and Unix in `muxcore/serverid/serverid.go`.
- [x] T020 [US1] Store immutable era and native R1 owner policy through `OwnerConfig`, `NewOwner`, `handleDownstreamMessage`, and upstream routing without adding R2 route authorities in `muxcore/owner/owner.go`.
- [x] T021 [US1] Bypass `sendProactiveInit`, cache/template publication, legacy list-change synthesis, and legacy replay for a modern owner while writing the accepted opening bytes unchanged in `muxcore/owner/materialization.go`.
- [x] T022 [US1] Run the focused native-route and legacy-parity tests in `cmd/mcp-mux/modern_policy_test.go`, `muxcore/era/opening_test.go`, `muxcore/engine/engine_test.go`, `muxcore/daemon/daemon_test.go`, `muxcore/control/control_test.go`, `muxcore/serverid/serverid_test.go`, and `muxcore/owner/owner_test.go`.
- [x] T023 [US1] Stage and commit the proven native-route slice: `cmd/mcp-mux/main.go`, `cmd/mcp-mux/daemon.go`, `cmd/mcp-mux/modern_policy_test.go`, `muxcore/era/opening.go`, `muxcore/era/opening_test.go`, `muxcore/engine/engine.go`, `muxcore/engine/engine_test.go`, `muxcore/daemon/daemon.go`, `muxcore/daemon/daemon_test.go`, `muxcore/control/server.go`, `muxcore/control/control_test.go`, `muxcore/serverid/serverid.go`, `muxcore/serverid/serverid_test.go`, `muxcore/owner/owner.go`, `muxcore/owner/materialization.go`, and `muxcore/owner/owner_test.go`.

**Checkpoint**: The positive R1 route works for a known modern host. It is not safe until strict refusal completes.

---

## Phase 4: User Story 2 — Reject Unsafe Modern Admission (Priority: P1)

**Goal**: Invalid, ambiguous, unsupported, contradictory, or control-mismatched modern admission fails before an upstream starts, an owner attaches, or a legacy fallback is attempted.

**Independent Test**: Submit every malformed-metadata class and each bad control echo to a fresh fixture directory. Prove zero upstream start/attach and zero modern-to-legacy fallthrough, including the CLI JSON-RPC admission-error path.

### RED Tests for User Story 2

- [x] T024 [P] [US2] Add RED table tests for missing, null, non-object, malformed, unsupported, conflicting, and ambiguous modern metadata with exact `-32602` versus `-32022` classification in `muxcore/era/selector_test.go`.
- [x] T025 [P] [US2] Add RED daemon tests that invalid or unknown explicit control eras perform zero identity, spawn, reuse, or attachment side effects in `muxcore/daemon/daemon_test.go`.
- [x] T026 [P] [US2] Add RED shim tests for absent, old-daemon, unknown, and mismatched successful control echoes that fail before IPC attachment without falsely returning `-32022` in `muxcore/engine/engine_test.go`.
- [x] T027 [P] [US2] Add RED CLI tests that malformed metadata, unsupported versions, and local control mismatches produce the correct admission error with zero upstream start and zero legacy fallback in `cmd/mcp-mux/modern_admission_test.go`.

### Implementation for User Story 2

- [x] T028 [US2] Implement one-newline-frame strict modern selector parsing that preserves accepted raw bytes and returns local `AdmissionError` categories without a legacy fallback in `muxcore/era/selector.go`.
- [x] T029 [US2] After T028 has made local admission errors native at ingress, emit them before `spawnViaDaemonWithReason`, require an exact modern control echo before attachment, and keep later same-era upstream errors native in `muxcore/engine/engine.go`.
- [x] T030 [US2] Independently validate explicit control-era input before owner identity/election and reject malformed or cross-era requests before attachment in `muxcore/daemon/daemon.go`.
- [x] T031 [US2] After T029, map valid ingress admission errors to the CLI JSON-RPC path while preserving local control-failure distinctions and preventing speculative legacy retries in `cmd/mcp-mux/main.go`.
- [x] T032 [US2] Run the focused strict-admission tests in `muxcore/era/selector_test.go`, `muxcore/engine/engine_test.go`, `muxcore/daemon/daemon_test.go`, and `cmd/mcp-mux/modern_admission_test.go`.
- [x] T033 [US2] Stage and commit the proven strict-admission slice: `muxcore/era/era.go`, `muxcore/era/selector.go`, `muxcore/era/selector_test.go`, `muxcore/engine/engine.go`, `muxcore/engine/engine_test.go`, `muxcore/daemon/daemon_test.go`, `cmd/mcp-mux/main.go`, `cmd/mcp-mux/daemon.go`, and `cmd/mcp-mux/modern_admission_test.go`.

**Checkpoint**: Phases 1 through 4 are the smallest safe R1 demonstration. Release readiness still requires lifecycle quarantine, truthful readback, customer proof, and independent checking.

---

## Phase 5: User Story 3 — Keep Modern Work Safe During Lifecycle Events (Priority: P2)

**Goal**: Every named lifecycle re-entry retains the exact modern era only when safe. Otherwise it drains, cold-starts, or fails closed without replaying stale work or bypassing current process-generation authority.

**Independent Test**: Exercise each quarantine boundary through existing observable daemon/owner behavior. Do not write RED tests against proposed lifecycle APIs. Prove an allowed outcome and the absence of legacy hydration, replay, stale delivery, or competing replacement.

### Slice A: Snapshot and handoff exclusion only

- [x] T034 [P] [US3] Add RED snapshot export, restore, and staged-restore tests that observe an era-less current snapshot payload excluding a modern owner and never hydrating it as legacy, cache, token, or live route in `muxcore/daemon/snapshot_test.go`.
- [x] T035 [P] [US3] Add RED live-handoff tests that observe a modern owner excluded or refused before the current era-less transfer payload detaches it in `muxcore/daemon/handoff_test.go`.
- [x] T036 [US3] Exclude modern owners from current era-less snapshot export, restore, staged restore, and cache/token/live-route hydration in `muxcore/daemon/snapshot.go`. Do not add an era field, schema version, or retry rehydration here.
- [x] T037 [US3] Exclude or refuse modern owners from current handoff collection, transfer, and successor adoption while retaining existing handoff authority and fallback gates in `muxcore/daemon/handoff.go` and `muxcore/daemon/daemon.go`.
- [x] T038 [US3] Run the focused snapshot and handoff quarantine tests in `muxcore/daemon/snapshot_test.go` and `muxcore/daemon/handoff_test.go`.
- [x] T039 [US3] Stage and commit the proven snapshot/handoff exclusion slice: `muxcore/daemon/snapshot.go`, `muxcore/daemon/snapshot_test.go`, `muxcore/daemon/handoff.go`, `muxcore/daemon/handoff_test.go`, and `muxcore/daemon/daemon.go`.

### Slice B: Reaper and zero-session removal

- [x] T040 [P] [US3] Add RED reaper tests that use existing eligibility/activity gates to drain and remove a modern owner without synthesizing a legacy replacement in `muxcore/daemon/reaper_test.go`.
- [x] T041 [P] [US3] Add RED zero-session cleanup tests that preserve existing reservation and exact-entry CAS gates, then drain/remove instead of reconstructing legacy behavior in `muxcore/daemon/zero_session_cleanup_test.go`.
- [x] T042 [US3] Apply the reaper and zero-session quarantine decisions through existing activity, CAS, exact-current-entry, and finalization rails in `muxcore/daemon/reaper.go` and `muxcore/daemon/owner_lifecycle.go`.
- [x] T043 [US3] Run the focused removal tests in `muxcore/daemon/reaper_test.go` and `muxcore/daemon/zero_session_cleanup_test.go`.
- [x] T044 [US3] Stage and commit the proven removal slice: `muxcore/daemon/reaper.go`, `muxcore/daemon/owner_lifecycle.go`, `muxcore/daemon/reaper_test.go`, and `muxcore/daemon/zero_session_cleanup_test.go`.

### Slice C: Retry rehydration, respawn, and blocked finalization

- [ ] T045 [P] [US3] Add RED retry-rehydration tests against the existing retry selection that permit bounded restoration of retry-family identity, counter, and eligibility only with an explicit modern era, and otherwise cold-start or refuse without a legacy default in `muxcore/daemon/retry_counter_rehydrate_test.go`.
- [ ] T046 [P] [US3] Add RED owner-respawn tests that permit same-era modern replacement only after terminal work and reject legacy bootstrap, cache, and replay on the replacement in `muxcore/owner/respawn_test.go`.
- [ ] T047 [P] [US3] Add RED blocked-finalization tests that keep the exact current owner authoritative until `RetirementProven` or the existing explicit failure path resolves in `muxcore/owner/materialization_controller_test.go`.
- [ ] T048 [US3] Carry the immutable modern era through bounded retry-family rehydration and retry eligibility without a legacy default in `muxcore/daemon/daemon.go` and `muxcore/daemon/owner_lifecycle.go`.
- [ ] T049 [US3] Restrict owner-local replacement to the same explicit modern era and retain current process-generation and finalization fences before a successor becomes authoritative in `muxcore/owner/materialization.go`.
- [ ] T050 [US3] Run the focused retry, respawn, and blocked-finalization tests in `muxcore/daemon/retry_counter_rehydrate_test.go`, `muxcore/owner/respawn_test.go`, and `muxcore/owner/materialization_controller_test.go`.
- [ ] T051 [US3] Stage and commit the proven retry/respawn/finalization slice: `muxcore/daemon/daemon.go`, `muxcore/daemon/owner_lifecycle.go`, `muxcore/daemon/retry_counter_rehydrate_test.go`, `muxcore/owner/materialization.go`, `muxcore/owner/respawn_test.go`, and `muxcore/owner/materialization_controller_test.go`.

### Slice D: Loss and reconnect

- [ ] T052 [P] [US3] Add RED daemon/upstream-loss tests for the cleared-state branch: the daemon survives, the upstream generation dies, existing ephemeral request/progress/subscription state clears once, a stale generation cannot deliver, and no old route or replay remains in `muxcore/daemon/daemon_test.go` and `muxcore/owner/owner_test.go`.
- [ ] T053 [P] [US3] Add RED reconnect tests that admit only an exact modern era onto a fresh empty route after loss, require a host-issued new `subscriptions/listen`, and reject legacy replay/list-change/automatic re-listen in `muxcore/owner/resilient_client_reconnect_test.go`.
- [ ] T054 [US3] Clear existing ephemeral route state once after terminal modern loss and preserve exact-generation rejection of stale response, cancellation, progress, subscription, cache, or replacement authority in `muxcore/daemon/daemon.go` and `muxcore/owner/owner.go`.
- [ ] T055 [US3] Make modern reconnect require exact-era fresh admission or an explicit new-launch failure and bypass legacy replay, synthetic list change, and automatic re-listen behavior in `muxcore/owner/resilient_client.go`.
- [ ] T056 [US3] Run the focused loss and reconnect tests in `muxcore/daemon/daemon_test.go`, `muxcore/owner/owner_test.go`, and `muxcore/owner/resilient_client_reconnect_test.go`.
- [ ] T057 [US3] Stage and commit the proven loss/reconnect slice: `muxcore/daemon/daemon.go`, `muxcore/daemon/daemon_test.go`, `muxcore/owner/owner.go`, `muxcore/owner/owner_test.go`, `muxcore/owner/resilient_client.go`, and `muxcore/owner/resilient_client_reconnect_test.go`.

**Checkpoint**: Every R1 lifecycle boundary preserves exact era or takes an explicit safe quarantine outcome. No slice introduces R2 correlation or R3 persistence.

---

## Phase 6: User Story 4 — Inspect an Isolated Modern Owner Safely (Priority: P3)

**Goal**: Existing `OwnerInfo`-carrying status/list/CLI/`mux_list` projections agree on four minimal R1 policy facts, retain readiness meaning, expose no private material, and add no R3 observability contract.

**Dependency**: Start only after T057 commits the final US3 lifecycle slice. Do not claim US3 and US4 in parallel: both modify shared owner and daemon files.

**Independent Test**: Compare direct `OwnerInfo` projections for one active R1 owner, compare their readiness result with the released equivalent state, inspect normal and unavailable paths, and prove no registry descriptor/capability, `mux_engines`/topology contract, lifecycle taxonomy, or counter model appears.

### RED Tests for User Story 4

- [ ] T058 [P] [US4] Add RED direct-owner status tests for four R1 policy facts, readiness meaning identical to the released equivalent state, and redaction of payload, opaque state, token, credential, environment, and route identifiers in `muxcore/owner/owner_test.go`.
- [ ] T059 [P] [US4] Add RED daemon status and `list_owners` tests for exact `OwnerInfo` agreement, unavailable truthfulness, and preserved readiness meaning in `muxcore/daemon/status_contract_test.go` and `muxcore/daemon/list_owners_test.go`.
- [ ] T060 [P] [US4] Add RED CLI status tests for retaining four required facts and the existing readiness meaning without fabricating unavailable or legacy values in `cmd/mcp-mux/status_test.go`.
- [ ] T061 [P] [US4] Add RED `mux_list` projection tests that, for an active R1 owner, prove field agreement/redaction and prove R1 adds no registry descriptor/capability, `mux_engines` or topology output contract, lifecycle taxonomy, or counter model in `internal/mcpserver/server_test.go`.

### Implementation for User Story 4

- [ ] T062 [US4] Add only the four additive `OwnerInfo` policy fields in `muxcore/control/protocol.go`; do not add registry, topology, lifecycle-taxonomy, counter, or logging fields.
- [ ] T063 [US4] Populate truthful modern `protocol_era`, `sharing_policy`, `cache_policy`, and `lifecycle_policy` while retaining existing readiness and redaction rules in `muxcore/owner/owner.go`.
- [ ] T064 [US4] Preserve direct owner facts through daemon status and `HandleListOwners` without converting unknown state to legacy in `muxcore/daemon/daemon.go`.
- [ ] T065 [US4] Render available R1 `OwnerInfo` policy facts through the existing CLI status projection without disclosing prohibited data in `cmd/mcp-mux/main.go`.
- [ ] T066 [US4] Preserve the same minimal fields and redaction through `formatOwnerList` and `mux_list` without adding a topology or registry contract in `internal/mcpserver/server.go`.
- [ ] T067 [US4] Run the focused owner, daemon, CLI, and `mux_list` readback tests in `muxcore/owner/owner_test.go`, `muxcore/daemon/status_contract_test.go`, `muxcore/daemon/list_owners_test.go`, `cmd/mcp-mux/status_test.go`, and `internal/mcpserver/server_test.go`.
- [ ] T068 [US4] Stage and commit the proven readback slice: `muxcore/control/protocol.go`, `muxcore/owner/owner.go`, `muxcore/owner/owner_test.go`, `muxcore/daemon/daemon.go`, `muxcore/daemon/status_contract_test.go`, `muxcore/daemon/list_owners_test.go`, `cmd/mcp-mux/main.go`, `cmd/mcp-mux/status_test.go`, `internal/mcpserver/server.go`, and `internal/mcpserver/server_test.go`.

**Checkpoint**: An operator can distinguish an active R1 owner from legacy behavior using approved minimal facts only.

---

## Phase 7: Documentation, Customer Proof, Consumer Handoff, and Independent Check

**Purpose**: Document the additive public boundary, prepare the candidate-owned consumer handoff, prove exact built artifacts on Windows and Unix, then obtain an independent implementation-time checker receipt. Do not merge, tag, publish, or perform external consumer handoff in this phase.

- [ ] T069 [P] Document the explicit selector, forced isolation, cache-off behavior, no automatic fallback, request-scoped logging, host re-listen duty, and rollback rules in `README.md` and `README.ru.md`.
- [ ] T070 [P] Document the consumer-visible era boundary, preserved process-generation/`RetirementProven` authority, and R1 exclusions in `AGENTS.md` and `docs/mux-protocol.md`.
- [ ] T071 [P] Draft the user-visible R1 change and rollback statement without publication in `CHANGELOG.md` and `RELEASE_NOTES.md`.
- [ ] T072 [P] Prepare `specs/001-mcp-2026-07-28-r1/consumer-handoff.md` with the impacted `aimux`, `engram`, and other `muxcore` consumer map, additive migration, rollback, evidence, and release-stage publication boundary.
- [ ] T073 [P] Add a RED Windows runner-contract test/support script in `scripts/verify-r1-native-isolation.contract.ps1` that fails until the runner builds into a fresh `BaseDir`, drives modern and legacy scenarios, captures a transcript, and fails when required evidence is missing.
- [ ] T074 [P] Add a RED Unix runner-contract test/support script in `scripts/verify-r1-native-isolation.contract.sh` with the same fresh-build, modern/legacy, transcript, and missing-evidence contract.
- [ ] T075 Build the Windows customer-proof runner in `scripts/verify-r1-native-isolation.ps1` to satisfy T073.
- [ ] T076 Build the Unix customer-proof runner in `scripts/verify-r1-native-isolation.sh` to satisfy T074.
- [ ] T077 Run both runner-contract scripts and their platform runners against their exact fresh candidate builds. Record platform-specific transcripts, fixture IDs, and evidence hashes in `specs/001-mcp-2026-07-28-r1/platform-proof.md`.
- [ ] T078 Build the exact candidate from `cmd/mcp-mux/main.go`, execute Scenarios 1 through 8 in `specs/001-mcp-2026-07-28-r1/quickstart.md`, and record source/binary identity, policy spelling, all corpus results, captured opening bytes, control echo, status/redaction snapshots, modern/legacy outcomes, consumer-handoff preparation, rollback, and fixture revision in `specs/001-mcp-2026-07-28-r1/release-evidence.md`.
- [ ] T079 Stage and commit the proven documentation and repeatable customer-proof slice: `README.md`, `README.ru.md`, `AGENTS.md`, `docs/mux-protocol.md`, `CHANGELOG.md`, `RELEASE_NOTES.md`, `specs/001-mcp-2026-07-28-r1/consumer-handoff.md`, `scripts/verify-r1-native-isolation.contract.ps1`, `scripts/verify-r1-native-isolation.contract.sh`, `scripts/verify-r1-native-isolation.ps1`, `scripts/verify-r1-native-isolation.sh`, `specs/001-mcp-2026-07-28-r1/platform-proof.md`, and `specs/001-mcp-2026-07-28-r1/release-evidence.md`.
- [ ] T080 Have an `nvmd-checker` distinct from every maker inspect only the exact T079 parent, re-derive the selected-era and legacy-parity boundary from the pinned protocol source, execute the completed quickstart matrix, and write `specs/001-mcp-2026-07-28-r1/independent-check.md`. The receipt must record protocol revision; parent, source, and binary SHA; boundary re-derivation; quickstart scenarios executed; platform and fixture IDs; evidence hashes; findings with dispositions; and a verdict.
- [ ] T081 Stage and commit the evidence-only final slice: `specs/001-mcp-2026-07-28-r1/independent-check.md`. The receipt must name the exact T079 parent SHA it exercised; do not amend the T079 proof commit.

---

## Dependencies and Execution Order

### Phase dependency graph

```text
Fixture and corpus (T001–T004, committed)
  -> Foundational shared-era contract (T005–T010, committed)
    -> US1 native modern route (T011–T023, committed)
      -> US2 strict admission refusal (T024–T033, committed)
        -> US3 snapshot/handoff exclusion (T034–T039, committed)
          -> US3 reaper/zero-session removal (T040–T044, committed)
            -> US3 retry/respawn/finalization (T045–T051, committed)
              -> US3 loss/reconnect (T052–T057, committed)
                -> US4 minimal readback (T058–T068, committed)
                  -> Customer proof and consumer-handoff preparation (T069–T079, committed)
                    -> Independent checker receipt (T080)
                      -> Evidence-only commit (T081, committed)
```

- **Fixture**: T004 is a prerequisite for T005. The corpus becomes the nonzero deterministic SC-001 denominator.
- **US1 → US2**: CLI, engine, and daemon ingress overlap. Commit the positive route before the strict refusal branch.
- **T028 → T031**: T028 establishes local selector errors. T029 exposes them at engine ingress. Only then may T031 map admission errors on the CLI path without inventing a legacy retry.
- **US3**: Each lifecycle slice begins with observable RED tests, runs its focused suite, and commits before the next slice. Snapshot/handoff has no retry rehydration ownership.
- **US4**: It starts only after T057. US3 and US4 must not be claimed in parallel because they share daemon and owner source paths.
- **Independent check**: T080 examines the exact post-T079 parent. T081 is the final evidence-only commit; it is not source work.

### Per-story parallel examples

**US1 after T010**

```text
Parallel RED-test wave: T011, T012, T013, T014, T015
Then serialize implementation: T016 -> T017 -> T018 -> T019 -> T020 -> T021 -> T022 -> T023
```

**US2 after T023**

```text
Parallel RED-test wave: T024, T025, T026, T027
Then serialize admission implementation: T028 -> T029 -> T030 -> T031 -> T032 -> T033
```

**US3 after T033**

```text
Snapshot/handoff RED wave: T034, T035
Then: T036 -> T037 -> T038 -> T039
Removal RED wave: T040, T041
Then: T042 -> T043 -> T044
Retry/respawn/finalization RED wave: T045, T046, T047
Then: T048 -> T049 -> T050 -> T051
Loss/reconnect RED wave: T052, T053
Then: T054 -> T055 -> T056 -> T057
```

**US4 after T057**

```text
Parallel RED-test wave: T058, T059, T060, T061
Then serialize shared readback implementation: T062 -> T063 -> T064 -> T065 -> T066 -> T067 -> T068
```

### MVP scope

The smallest safe R1 demonstration is T001 through T033: fixture/corpus, shared era contract, US1, and US2. It proves native modern traffic and fail-closed unsafe admission. It is not release-ready until US3, US4, Phase 7 customer proof, the consumer-handoff preparation, and the independent checker receipt complete.

---

## Requirement and Success-Criterion Coverage

### Functional requirements

| Requirement | Task coverage |
| --- | --- |
| FR-001 / MPE-CORE-001 | T005–T006, T011–T018, T024–T031, T052–T055 |
| FR-002 / MPE-CORE-002 | T011, T014–T015, T019, T022–T023, T078 |
| FR-003 / MPE-CORE-003 | T013, T018–T020, T058–T068 |
| FR-004 / MPE-CORE-004 | T015, T020, T024–T031, T052–T055, T069–T070 |
| FR-005 / MPE-R1-001 | T024–T032, T078 |
| FR-006 / MPE-R1-002 | T001–T004, T012, T016–T021, T078 |
| FR-007 / MPE-R1-003 | T015, T020–T021, T046, T049, T053–T055, T078 |
| FR-008 / MPE-R1-004 | T007–T008, T013, T025–T032, T078 |
| FR-009 / MPE-R1-005 | T014, T018–T019, T045, T048, T052–T055 |
| FR-010 / MPE-R1-006 | T015, T020, T052, T078 |
| FR-011 / MPE-R1-007 | T015, T020, T078 |
| FR-012 / MPE-R1-008 | T034–T039, T078 |
| FR-013 / MPE-R1-009 | T040–T057, T077–T078 |
| FR-014 / MPE-R1-010 | T052–T057, T077–T078 |
| FR-015 / MPE-OBS-001 | T058–T068, T078 |
| FR-016 / MPE-OBS-002 | T069–T072 |
| FR-017 / MPE-OBS-003 | T072–T081 |

### Success criteria

| Success criterion | Task coverage |
| --- | --- |
| SC-001 | T001–T004, T012, T016–T023, T078 |
| SC-002 | T024–T033, T078 |
| SC-003 | T011, T014–T015, T019, T022–T023, T077–T078 |
| SC-004 | T034–T057, T077–T078 |
| SC-005 | T012, T015, T020–T021, T078 |
| SC-006 | T058–T068, T078 |
| SC-007 | T072–T081 |

### Custom checklist traceability

`[x]` markers in `checklists/protocol-lifecycle.md` and `checklists/requirements.md` certify requirements quality only. They do not mark implementation, tests, customer proof, review, or release evidence complete.

| Checklist obligation | Task coverage |
| --- | --- |
| CHK001 | T005–T006, T011–T018 |
| CHK002 | T013, T018–T020 |
| CHK003 | T014, T019 |
| CHK004 | T024–T032 |
| CHK005 | T015, T020–T021, T046, T049, T053–T055 |
| CHK006 | T015, T020, T052–T055 |
| CHK007 | T024, T028 |
| CHK008 | T024–T032 |
| CHK009 | T025, T029–T031 |
| CHK010 | T013, T026, T029–T030 |
| CHK011 | T027, T031–T032, T058–T068 |
| CHK012 | T011, T016–T019 |
| CHK013 | T012, T015, T020 |
| CHK014 | T011, T014–T015, T022 |
| CHK015 | T012, T016–T017 |
| CHK016 | T015, T020, T052 |
| CHK017 | T001–T004, T012, T022, T078 |
| CHK018 | T013–T014, T018–T019 |
| CHK019 | T015, T020, T052 |
| CHK020 | T015, T020, T078 |
| CHK021 | T015, T020, T052 |
| CHK022 | T053, T055, T078 |
| CHK023 | T034–T039 |
| CHK024 | T040–T051 |
| CHK025 | T052–T057 |
| CHK026 | T042, T047–T049, T054 |
| CHK027 | T047, T049–T051 |
| CHK028 | T034–T057 |
| CHK029 | T058–T066 |
| CHK030 | T007–T008, T013, T058–T066 |
| CHK031 | T058–T068 |
| CHK032 | T058–T068, T078 |
| CHK033 | T001–T004, T022, T032, T038, T043, T050, T056, T067, T078 |
| CHK034 | T073–T081 |
| CHK035 | T072, T078 |
| CHK036 | T001–T004, T012, T077–T078 |
| CHK037 | T015, T020–T021, T034–T039, T062, T069–T070 |
| CHK038 | T072, T078, T080 |

`checklists/requirements.md` remains satisfied: all 17 functional requirements and all 7 success criteria map above; no clarification or placeholder is introduced; legacy behavior is retained; and excluded R2/R3 capabilities remain outside the implementation tasks.

---

## Plan-Stage Correction to Task Mapping

| Plan-stage correction | Implementing tasks | Proof boundary |
| --- | --- | --- |
| Pre-election immutable era with legacy default | T005–T010, T011–T023, T024–T033 | Shared era commit, native-route commit, and strict-admission commit |
| Keep era independent from sharing and exclude R2 correlation | T013, T018–T020, T045–T057 | Forced-isolation and lifecycle tests with no new route authority |
| Preserve legacy identity byte-identical to the released baseline while making modern identity platform-safe | T014, T019, T022–T023 | Windows creation plus Unix segment/NUL proof and native-route commit |
| Treat current era-less snapshot/handoff payloads as exclusion, not R1 persistence | T034–T039 | Snapshot/handoff exclusion commit; no schema/version field |
| Preserve existing lifecycle authority through removal, retry, respawn, loss, reconnect, and blocked finalization | T040–T057 | Three focused lifecycle commits |
| Keep readback minimal and defer R3 observability | T058–T068 | Negative public-surface tests and readback commit |
| Prove the customer boundary and prepare, not publish, consumer handoff | T069–T081 | Exact-build proof commit, checker receipt, final evidence-only commit |

## Plan Boundary Contract to Task Mapping

| Plan boundary contract | Task coverage |
| --- | --- |
| Caller input | T005–T006, T011–T012, T016–T017, T024–T031 |
| Successful output | T001–T004, T012–T023, T078 |
| Unsafe input output | T024–T033, T078 |
| Existing attachment points | T016–T021, T028–T031, T034–T068 |
| Explicit non-changes | T015, T020–T021, T034–T039, T053–T055, T061–T062, T069–T072 |

---

## Task-Review Receipt and Finding Disposition

**Initial review state**: `R1TaskGraphReview` returned `REVISE`; `R1SpecKitAnalyze` recorded the findings below. This amended graph records dispositions only. It does **not** claim a post-correction GO, acceptance, or independent-checker verdict.

| Finding set | Disposition | Graph change |
| --- | --- | --- |
| G-001 fixture had no RED/run/commit | Accepted | T001–T004 create the deterministic fixture/corpus RED-to-commit slice; T005 depends on T004. |
| G-002 native-route commit omitted owner tests | Accepted | T023 stages `muxcore/owner/owner_test.go` and every named US1 source/test path. |
| G-003 CLI admission error lacked RED/run/commit | Accepted | T027, T031–T033 own the CLI no-fallback contract. |
| G-004 snapshot/handoff mixed retry rehydration | Accepted | T034–T039 own exclusion/no-hydration only. T045–T051 own retry rehydration. |
| G-005 lifecycle slice was oversized | Accepted | T040–T044, T045–T051, and T052–T057 are separate RED/run/commit slices. |
| G-006 US3/US4 had an unsafe parallel claim | Accepted | US4 begins only after T057. The dependency graph forbids a parallel US3/US4 claim. |
| G-007 checker evidence was uncommitted | Accepted | T080 writes the receipt and T081 commits it as evidence-only. |
| G-008 runners lacked prior RED support | Accepted | T073–T077 add and run Windows/Unix runner-contract support before T079 commits it. |
| S-001 plan said tasks were absent | Accepted | `plan.md` now identifies this generated graph. |
| A-1 snapshot/handoff era-persistence proposal | Rejected as an invalid R1 gate | FR-012 requires exclusion when the current era-less payload cannot carry a safe explicit era. T034–T039 prove exclusion/cold-start/fail-closed. Schema field/versioning remains R3. |
| A-2, A-5, A-25 readback negative scope and readiness invariant | Accepted | T058–T068 prove unchanged readiness and absence of registry/capability, `mux_engines`/topology, taxonomy, and counter work. |
| A-3, A-7, A-8, A-18 ordering and lifecycle partition | Accepted | The prior T026 → T028 ingress-to-CLI relation is the revised T029 → T031 dependency. Serialized lifecycle slices make RED-before-Code and scope ownership explicit. |
| A-4 platform identity | Accepted | T014 requires actual Windows-safe creation and Unix segment/NUL safety. |
| A-6 logging ambiguity | Accepted | T015 and the supporting contracts require opted-in valid request-scoped logs to reach the sole downstream once, never synthesized or broadcast. |
| A-9 retry rehydration definition | Accepted | T045/T048 implement the data-model and lifecycle-contract definition at existing observable boundaries. |
| A-10, A-11, A-21 checker parent/identity/schema | Accepted | T080 requires an `nvmd-checker`, exact T079 parent, and the receipt schema; T081 commits only that evidence. |
| A-12 runner proof ordering | Accepted | T073–T077 are RED-before-runner implementation and platform-specific proof. |
| A-13, A-19 plan traceability | Accepted | The two mapping appendices above map corrections and boundary contracts to tasks. |
| A-14 SC-001 denominator | Accepted | T001–T004 establish the deterministic corpus of at least 100 frames. |
| A-15 legacy wording | Accepted | T014/T019 use “byte-identical to the released baseline.” |
| A-16 data-model alignment | Accepted | Task language follows `ProtocolEra`, `OwnerIdentity`, and `RetryRehydration` terms in `data-model.md`. |
| A-17 doc source-text RED test | Deferred | Requirements quality stays under the checklists/review. R1 adds no source-text doc-lint test. |
| A-20 cleared-state reconnect branch | Accepted | T052–T057 prove daemon-survives/upstream-dies clearing and exact-era fresh admission. |
| A-22, A-24 existing scope consistency | Carried without new task | The scope lock and direct-OwnerInfo boundary remain unchanged. |
| A-23, A-28 consumer handoff | Accepted as preparation only | T072 prepares the candidate-owned artifact. External publication remains release-stage work. |
| A-26 post-opening ordering | Deferred to R2 | R1 keeps the specified opening-message boundary only. |
| A-27 compatibility matrix completeness | Accepted | The specification and public-policy contract enumerate support/refusal combinations and fail-close all omitted R1 combinations. |

## Implementation Strategy

1. **Build the safety core first**: Commit the fixture/corpus and era/control boundary before positive and negative admission paths.
2. **Quarantine in proven slices**: Keep snapshot/handoff exclusion, removal, retry/respawn/finalization, and loss/reconnect separate so each commit has its own focused proof.
3. **Expose only truthful minimal state**: Add four `OwnerInfo` facts through existing projections. Preserve readiness meaning and prove that R3 observability surfaces remain absent.
4. **Prove as a customer**: Run fresh Windows and Unix artifacts, retain modern and legacy evidence separately, prepare the consumer handoff, then have an independent checker examine the exact proof parent.
