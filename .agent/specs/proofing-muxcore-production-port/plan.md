# Implementation Plan: Proofing to Muxcore Production Port

**Spec:** `.agent/specs/proofing-muxcore-production-port/spec.md`
**Created:** 2026-05-19
**Status:** Draft

> **Provenance:** Planned by Codex on 2026-05-19 via `nvmd-plan`.
> Evidence from: clarified spec, clarification report, ADR 010, ADR 011,
> project constitution, SocratiCode status/search/graph queries, Serena symbol
> lookup, and direct file reads.
> **Mode:** CREATE, legacy slug-only SpecKit folder.
> **Open CRs:** CR-001 (`changes/CR-001-initial-scope/change.md`)
> **Confidence:** VERIFIED for plan scope, source files, and phase ordering;
> BLOCKED for production readiness until implementation and runtime smoke pass.

## Contract Check

Active contract: port proofing Phase 6-9 invariants into production `muxcore`
tests/refactors without importing the standalone PoC or changing the topology.

Planning handoff from clarification:

- Stable generation, handoff, reconnect, and lifecycle evidence is public
  operator API.
- Owner lifecycle cleanup routes through one daemon-level removal helper.
- Runtime smoke uses `time` as the primary real MCP upstream; `serena` is
  optional secondary only after `time` passes.

Out of scope remains launcher redesign, downstream consumer rebuilds, and any
default replay of already-sent side-effecting calls.

## Tech Stack

| Component | Choice | Rationale |
| --- | --- | --- |
| Language/runtime | Go 1.25.4 | Existing project stack from `go.mod`; no stack migration. |
| Multiplexer library | Existing `muxcore` packages | The feature is a production parity port inside `muxcore`, not a new wrapper. |
| Supervision | Existing `github.com/thejerf/suture/v4` usage | Owner lifecycle is already registered with suture; plan refactors cleanup around it. |
| IPC/status/protocol | Existing `muxcore/control`, `muxcore/ipc`, `muxcore/jsonrpc` | Avoid new protocol dependencies; preserve transparent MCP proxy behavior. |
| External libraries | None new | No library candidate: failure class is internal lifecycle/reconnect/observability state. |

## Source Requirements

No new framework or SDK API is selected by this plan. Implementation must still
verify MCP/JSON-RPC behavior against current official MCP protocol sources if it
adds any synthetic client-visible message. This plan intentionally avoids new
synthetic MCP messages; status fields are operator/control-plane data only.

## Architecture

The production seam remains:

```text
MCP host stdio -> resilient shim -> daemon control plane -> owner IPC -> upstream MCP server
```

The plan adds production parity evidence at existing `muxcore` package seams:

- `muxcore/owner`: same-stdio shim state, concurrent request dispatch, response
  demux, already-sent in-flight error-by-ID behavior.
- `muxcore/session`: pending tokens, bound reconnect history, owned cleanup
  primitives.
- `muxcore/daemon`: owner registry, refresh-token control, generation/status
  evidence, unified owner removal, reaper, graceful restart.
- `muxcore/snapshot`: owner metadata that must survive daemon handoff.
- `cmd/mcp-mux` and `scripts`: final fresh-binary smoke only after library parity.

### Reversibility Decision Table

| Decision | Tag | Evidence | Rollback / compatibility path |
| --- | --- | --- | --- |
| Expose generation/lifecycle evidence as documented status fields | PARTIALLY REVERSIBLE | User-selected clarification C1; current `HandleStatus` already exposes operator counters | Add fields only. If a field shape proves wrong before release, rename before tagging. After release, keep old field as deprecated alias for at least one compatibility window. |
| Route all owner removal through a daemon-level helper | PARTIALLY REVERSIBLE | C2; `Remove`, `SoftRemove`, `cleanupDeadOwner`, and `Reaper.sweep` currently remove owners through separate paths | Keep public `Remove`/`SoftRemove` signatures; rollback by making them thin wrappers again. |
| Add owner/daemon generation evidence to daemon, snapshot, and status | PARTIALLY REVERSIBLE | P8 false-positive guard needs restored-owner identity, not traffic success alone | Use additive fields and zero-value-compatible snapshot tags. If too noisy, keep internal fields and expose minimal status aliases. |
| Preserve ADR 010 error-by-ID for already-sent in-flight requests | REVERSIBLE only through new ADR | ADR 010 states blind replay risks duplicate side effects | No implementation pivot inside this CR; any retry-policy change requires a new ADR/spec amendment. |
| Use `time` as primary runtime smoke target | REVERSIBLE | C3; `time` is low-risk and fast compared with heavier upstreams | Swap/add targets in smoke script after `time` passes; do not block core parity on optional `serena` alone. |

