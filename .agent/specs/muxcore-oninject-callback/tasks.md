# Tasks: muxcore-oninject-callback

**Spec:** `.agent/specs/muxcore-oninject-callback/spec.md`
**Plan:** `.agent/specs/muxcore-oninject-callback/plan.md`
**Mode:** Legacy fallback (no `open_crs` in spec.md — slug-only layout per FR-11)
**TDD:** Enabled (default)
**Generated:** 2026-04-28

## User-value anchors (Phase 0 Value-Sequence Planner)

| Story | Priority | Anchor |
|-------|----------|--------|
| US3 | P1 | When US3 is done, mcp-launcher / engram developers run `go get muxcore@v0.23.0 && go build ./...` and observe byte-identical runtime to v0.22.1. |
| US2 | P2 | When US2 is done, a muxcore consumer calls `engine.New(Config{OnInject: cb})` and pushes telemetry frames via the returned closure without managing reconnect. |
| US1 | P1 | When US1 is done, aimux's `IPCSink.SendFunc` is wired in ~20 LOC and shim log entries reach the daemon's `LogIngester` via `notifications/aimux/log_forward` over the existing IPC channel. |

US3 + US2 deliver mcp-mux-side value (the API surface). US1 is unblocked downstream (aimux PR is out-of-scope for this repo arc — it consumes the API once published).

`PHASE_0_COMPLETE`.

## Phase 1: API surface — owner package (delivers US3 + US2 foundation)

Independent Test: `go test ./muxcore/owner/... -run TestResilientClient_OnInject` PASS on Linux/macOS/Windows.

### T001 — RED: write `TestResilientClient_OnInject_DeliversFrames`

**File:** `muxcore/owner/resilient_client_test.go` (modify, add new test func)
**Description:** Add a failing regression test that asserts: (a) `OnInject` callback fires exactly once after initial handshake, (b) calling `inject(frame)` enqueues the frame via `msgFromCC`, (c) the frame reaches the IPC reader on the server side, (d) a subsequent CC request still proxies normally.
**AC:**
- Test compiles but fails with "OnInject not yet defined" or equivalent
- Uses existing `startEchoIPCServer` + `RunResilientClient` helper pattern from this test file
- Cross-platform — no goroutine ordering assumptions; uses sync primitives for arrival check
**so that** the test guards FR-1 (callback field), FR-2 (msgFromCC delivery path), FR-4 (single-fire after handshake) before implementation lands.

### T002 — RED: write `TestResilientClient_OnInject_BufferFull`

**File:** `muxcore/owner/resilient_client_test.go` (modify)
**Description:** Add a failing regression test that asserts: (a) when `msgFromCC` is saturated (1000-deep buffer), `inject(frame)` returns `ErrInjectFull` without blocking, (b) the call returns within 1ms (NFR-1 non-blocking guarantee).
**AC:**
- Test compiles but fails with "ErrInjectFull undefined" or "OnInject not yet defined"
- Uses an IPC server that does NOT drain (block reads) so the buffer fills deterministically
- Asserts both error identity (`errors.Is(err, ErrInjectFull)`) and elapsed time (<1ms via `time.Now()` deltas)
**so that** FR-3 (sentinel errors) and NFR-1 (non-blocking) are guarded before implementation.

### T003 — GREEN: implement `OnInject` field + sentinel errors + single-fire closure

**File:** `muxcore/owner/resilient_client.go` (modify)
**Description:** Implement the production code:
1. Add `OnInject func(inject func([]byte) error)` to `ResilientClientConfig`
2. Export `var ErrInjectFull = errors.New("muxcore: inject buffer full")` and `var ErrInjectClosed = errors.New("muxcore: inject channel closed")`
3. In `RunResilientClient` after the initial token handshake completes, build the closure:
   - `inject := func(b []byte) error { ... }` with `closed atomic.Bool` check + `select { case rc.msgFromCC <- b: default: return ErrInjectFull }`
4. Invoke `cfg.OnInject(inject)` exactly once via `sync.Once` (single-fire — FR-4)
5. On proxy exit (`runProxy` returns), set `closed.Store(true)` so subsequent `inject(b)` calls return `ErrInjectClosed` without blocking (FR-5)
**AC:**
- T001 + T002 PASS
- `go vet ./muxcore/owner/...` clean
- `go test ./muxcore/owner/...` full suite green (zero regressions in pre-existing tests with `OnInject == nil`)
- Verify zero-allocation path: `OnInject == nil` skips the `sync.Once` + closure construction entirely
- **NFR-3 log markers emitted** at `Logger.Printf` level: `proxy.inject.armed` (post-handshake single-fire), `proxy.inject.delivered` (after `msgFromCC <-` succeeds), `proxy.inject.dropped reason=full|closed` (sentinel return path)
**IF-WRONG:** if T002 fails because `select-default` returns `ErrInjectFull` BEFORE the buffer truly saturates (off-by-one in test harness), debug the harness, NOT the production code. Confirm 1000 unbuffered sends are queued via channel-len check before claiming the bug is in `inject()`.
  → if real bug in production: re-tag PARTIALLY REVERSIBLE — buffer behavior may need explicit drain coordination
  → if test harness only: amend T002 inline, no IRREVERSIBLE pivot
