# Plan: muxcore engine.Config.OnInject — fire-and-forget IPC frame injection

**Slug:** muxcore-oninject-callback
**Created:** 2026-04-28
**Spec:** `.agent/specs/muxcore-oninject-callback/spec.md`
**Status:** Draft (CREATE mode — plan.md did not exist)

> **Provenance:** Planned by Opus 4.7 on 2026-04-28.
> Evidence: spec.md, clarification-report-2026-04-28.md, codebase exploration (`muxcore/owner/resilient_client.go`, `muxcore/engine/engine.go:21,381`), AGENTS.md v0.21.6 + v0.22.0 passthrough precedent, gist `af3b003` reference patch.
> Confidence: VERIFIED.
> Mode: CREATE (no `feature_id` — legacy slug-only spec per FR-11; no `_index.json` registry on this branch).

## Tech Stack

- **Language:** Go (matches existing muxcore stack)
- **Stdlib only:** `errors` (sentinel `errors.New`), `sync` (only if `sync.Once` chosen for single-fire enforcement), `sync/atomic` (alternative for close-flag)
- **No new third-party dependencies**
- **Test stack:** stdlib `testing` + existing helper patterns from `muxcore/owner/resilient_client_test.go`
- **Target:** muxcore master at `eebee00` (v0.22.1)
- **Release tags:** `muxcore/v0.23.0` (MINOR — purely additive API) + `v0.23.0` (binary tag)

## Architecture

### Reversibility Decision Table

| # | Decision | Reversibility | Evidence (spec.md anchor) | Alternatives | Rollback Cost |
|---|----------|---------------|---------------------------|--------------|---------------|
| D1 | Public API: `OnInject` field on both `ResilientClientConfig` + `engine.Config` | **IRREVERSIBLE** | US3 P1 — "consumer ... bumps `muxcore@v0.23.0` ... zero source change" | (a) opt-in via build tag; (b) separate `injectable` package | High — once tagged v0.23.0, removing the field breaks SemVer; downstream consumers (aimux) lock onto signature |
| D2 | Single-fire semantics (FR-4) | **PARTIALLY REVERSIBLE** | US1 P1 — "wire shim's `IPCSink.SendFunc` once at engine construction" | Multi-fire on every reconnect (re-arms callback) | Medium — can extend to multi-fire later (additive); narrowing is breaking |
| D3 | Reuse existing `msgFromCC` channel vs new dedicated `injectCh` | **REVERSIBLE** | FR-2 ("No priority lane, no second mutex") + Clarifications C3 | New channel + select-merge in `runIPCWriter` | Low — internal implementation, no public surface change |
| D4 | Sentinel error VALUES (`var Err... = errors.New(...)`) vs error type with `Code()` method | **IRREVERSIBLE** | NFR-1 — "non-blocking inject" + standard Go `errors.Is` pattern | Custom `*InjectError` struct with method | High — public API; consumers will write `errors.Is(err, ErrInjectFull)` |
| D5 | SemVer bump `v0.23.0` (MINOR) vs `v0.22.2` (PATCH) | **IRREVERSIBLE** | SemVer §7 — "MINOR version when functionality added in backward compatible manner" | PATCH bump (v0.22.2) — wrong, this is API addition | High — once tag is pushed and consumed, retraction is messy |

**Reversibility audit signal:** `REVERSIBILITY_AUDIT: PASS` — all 5 decisions classified, IRREVERSIBLE decisions cite spec evidence, alternatives enumerated for PARTIALLY REVERSIBLE / IRREVERSIBLE.

### Component map

```
engine.Config.OnInject  (NEW field — passthrough)
        │
        ▼
runProxy() in engine.go:381
        │
        ▼
owner.RunResilientClient(owner.ResilientClientConfig{
    OnInject: cfg.OnInject,  ← passthrough wiring (NEW line)
    ...
})
        │
        ▼
ResilientClientConfig.OnInject  (NEW field)
        │
        ▼
After initial handshake in RunResilientClient():
    inject := func(b []byte) error {
        if rc.closed.Load() { return ErrInjectClosed }
        select {
        case rc.msgFromCC <- b:
            return nil
        default:
            return ErrInjectFull
        }
    }
    rc.cfg.OnInject(inject)   ← single-fire, sync.Once-guarded
```