REVERSIBILITY_AUDIT: PASS. No IRREVERSIBLE decision is introduced. The only
Hyrum-sensitive surface is additive status API, already approved by C1 and
guarded by compatibility notes.

### Strangler Mapping

| Strangler phase | This feature's equivalent |
| --- | --- |
| Identify seam | Current `consumer -> shim -> daemon -> owner -> upstream` seam is fixed by spec and ADR 011. |
| Establish facade | Existing shim/daemon facade stays in place; status contract becomes the observable facade. |
| Build replacement | P6-P9 production parity slices replace unreliable lifecycle behavior within existing components. |
| Route traffic and verify parity | Root/muxcore tests, PoC runner, then fresh `time` runtime smoke. |
| Mark/remove obsolete | No production component is removed. The standalone PoC remains an oracle, not a runtime dependency. |

## Data Model

This is runtime state, not database state. No schema migration is needed.

### DaemonStatus

| Field | Type | Constraints | Notes |
| --- | --- | --- | --- |
| daemon_generation | string | Non-empty per daemon process | New stable status field; generated at daemon construction. |
| pid | int | Existing | Current process ID remains operator evidence but is not sufficient alone. |
| handoff | object | Existing object, extended additively | Carries predecessor/successor/restored-owner evidence. |
| reaped_owner_count | uint64 | Monotonic per daemon process | Required for P9 lifecycle evidence. |
| owner_removal | object | Additive | Counts removal by reason and cleanup results. |

### OwnerEntry / OwnerStatus

| Field | Type | Constraints | Notes |
| --- | --- | --- | --- |
| owner_generation | string | Non-empty when owner is live | New stable per-owner identity, changes on respawn. |
| restored_from_owner_generation | string | Omit when fresh | Proves handoff/restore path instead of fresh spawn. |
| persistent | bool | Existing | Must survive idle and restart. |
| session_count | int | Existing owner status | Active-session guard for P9. |
| pending_requests | int64 | Existing owner status | Reaper veto input. |
| active_progress_tokens | int | Existing daemon status | Reaper veto input. |

### Session Token State

| State | Current owner | Planned change |
| --- | --- | --- |
| Pending token | `session.Manager.pending` | Add owner-key aware registration path so daemon can remove pending tickets for a reaped owner. |
| Bound reconnect history | `session.Manager.bound` | Add owner-key cleanup primitive for owner removal. |
| In-flight request correlation | `session.Manager.inflight` and resilient-client inflight map | Preserve existing error-by-ID behavior and P6 ID demux assertions. |

## API Contracts

### Control Status Contract

`Daemon.HandleStatus() map[string]any` remains the operator API. Additive fields
introduced by this feature are stable once released.

Top-level status additions:

| Field | Semantics |
| --- | --- |
| `daemon_generation` | Process-lifetime generation string for successor/predecessor distinction. |
| `reaped_owner_count` | Count of owners removed by idle/lifecycle reaping. |
| `owner_removal.total` | Count of owner-removal helper executions by this daemon. |
| `owner_removal.by_reason` | Counts keyed by `operator_hard`, `operator_soft`, `idle`, `zombie`, `handoff`, `restore_failed`. |
| `owner_removal.pending_tokens_removed` | Total pending tickets removed by owner-removal cleanup. |
| `owner_removal.bound_history_removed` | Total reconnect-history entries removed by owner-removal cleanup. |

`handoff` sub-map additions:

| Field | Semantics |
| --- | --- |
| `predecessor_pid` | PID of daemon that produced the handoff snapshot, when known. |
| `predecessor_daemon_generation` | Generation of predecessor daemon, when known. |
| `successor_daemon_generation` | Current daemon generation after restore. |
| `restored_owner_count` | Count of owners restored from handoff/snapshot. |
| `old_owner_socket_retired_count` | Count of predecessor owner sockets proven unreachable or retired. |