**so that** US2 (consumer pushes telemetry) and US3 (zero source change for non-adopters) become deliverable.

## Phase 2: Engine passthrough + docs (parallel) (delivers US3 fully)

Independent Test: `go build ./...` clean + downstream non-adopting consumer (mcp-launcher mock) bumps `muxcore@v0.23.0-rc` and observes zero source change.

### T004 — Engine passthrough

**File:** `muxcore/engine/engine.go` (modify, lines ~78-139 + ~381)
**Description:** Add `OnInject func(inject func([]byte) error)` to `engine.Config`. In `runProxy` at line 381, wire `cfg.OnInject` → `owner.ResilientClientConfig{OnInject: cfg.OnInject, ...}`.
**AC:**
- `go build ./muxcore/engine/...` clean
- `go vet ./muxcore/engine/...` clean
- `go test ./muxcore/engine/...` full suite green
- When `engine.Config.OnInject == nil`, owner-side `ResilientClientConfig.OnInject` is also nil — byte-identical pre-v0.23 behavior (US3)
**PARALLEL-WITH:** T005 — graph evidence: T004 modifies `muxcore/engine/engine.go`; T005 modifies `AGENTS.md`. Disjoint file set. No shared symbol mutations. (`grep "github.com/thebtf/mcp-mux/muxcore/owner" muxcore/engine/*.go` confirms one import line; T004 does not touch `owner` package.)
**so that** US2 (engine consumer can `engine.New(Config{OnInject: cb})`) and US3 (non-adopters see no change) both unlock.

### T005 [P] — AGENTS.md v0.23.0 release entry

**File:** `AGENTS.md` (modify, append v0.23.0 section under `## muxcore Library API`)
**Description:** Add v0.23.0 entry mirroring v0.22.0 entry style:
- **Header:** `### v0.23.0 — engine.Config.OnInject for fire-and-forget IPC frame injection (#107)`
- **Breaking changes table:** None (additive only)
- **New API table:** `engine.Config.OnInject` + `ResilientClientConfig.OnInject` + `ErrInjectFull` + `ErrInjectClosed`
- **Migration:** "Existing consumers (aimux ≤v0.22.0, engram, mcp-launcher) require zero source change. `OnInject == nil` is the zero value."
- **Aimux adoption snippet:** template ~20 LOC for `cmd/aimux/shim.go` per issue #107 body
- **Regression tests added:** `TestResilientClient_OnInject_DeliversFrames`, `TestResilientClient_OnInject_BufferFull`
- **Forward link:** GitHub release URL placeholder (filled in T009)
**AC:**
- AGENTS.md renders without lint errors
- v0.23.0 entry follows the exact section layout of v0.22.0 entry
- Upgrade instruction line elsewhere in AGENTS.md (top of muxcore section) bumped to v0.23.0
**PARALLEL-WITH:** T004 (above)
**so that** non-adopting consumers can read the migration note before bumping.

### G001 — GATE: code-review lite + full muxcore suite

**File:** N/A (verification only)
**Description:** RUN `go build ./... && go vet ./... && go test ./muxcore/... -timeout 120s` on master + Linux/macOS/Windows CI matrix locally where reachable.
- CHECK: zero failures, zero vet warnings, all 2 new tests pass
- ENFORCE: if any pre-existing test regresses, BLOCK and triage — production code MUST stay zero-impact for `OnInject == nil`
- RESOLVE: feed regressions back into T003 fix loop
- SAVE: gate result inline in PR description
**AC:**
- All 4 conditions PASS
- PR description includes test summary table
**so that** US3 (zero source change) is empirically verified before tagging.

## Phase 3: Release (delivers US1 unblocking)

Independent Test: `gh release view muxcore/v0.23.0 --repo thebtf/mcp-mux` returns release page; `go get github.com/thebtf/mcp-mux/muxcore@v0.23.0` works from a fresh clone of mcp-launcher.

### T006 — Open PR + run pr-review