## Data Model

No persistent data model. API additions only:

```go
// muxcore/owner/resilient_client.go (additive)

var (
    ErrInjectFull   = errors.New("muxcore: inject buffer full")
    ErrInjectClosed = errors.New("muxcore: inject channel closed")
)

type ResilientClientConfig struct {
    // ... existing fields preserved ...

    // OnInject, when non-nil, is invoked exactly once after the initial IPC
    // handshake completes. The closure pushes raw JSON-RPC frames into msgFromCC
    // via select-default semantics. Zero value (nil) preserves pre-v0.23 behavior.
    OnInject func(inject func([]byte) error)
}
```

```go
// muxcore/engine/engine.go (additive)

type Config struct {
    // ... existing fields preserved ...

    // OnInject forwards to ResilientClientConfig.OnInject. See owner package
    // documentation for semantics. Zero value (nil) preserves pre-v0.23 behavior.
    OnInject func(inject func([]byte) error)
}
```

## API Contracts

### Closure contract (caller-facing)

```go
// inject is single-instance, safe for concurrent use, lifecycle-aware.
//
// Returns nil on successful enqueue.
// Returns ErrInjectFull if msgFromCC buffer is saturated (transient backpressure).
// Returns ErrInjectClosed if the proxy has exited (no more writes accepted).
//
// Never blocks. Never panics on closed-channel write (closed flag checked first).
inject := func(frame []byte) error
```

### Single-fire invariant

`OnInject(inject)` is invoked exactly once per `ResilientClient` lifecycle, after the
FIRST successful IPC handshake. Reconnects do NOT re-fire — the buffer survives across
reconnects via existing `flushBuffer` path.

### Concurrent safety

`inject(b)` is safe for concurrent use across goroutines. Implementation uses
`select { case ch <- b: default: ... }` which is atomic relative to other senders;
no external mutex required by callers.

## File Structure

```
muxcore/
  owner/
    resilient_client.go        ← MODIFY: add OnInject field, sentinel errors, single-fire wiring
    resilient_client_test.go   ← MODIFY: add 2 regression tests
  engine/
    engine.go                  ← MODIFY: add OnInject field + passthrough in runProxy

AGENTS.md                      ← MODIFY: add v0.23.0 release entry under "muxcore Library API"
```

No new files. All changes are additive within existing files. ~50-80 LOC new code (production) + ~80-120 LOC tests.

## Phases

> **Computed parallelism note (Phase 0.5):** SocratiCode index green (1381 chunks). File-graph empty (`codebase_graph: 149 files, 0 edges` per CONTINUITY) — fallback to grep+import-chain analysis. Confirmed: `engine/engine.go:21,381` imports `owner` package. T1 (owner additions) precedes T2 (engine passthrough) and T3 (owner tests). T2 + T3 + T4 are independent post-T1.

### Phase 1: Owner package additions (T1 — sequential foundation)

**Deliverable:** `muxcore/owner/resilient_client.go` with `ResilientClientConfig.OnInject` + sentinel errors + single-fire closure wiring inside `RunResilientClient`.

**Tasks:**
- T1: Add `OnInject` field to `ResilientClientConfig`, export `ErrInjectFull` / `ErrInjectClosed`, implement single-fire closure invocation after initial handshake; add `closed atomic.Bool` (or `sync.Once` + atomic flag) to enforce `ErrInjectClosed` post-shutdown.

**Acceptance:**
- `go build ./muxcore/owner/...` clean
- `go vet ./muxcore/owner/...` clean
- Existing `muxcore/owner/...` test suite green (zero regressions when `OnInject == nil`)

#### Concurrent Work Directives (computed)

Phase 1 is sequential — T1 is the foundation. No `[P]` markers.

#### Contingency Branches