Per-server status additions:

| Field | Semantics |
| --- | --- |
| `owner_generation` | Owner identity for false-positive guards and respawn detection. |
| `restored_from_owner_generation` | Previous owner generation when restored; omitted on fresh spawn. |
| `restore_source` | `fresh`, `snapshot_handoff`, or `snapshot_fallback`; used by P8 to reject fresh-spawn masquerade. |

### Daemon Owner-Removal Helper

Planned internal shape:

```go
type ownerRemovalReason string

type ownerRemovalResult struct {
    ServerID             string
    Reason               ownerRemovalReason
    PendingTokensRemoved int
    BoundHistoryRemoved  int
    Soft                 bool
}

func (d *Daemon) removeOwner(serverID string, reason ownerRemovalReason, soft bool) (ownerRemovalResult, error)
```

Public `Remove(serverID)` and `SoftRemove(serverID)` remain as compatibility
wrappers. `cleanupDeadOwner`, `Reaper.sweep`, restore-failure cleanup, and
operator stop/restart paths must route through the helper instead of deleting
`d.owners` directly.

### Session Manager Cleanup Primitives

Keep existing `PreRegister(token, cwd, env)` source-compatible. Add owner-aware
APIs and use them from daemon code:

```go
func (sm *Manager) PreRegisterForOwner(token, ownerKey, cwd string, env map[string]string)
func (sm *Manager) RemovePendingForOwner(ownerKey string) int
func (sm *Manager) RemoveBoundForOwner(ownerKey string) int
```

`PreRegister` delegates to owner-key empty mode for compatibility; new daemon
paths use `PreRegisterForOwner`.

## File Structure

Planned production files:

```text
muxcore/
  daemon/
    daemon.go                         # thin wrappers, generation/status wiring
    owner_lifecycle.go                # new: unified removal helper and result/counters
    owner_lifecycle_test.go           # new: helper coverage
    status_contract_test.go           # new: stable status field coverage
    generation_handoff_parity_test.go # new: P8 parity
    persistent_idle_parity_test.go    # new: P9 parity
    reaper.go                         # route eviction through helper
    snapshot_reattach_test.go         # extend/guard handoff restore evidence
  owner/
    owner.go                          # only if P6 dispatch/status evidence needs code changes
    concurrent_demux_parity_test.go   # new: P6 parity
    resilient_client_reconnect_test.go# extend P7/ADR010 parity if needed
  session/
    session_manager.go                # owner-aware pending/bound cleanup primitives
    session_manager_test.go           # cleanup primitive tests
  snapshot/
    snapshot.go                       # additive owner/daemon generation fields if needed
    snapshot_test.go                  # zero-value compatibility and round-trip tests
cmd/
  mcp-mux/
    main.go or daemon.go              # only if final smoke reveals CLI wiring gap
scripts/
  smoke-time-upstream.ps1             # new or extended final runtime smoke
```

Release-readiness files may be required before tagging but are not the first
production parity path:

```text
docs/PRODUCTION-TESTING-PLAYBOOK.md   # release gate if still missing
tests/critical/                       # release gate if still missing
```

## Phases

The plan is Complex: more than 10 tasks, cross-cutting refactor, and shared
daemon/session/owner state. SocratiCode index is green. Graph evidence shows
`muxcore\daemon\daemon.go` is imported by `cmd\mcp-mux\daemon.go`,
`cmd\mcp-mux\daemon_test.go`, `internal\mcpserver\cross_engine_integration_test.go`,
and `muxcore\engine\engine.go`; owner and daemon changes must therefore stay
sequential until their public behavior is stable.

### Phase 1: Status And Lifecycle Foundation

Requirements covered: FR-6, FR-7, NFR-3, NFR-5, NFR-6, US2, US4.

Work:

1. Add RED tests for stable status contract fields in
   `muxcore/daemon/status_contract_test.go`.
2. Add owner-aware pending/bound cleanup tests in
   `muxcore/session/session_manager_test.go`.
3. Add session cleanup primitives in `muxcore/session/session_manager.go`.
4. Add daemon-level `removeOwner` helper and counters in
   `muxcore/daemon/owner_lifecycle.go`.