**File:** N/A (git/gh action)
**Description:** Push feature branch (e.g., `feat/oninject-callback`); `gh pr create --base master --title "muxcore: engine.Config.OnInject — fire-and-forget IPC frame injection (#107)" --body <release-notes-style summary>`. Trigger pr-review (CodeRabbit + Gemini per repo config).
**AC:**
- PR open against master
- CI green on PR HEAD
- CodeRabbit + Gemini review feedback addressed (any HIGH/CRIT must be fixed before merge)
**IF-WRONG:** if review surfaces an API ergonomic concern (e.g., closure signature should accept context for cancellation) — STOP, do NOT improvise. Route as `nvmd-specify --amend muxcore-oninject-callback` per G13 (do not silently change the public API mid-merge).
**so that** the change ships through standard repo gating.

### T007 — Merge + tag muxcore/v0.23.0 + v0.23.0

**File:** N/A (git/gh action)
**Description:** After PR approval and CI green:
1. `gh pr merge <PR-number> --squash --auto`
2. `git fetch origin && git checkout master && git pull --ff-only`
3. `git tag muxcore/v0.23.0 <merge-commit-sha>`
4. `git tag v0.23.0 <merge-commit-sha>`
5. `git push --follow-tags origin master`
**AC:**
- Both tags visible on `git ls-remote --tags origin`
- master HEAD = merge-commit-sha
**IF-WRONG:** if `git push --follow-tags` is rejected (tag conflict — extremely rare for v0.23.0) — STOP. `git ls-remote --tags origin | grep v0.23` to inspect remote state. Do NOT force-push tags.
**so that** the SemVer release is canonical and pull-only.

### T008 — Publish GitHub releases

**File:** N/A (gh release action)
**Description:**
1. `gh release create muxcore/v0.23.0 --title "muxcore/v0.23.0 — engine.Config.OnInject for fire-and-forget IPC frame injection (#107)" --notes-file .agent/release-notes/muxcore-v0.23.0.md`
2. `gh release create v0.23.0 --title "v0.23.0" --notes "Wraps muxcore/v0.23.0 — see release notes there."`
3. Release notes file: write `.agent/release-notes/muxcore-v0.23.0.md` lifted from AGENTS.md v0.23.0 entry.
**AC:**
- Both releases live and visible at https://github.com/thebtf/mcp-mux/releases
- Latest = v0.23.0 (replacing v0.22.1)
**so that** downstream consumers (aimux, engram, mcp-launcher) can bump muxcore@v0.23.0 with full release notes context.

### T009 — Close issue #107 with completion comment

**File:** N/A (gh issue action)
**Description:** `gh issue close 107 --repo thebtf/mcp-mux --comment "<body>"` where body summarizes:
- API landed: `engine.Config.OnInject` + `ResilientClientConfig.OnInject` + `ErrInjectFull` + `ErrInjectClosed`
- Tag: `muxcore/v0.23.0`
- PR: link to merge commit
- Aimux migration: 20-LOC template referenced
- Cross-link to engram#178 for downstream consumer readiness
**AC:**
- Issue #107 state = CLOSED with stateReason = COMPLETED
- Comment posted with all five items above
**so that** the source-side close lifecycle (per issues-tracker discipline) closes the loop on the original request.

## Concurrent Execution Map

```
T001 ─┐
       ├─→ T003 ─→ T004 ─┬─→ G001 ─→ T006 ─→ T007 ─→ T008 ─→ T009
T002 ─┘                   │
                          └─[P]─ T005 (AGENTS.md docs, parallel with T004)
```

Notes:
- T001 → T002 sequential because both modify the same file (`resilient_client_test.go`); merge-conflict risk if parallel (file-level coupling, not symbol-level).
- T003 sequential foundation for T004 (engine.Config.OnInject depends on `owner.ResilientClientConfig.OnInject` field existing).
- T004 || T005 — disjoint file sets, no shared symbols.
- G001 gates Phase 1+2 deliverables before release.
- Phase 3 sequential — release flow is linear by nature.

## Dependencies

- T002 depends on T001 (same file)
- T003 depends on T001, T002 (RED tests must exist first per TDD)
- T004 depends on T003 (engine config references owner field)
- T005 depends on T003 (docs reference final API names)
- G001 depends on T004, T005
- T006 depends on G001 (gate must pass before PR open)
- T007 depends on T006 (merge after review)
- T008 depends on T007 (release after tag)
- T009 depends on T008 (close issue after release published)

## Suggested MVP scope

**Minimum:** T001 → T002 → T003 + G001 — delivers the API surface in owner package only. Engine passthrough (T004) + docs (T005) + release (Phase 3) follow.

**Recommended ship:** all 9 tasks + G001 — full PR landing v0.23.0 in one merge.