Decision D2 (single-fire) — PARTIALLY REVERSIBLE → Medium contingency:
- **If single-fire creates ergonomic friction** (e.g., consumer logger needs re-registration on reconnect): extend to multi-fire by removing `sync.Once` guard. Additive change, no API break. Roll forward.
- **If `closed` flag races with `msgFromCC <-`**: switch to `select` with closed-channel pattern (`case <-rc.done: return ErrInjectClosed`). Internal-only change.

### Phase 2: Engine passthrough + regression tests (T2, T3, T4 [P])

**Deliverable:** `engine.Config.OnInject` field + `runProxy` passthrough; 2 regression tests; AGENTS.md v0.23.0 entry.

**Tasks:**
- **T2 [P after T1]:** Add `OnInject` to `engine.Config`, wire `cfg.OnInject` → `owner.ResilientClientConfig{OnInject: cfg.OnInject}` at `engine.go:381`. Empty value preserves byte-identical behavior.
- **T3 [P after T1]:** Add `TestResilientClient_OnInject_DeliversFrames` + `TestResilientClient_OnInject_BufferFull` to `muxcore/owner/resilient_client_test.go`. Both pass on Linux + macOS + Windows.
- **T4 [P with T1/T2/T3]:** Update `AGENTS.md` with v0.23.0 release entry under `muxcore Library API` section, mirroring v0.22.0 entry style (additive API, no migration required for non-adopters).

**Concurrent Work Directives (computed):**
- T2 PARALLEL-WITH T3: graph evidence — T2 modifies `muxcore/engine/engine.go`; T3 modifies `muxcore/owner/resilient_client_test.go`. Disjoint file set. No shared symbol mutations.
- T4 PARALLEL-WITH T2 and T3: T4 modifies `AGENTS.md` (docs). No code overlap.

**Acceptance:**
- `go build ./...` clean (root + muxcore)
- `go vet ./...` clean
- `go test ./muxcore/...` full suite green
- 2 new tests PASS on Linux + macOS + Windows CI matrix (NFR-5)

#### Contingency Branches

Decision D3 (reuse `msgFromCC`) — REVERSIBLE → Light contingency only:
- **If T3 BufferFull test reveals races on saturation**: drop a separate `injectCh` + select-merge in `runIPCWriter`. Internal-only refactor, no public API impact.

### Phase 3: Release (T5)

**Deliverable:** `muxcore/v0.23.0` + `v0.23.0` tags pushed; GitHub releases live; release notes lifted from AGENTS.md.

**Tasks:**
- **T5:** After PR merge to master:
  1. `git tag muxcore/v0.23.0` + `git tag v0.23.0` on the merge commit
  2. `git push --follow-tags origin master`
  3. `gh release create muxcore/v0.23.0 --notes-file <release-notes>`
  4. `gh release create v0.23.0 --notes-file <release-notes>`
  5. Comment on `thebtf/mcp-mux#107` with API confirmation + release links

**Acceptance:**
- Both tags visible on `git ls-remote --tags origin`
- Both GitHub releases live and matched to AGENTS.md entry
- `thebtf/mcp-mux#107` closed with completion comment

#### Contingency Branches

Decision D5 (v0.23.0 MINOR bump) — IRREVERSIBLE → Deep contingency:
- **If aimux discovers signature mismatch downstream after tag**: cut `muxcore/v0.23.1` PATCH with refined signature (still additive). Cannot retract v0.23.0 — git tags are pull-only on operator side. Documentation must explicitly mark v0.23.0 as superseded.
- **If patch reveals a fundamentally wrong contract**: revert via `muxcore/v0.24.0` with field renamed/restructured + AGENTS.md migration note. Original v0.23.0 stays for back-compat.

## Library Decisions

| Component | Decision | Rationale |
|-----------|----------|-----------|
| Sentinel errors | `errors.New(...)` package values | Standard Go pattern; consumers use `errors.Is(err, ErrInjectFull)`. No third-party `pkg/errors`. |
| Single-fire enforcement | `sync.Once` OR `atomic.Bool` flag | Both stdlib. `sync.Once` is canonical for "exactly once" intent. Decide at implementation by which gives cleaner test coverage. |
| Close-flag | `atomic.Bool` | Stdlib. Avoids mutex overhead on hot inject path. |
| Channel reuse | Existing `msgFromCC` (1000-deep buffer) | Decided per FR-2 + Clarifications C3. No new dependency. |