5. Route `Remove`, `SoftRemove`, `cleanupDeadOwner`, and `Reaper.sweep` through
   the helper.
6. Add owner/daemon generation generation and status wiring, using additive
   fields only.

Verification:

```powershell
Push-Location muxcore
go test ./session -run "Owner|Pending|Bound|Reconnect" -count=1
go test ./daemon -run "Status|OwnerLifecycle|Reaper|Remove|SoftRemove" -count=1
Pop-Location
```

#### Concurrent Work Directives (computed)

Sequential phase. The apparent file split is not enough for safe parallelism:
daemon helper code depends on session cleanup signatures, and status tests
consume generation/removal fields from daemon state. Soft-parallel handoff is
allowed only after the exact `session.Manager` cleanup API signatures land:

- Session cleanup primitive tests/code can be authored first in
  `muxcore/session/*`.
- Daemon lifecycle helper starts after the cleanup signatures are committed.
- Status contract tests can be drafted while helper implementation is in
  progress, but they must not be marked `[P]` downstream because both converge
  on `muxcore/daemon` exported status behavior.

#### Contingency Branches

**If-Wrong:** stable status contract field set is too broad or noisy.
**Trigger:** implementation needs more than the listed fields to pass P8/P9, or
review flags fields that expose unstable internals without acceptance value.
**Cost so far:** Phase 1 tests plus additive fields, before release.
**Pivot:** keep `daemon_generation`, `owner_generation`, `handoff.restored_owner_count`,
and cleanup counters as the minimum stable set; move extra diagnostic detail
behind internal tests or a nested `debug` map before release.

**If-Wrong:** unified `removeOwner` helper creates deadlocks or suture ordering regressions.
**Trigger:** `go test ./daemon -run "Remove|SoftRemove|Reaper"` hangs, races, or
changes supervisor cleanup order compared with existing tests.
**Cost so far:** one helper file plus wrapper rewiring.
**Pivot:** split helper into two phases: `prepareOwnerRemovalLocked` for registry
and cleanup metadata, then `finishOwnerRemovalUnlocked` for supervisor/owner
shutdown. Keep public wrappers unchanged.

### Phase 2: P7 Refresh-Token Reconnect Parity

Requirements covered: FR-3, FR-10, NFR-1, NFR-3, US1.

Work:

1. Audit existing refresh tests:
   `TestReconnect_RefreshesAndSucceeds`,
   `TestReconnect_DrainsAlreadySentInflightWithoutReplay`,
   `TestHandleRefreshSessionToken_CountersIncrement`, and
   `TestReconnectRefreshPreservesOwner`.
2. Add the missing production parity test if existing tests do not prove all
   P7 evidence in one path: consumed original token, different refreshed token,
   owner alive/accepting, refresh counter increment, fallback counter unchanged.
3. Ensure fallback-spawn counter increments only on explicit
   `ReconnectReason == "fallback_spawn"` and never on refresh success.
4. Preserve ADR 010: already-sent in-flight requests receive one JSON-RPC error
   by original ID and are not replayed by default.

Verification:

```powershell
Push-Location muxcore
go test ./owner -run "TestReconnect" -count=1
go test ./daemon -run "Refresh|Reconnect" -count=1
go test ./session -run "RegisterReconnect|LookupHistory|Owner" -count=1
Pop-Location
```

#### Concurrent Work Directives (computed)

Sequential after Phase 1. P7 spans `owner`, `daemon`, and `session`, and the
same token/status counters are shared across the tests. Do not parallelize
until Phase 1 status and cleanup signatures are stable.

#### Contingency Branches

**If-Wrong:** owner-alive refresh succeeds in unit tests but fails through daemon restore.
**Trigger:** daemon integration test falls back to spawn while direct
`RegisterReconnect` tests pass.
**Cost so far:** P7 test work, no public API changes beyond Phase 1.
**Pivot:** inspect snapshot restore of bound token history before touching shim
retry policy. Add restore-specific test in `muxcore/daemon/snapshot_reattach_test.go`
and keep fallback spawn as fallback-only.

### Phase 3: P6 Concurrent Demux Parity

Requirements covered: FR-2, FR-10, NFR-1, NFR-3, US3.

Work:

1. Add a production owner/resilient-client parity test with two outstanding
   JSON-RPC IDs and intentionally out-of-order responses.
2. Include restart/reconnect boundary evidence: successor daemon generation or
   restored owner generation must differ from predecessor evidence.
3. Assert both responses are delivered exactly once and matched to original IDs.
4. If the test fails because pipe-backed upstream dispatch is serial, isolate
   the fix to owner dispatch/demux without changing SessionHandler concurrency
   semantics that are already documented as concurrent.

Verification:

```powershell
Push-Location muxcore
go test ./owner -run "Concurrent|Demux|ResilientClient" -count=1
go test ./owner -race -run "Concurrent|Demux" -count=1
Pop-Location
```

#### Concurrent Work Directives (computed)

Sequential with P7/P8. P6 touches request/response correlation and resilient
client inflight state, which is the same failure boundary as ADR 010. Do not
run P6 in parallel with P7 reconnect or P8 handoff work.

#### Contingency Branches

**If-Wrong:** true concurrency requires broad upstream pipe protocol changes.
**Trigger:** P6 RED shows the only failing boundary is pipe-backed upstream
serialization, and fixing it would touch remap/cache/cancel paths beyond owner
dispatch.
**Cost so far:** one parity test and analysis.
**Pivot:** stop behavior changes, keep the failing test marked with a focused
RED evidence note, and amend the spec before broadening into a pipe-dispatch
architecture change. SessionHandler parity can still land if independent.

### Phase 4: P8 Generation-Aware Handoff Parity

Requirements covered: FR-4, FR-6, NFR-2, NFR-3, NFR-6, US2.

Work:

1. Add daemon generation and owner generation to snapshot/handoff restore
   evidence with zero-value-compatible snapshot tags.
2. Extend handoff/status tests to fail when traffic succeeds through fresh spawn
   but `restored_owner_count` or `restore_source` evidence is missing.
3. Prove old owner socket retirement or equivalent owner invalidation.
4. Keep public status additions documented and additive.

Verification:

```powershell
Push-Location muxcore
go test ./snapshot -run "RoundTrip|OwnerSnapshot" -count=1
go test ./daemon -run "Handoff|Snapshot|Status|Generation" -count=1
Pop-Location
```

#### Concurrent Work Directives (computed)

Sequential after Phase 1. Snapshot and status changes are tightly coupled:
status fields must reflect the exact persisted/restore shape. Do not parallelize
snapshot and status work unless a signature-only handoff lands first.

#### Contingency Branches

**If-Wrong:** stable status cannot safely expose all handoff internals.
**Trigger:** tests require platform-specific socket details that cannot be made
cross-platform or stable.
**Cost so far:** additive snapshot/status fields before release.
**Pivot:** expose platform-neutral evidence (`restore_source`, `restored_owner_count`,
`owner_generation`, `daemon_generation`) as stable API and keep socket-specific
retirement proof in platform-tagged tests/log assertions.

### Phase 5: P9 Persistent / Idle Lifecycle Parity

Requirements covered: FR-5, FR-7, NFR-2, NFR-3, NFR-4, US4.

Work:

1. Add short-TTL parity tests for:
   - non-persistent idle owner reaped;
   - active non-persistent session survives while connected;
   - active owner reaps after close;
   - persistent owner survives idle;
   - persistent classification restores through restart;
   - reaped owner pending/bound tokens are removed;
   - respawned non-persistent owner gets a new owner generation.
2. Route every reaper removal through `removeOwner`.
3. Ensure `Reaper.sweep` still preserves dead persistent owner respawn behavior.
4. Assert lifecycle counters rather than sleeps alone.

Verification:

```powershell
Push-Location muxcore
go test ./daemon -run "Reaper|Persistent|OwnerLifecycle|Generation" -count=1
go test ./session -run "Remove.*Owner|Sweep" -count=1
Pop-Location
```

#### Concurrent Work Directives (computed)

Sequential after Phase 1. P9 uses the same owner-removal helper and session
cleanup primitives; parallel work would duplicate lifecycle authority.

#### Contingency Branches