**Library-first search result:** No third-party library applicable. The change is a Go stdlib pattern (callback + sentinel errors + atomic flag); custom implementation is correct.

## Reusability Awareness

Detection invoked per planned module (Phase 1 production code):

### Candidate: OnInject callback pattern

- **Phase:** plan
- **Boundary:** `func(inject func([]byte) error)` — single-fire, lifecycle-aware push pattern
- **Project-specific deps:** None (stdlib `errors`, `sync/atomic` only)
- **Interface stability:** Draft (sketched in spec.md FR-1..5; no implementation yet)
- **Cross-Project Similarity:** Not queried — engram unavailable for cross-project pattern lookup at plan phase. Re-query at implementation time.
- **Maturity:** Single consumer planned (aimux `engram#178`). Below Rule-of-Three threshold for promotion.
- **Next step:** Keep watching. If a third consumer (engram, mcp-launcher, future muxcore consumer) wires the same callback pattern, evaluate for promotion to a shared `inject-pattern` package.

## Domain Modeling

DDD evaluated — not needed. Rationale: 0 entities in proposed schema (API surface only — 2 struct fields + 2 sentinel errors), 0 domain invariants (no business rules), no state machine beyond existing CONNECTED/RECONNECTING. FR-D2 detection criteria do not match.

## Unknowns and Risks

| # | Unknown / Risk | Severity | Mitigation |
|---|----------------|----------|------------|
| R1 | Race between `inject(b)` and proxy `Close()` | LOW | EC-4 codified; implementation MUST check `closed` flag before channel send (atomic ordering: load flag → branch). Test T3 covers this implicitly via close-then-inject sequence. |
| R2 | `OnInject` callback panics inside consumer code | LOW | EC-6 codified; panic propagates per existing `RefreshToken` semantics. No internal recover — keeps signature simple. Consumer responsibility documented. |
| R3 | Buffer pressure under sustained injection > drain rate | LOW | NFR-1 + EC-3; `select-default` returns `ErrInjectFull` immediately. Consumer decides drop-vs-retry. No internal queue grows unbounded. |
| R4 | Aimux downstream wiring discovers signature mismatch post-tag | MEDIUM | D5 contingency — patch v0.23.1 with refined signature (still additive). Verified pre-tag via test consumer in PR description. |

No CRITICAL risks. No unresolved unknowns.

## Constitution Compliance

| Principle | Status | Note |
|-----------|--------|------|
| Library-first | PASS | Searched, none applicable; stdlib choice documented |
| MCP spec compliance | N/A | Internal proxy plumbing, no MCP wire-format change |
| Multi-tenant safety | PASS | No shared state across tenants — per-`ResilientClient` callback |
| Backward compatibility | PASS | Additive only; zero source change for non-adopters (US3) |
| Test coverage | PASS | NFR-5 — 2 regression tests required pre-merge; cross-platform CI matrix |
| Documentation | PASS | NFR-6 — AGENTS.md v0.23.0 entry mandatory |

## Plan Validation Checklist

- [x] Every spec FR (FR-1..7) has implementation in at least one phase
- [x] Every NFR (NFR-1..6) has concrete approach
- [x] Library decisions documented for all components
- [x] File structure matches existing patterns (additive within existing files)
- [x] Phases have clear boundaries — Phase 1 (foundation), Phase 2 (passthrough+tests), Phase 3 (release)
- [x] Constitution principles respected
- [x] Phase 0.5 parallelism analysis ran (SocratiCode green, file-graph fallback to grep+import-chain)
- [x] Phase 2 has Concurrent Work Directives subsection (T2 || T3 || T4)
- [x] D1, D4, D5 (IRREVERSIBLE) have Deep Contingency Branches OR documented Light contingency where appropriate
- [x] D2, D3 have Medium / Light contingency blocks
- [x] No `[P]` marker without graph-backed provenance (T2/T3/T4 [P] all PARALLEL-WITH evidence cited)
- [x] Phase 0 Reversibility Audit emitted PASS