**If-Wrong:** owner-key cleanup cannot remove stale pending tokens because
legacy pending tokens lack owner identity.
**Trigger:** P9 test proves `RemovePendingForOwner` cannot distinguish pending
tokens for one owner from another.
**Cost so far:** session cleanup primitive tests and daemon helper draft.
**Pivot:** introduce owner-aware `PreRegisterForOwner` and migrate daemon calls;
leave legacy `PreRegister` as source-compatible fallback with owner key empty.
Treat owner-keyless pending tokens as TTL-only cleanup.

### Phase 6: Full Test Gate And PoC Regression Oracle

Requirements covered: FR-1, FR-9, NFR-7.

Work:

1. Run focused package tests from Phases 1-5.
2. Run full `muxcore` test suite.
3. Run root test suite.
4. Run current-topology PoC runner to ensure production changes did not regress
   the experimental oracle.
5. Run `git diff --check`.

Verification:

```powershell
Push-Location muxcore
go test ./... -count=1
Pop-Location
go test ./... -count=1
.\scripts\run-current-topology-poc.ps1 -WatchSeconds 1
git diff --check
```

#### Concurrent Work Directives (computed)

Sequential gate. Release-quality test gates are ordered so the first failure
identifies the narrowest layer.

### Phase 7: Fresh-Binary Runtime Smoke

Requirements covered: FR-8, NFR-7, US1.

Work:

1. Build a fresh `mcp-mux.exe` without replacing the locked workstation binary
   unless deployment is explicitly approved.
2. Run a controlled smoke against `time` as primary target.
3. Verify no `connection closed: initialize response`.
4. Verify status shows expected version/process/generation and reconnect or
   handoff counters.
5. Optionally run `serena` smoke only after `time` passes; classify any failure
   as upstream-specific or muxcore-specific.

Verification:

```powershell
go build -o .agent/tmp/mcp-mux-smoke/mcp-mux.exe ./cmd/mcp-mux
# smoke command to be finalized during implementation:
# .\scripts\smoke-time-upstream.ps1 -Binary .agent\tmp\mcp-mux-smoke\mcp-mux.exe
```

#### Concurrent Work Directives (computed)

Sequential. Smoke must run after library parity and full test gates; running it
earlier reintroduces the original false-positive risk.

#### Contingency Branches

**If-Wrong:** `time` smoke passes but fresh Claude/Codex sessions still fail.
**Trigger:** controlled `time` smoke reports healthy attach/restart, but a new
MCP host session still shows `connection closed: initialize response`.
**Cost so far:** feature implementation plus smoke harness.
**Pivot:** keep library changes, open a targeted runtime-host investigation
against Codex/Claude logs and process/socket ownership. Do not mutate muxcore
again until the host-specific failure is reproduced through a controlled shim.

## Library Decisions

| Component | Library | Version | Rationale |
| --- | --- | --- | --- |
| Owner lifecycle supervision | Existing `github.com/thejerf/suture/v4` | v4.0.6 from `go.mod` | Already manages owner services; plan changes removal orchestration, not supervisor choice. |
| UUID/generation | Custom random hex helper or existing `google/uuid` if already imported in touched package | Existing dependency is indirect | Prefer small local helper using `crypto/rand` to avoid new imports unless package already uses UUID. |
| Runtime smoke upstream | Existing `uvx mcp-server-time` path from workstation config | External command, no library dependency | Primary smoke target chosen by clarification C3. |
| Status serialization | Existing `map[string]any` control status | Existing API | Additive fields preserve compatibility. |

## Decision Evidence

| Claim | Classification | Evidence |
| --- | --- | --- |
| SocratiCode index is usable for planning | VERIFIED | `codebase_status`: green, 1583 chunks, watcher active. |
| `Daemon.HandleStatus` is the operator status surface | VERIFIED | `muxcore/daemon/daemon.go:1335-1383`; Serena symbol lookup. |
| `Remove`, `SoftRemove`, and reaper currently remove through separate paths | VERIFIED | `muxcore/daemon/daemon.go:952-1030`; `muxcore/daemon/reaper.go:163-176`; `cleanupDeadOwner` search result. |
| `session.Manager` lacks owner-keyed pending tokens today | VERIFIED | `muxcore/session/session_manager.go:18-23`, `97-108`, `172-190`. |
| Refresh-token reconnect primitives already exist but need parity integration evidence | VERIFIED | `muxcore/daemon/daemon.go:1303-1332`; `muxcore/session/session_manager.go:221-268`; existing tests from `rg`. |
| Direct runtime smoke first would be a false-positive risk | INFERRED | Spec requires P6-P9 parity before smoke; aimux decision framework ranked parity-first highest. |

## Reusability Awareness

None. All planned modules are improvements to the existing reusable unit
`muxcore`. No new reusable package is proposed. Cross-project similarity was
not queried through Engram because Engram is not reliably attached in this
session; repo-local evidence and existing ADRs are sufficient for this plan.

## Domain Modeling

DDD evaluated: not needed. Runtime entities are infrastructure concepts
(`Daemon`, `OwnerEntry`, `Session`, token history, snapshot, handoff, reaper),
not business aggregates. The Data Model section above documents runtime state
ownership instead.

## DX Review

This is a library/CLI feature. Developer experience goal: downstream consumers
should not need source changes for additive status fields and internal lifecycle
cleanup.

TTHW impact:

- Existing consumers wrapping commands with `mcp-mux` remain zero-config.
- Existing `muxcore` consumers compile unchanged unless they choose to read new
  status fields.
- Runtime smoke should become the public "hello reliability" path for operators
  after implementation.

## Unknowns And Risks

| Unknown / risk | Impact | Resolution strategy |
| --- | --- | --- |
| Whether pipe-backed upstream dispatch can satisfy P6 without broader remap/cache changes | HIGH | Start P6 with RED parity test. If fix spans remap/cache/cancel broadly, amend before changing architecture. |
| Exact stable status field names may need one implementation pass | MEDIUM | Treat names in API Contracts as target; rename only before release. After release, add aliases/deprecation instead. |
| Windows binary locks may block local deployment smoke | HIGH | Build fresh binary to temp path first. Deployment/swap requires explicit approval or closed locking sessions. |
| Missing critical suite/playbook still blocks a full release | MEDIUM | This plan closes feature parity. Before tag/deploy, run production-ready gate and add `tests/critical` / playbook if still missing. |
| Optional `serena` smoke may fail for upstream-specific reasons | MEDIUM | Do not block core parity unless failure reproduces with `time` or controlled shim evidence. |

## Constitution Compliance

| Principle | Compliance |
| --- | --- |
| Transparent Proxy | No new client-visible MCP messages; status additions are operator/control-plane only. |
| Zero-Configuration Default | Runtime topology and default wrapping remain unchanged. |
| Upstream Authority | Owner/status changes do not alter tool lists or upstream capabilities. |
| Session Isolation Correctness | P6 and P7 explicitly test response/token routing by session/request identity. |
| Graceful Degradation | Refresh-token path keeps fallback spawn as fallback, not normal path. |
| Cross-Platform | P8/P9 status evidence must avoid platform-only assertions or use build-tagged tests. |
| MCP Spec Compliance | ADR 010 error-by-ID behavior is preserved; synthetic progress messages remain out of scope. |
| Test Before Ship | Every phase is test-first or evidence-first; runtime smoke waits until package tests pass. |
| Atomic Upgrades | Runtime smoke avoids claiming deploy success while Windows locks can block binary replacement. |
| No Stubs, No Workarounds | Plan requires RED/GREEN parity tests and prohibits PoC import as production implementation. |

## Validation Checklist

- [x] Every FR maps to one or more phases.
- [x] Every NFR has a concrete approach or release-gate risk.
- [x] Library decisions are documented; no new external library is selected.
- [x] Source requirements are recorded for MCP/protocol-sensitive behavior.
- [x] Data architecture is documented as runtime state, with database N/A.
- [x] Test strategy names failure modes each parity slice must expose.
- [x] File structure follows existing Go package layout and avoids growing
      `daemon.go` further where a focused file is cleaner.
- [x] Phases have boundaries, deliverables, and verification commands.
- [x] Constitution compliance quotes project principles.
- [x] Phase 0 reversibility audit emitted PASS.
- [x] Parallelism analysis ran against green SocratiCode index; phases are
      sequential where shared state makes parallelism unsafe.

## Next Step

Proceed to `nvmd-checklist` for requirement quality, then `nvmd-tasks` to turn
this plan into executable CR tasks.
