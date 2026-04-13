# Tasks: Synthetic Progress Reporter

**Spec:** .agent/specs/synthetic-progress-reporter/spec.md
**Plan:** .agent/specs/synthetic-progress-reporter/plan.md
**Generated:** 2026-04-13

## Phase 1: Capability Parsing

- [x] T001 Add ParseProgressInterval() in internal/classify/classify.go
  AC: parses x-mux.progressInterval from initialize JSON · returns seconds (int) · range 1-60 clamped · 0 when absent · follows ParseIdleTimeout pattern exactly · swap body→return null ⇒ tests MUST fail

- [x] T002 [P] Write tests for ParseProgressInterval in internal/classify/progress_interval_test.go
  AC: 6+ test cases: direct x-mux path, experimental path, absent → 0, negative → 0, cap at 60, invalid JSON → 0 · mirrors idle_timeout_test.go structure · swap body→return null ⇒ tests MUST fail

- [x] G001 VERIFY Phase 1 (T001–T002) — 8/8 tests PASS, shipped in v0.11.2
  RUN: go test ./internal/classify/ -count 1 -v. Call Skill("code-review", "lite") on classify.go changes.
  CHECK: Confirm AC met for each task. Anti-stub check on ParseProgressInterval.
  ENFORCE: Zero stubs. Zero TODOs. Every parameter influences output.
  RESOLVE: Fix ALL findings before marking this gate [x].

---

**Checkpoint:** Capability parsing ready — progressInterval extractable from init response.

## Phase 2: Core Reporter — US1 Visible Tool Call Progress (P1)

**Goal:** Reporter goroutine emits synthetic progress for inflight tools/call requests.
**Independent Test:** Start Owner with mock upstream, send tools/call, wait >5s, verify session receives synthetic notifications/progress with tool name + elapsed time.

- [x] T003 [US1] Implement buildSyntheticProgress() in internal/mux/progress_reporter.go
  AC: returns valid JSON-RPC notification bytes · includes progressToken, progress (int), message ("{tool}: {N}s elapsed") · no total field · progress = elapsed seconds rounded to interval · swap body→return null ⇒ tests MUST fail

- [x] T004 [US1] Implement runProgressReporter() goroutine in internal/mux/progress_reporter.go
  AC: ticks every Owner.progressInterval · scans inflightTracker via sync.Map.Range · skips entries younger than interval · looks up tokens via requestToTokens · sends synthetic notification via session.WriteRaw · stops on ctx.Done() · no-op when inflightTracker empty · swap body→return null ⇒ tests MUST fail

- [x] T005 [US1] Wire reporter into Owner lifecycle in internal/mux/owner.go
  AC: add progressInterval field (default 5s) to Owner struct · parse from cached initResp via ParseProgressInterval after classification · start `go o.runProgressReporter(ctx)` in Serve() · goroutine exits on Owner shutdown · swap body→return null ⇒ tests MUST fail

- [x] T006 [P] [US1] Write tests for progress reporter in internal/mux/progress_reporter_test.go
  AC: test buildSyntheticProgress output matches wire format (3 cases: with tool, without tool, long elapsed) · test tick emits after threshold · test no-op when no inflight · test goroutine stops on cancel · 6+ test cases · swap body→return null ⇒ tests MUST fail

- [x] G002 VERIFY Phase 2 (T003–T006) — 10/10 PASS, 6 new tests, 296 LOC
  RUN: go test ./internal/mux/ -count 1 -run "Progress" -v. Call Skill("code-review", "lite") on progress_reporter.go + owner.go changes.
  CHECK: Confirm AC met. Independent test: mock owner with inflight request, verify synthetic notification sent after 5s.
  ENFORCE: Zero stubs. Zero TODOs. Reporter goroutine independently testable.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Core reporter works — CC sees tool name + elapsed time for inflight calls.

## Phase 3: Dedup — US3 No False Progress (P2)

**Goal:** Suppress synthetic when upstream sends real progress, resume after silence.
**Independent Test:** Send real notifications/progress for a token, verify synthetic stops. Stop real progress, wait one interval, verify synthetic resumes.

- [x] T007 [US3] Add lastRealProgress tracking to Owner in internal/mux/owner.go
  AC: new field `lastRealProgress map[string]time.Time` protected by progressMu · initialized in NewOwner and NewOwnerFromSnapshot · swap body→return null ⇒ tests MUST fail

- [x] T008 [US3] Implement recordRealProgress() in internal/mux/progress_reporter.go
  AC: sets lastRealProgress[token] = time.Now() under progressMu lock · called from routeProgressNotification() after successful routing · swap body→return null ⇒ tests MUST fail

- [x] T009 [US3] Add dedup check to runProgressReporter tick in internal/mux/progress_reporter.go
  AC: before emitting synthetic, check lastRealProgress[token] · if real progress within current interval → skip · if real progress had `total` field (determinate) → never override with synthetic · swap body→return null ⇒ tests MUST fail

- [x] T010 [US3] Clean up lastRealProgress in clearProgressTokensForRequest() in internal/mux/owner.go
  AC: delete lastRealProgress entries for all tokens being cleared · follows same pattern as progressOwners cleanup · swap body→return null ⇒ tests MUST fail

- [x] T011 [P] [US3] Write dedup tests in internal/mux/progress_reporter_test.go
  AC: test real progress suppresses synthetic · test synthetic resumes after silence · test determinate (with total) permanently suppresses synthetic · test cleanup on request complete · 4+ test cases · swap body→return null ⇒ tests MUST fail

- [x] G003 VERIFY Phase 3 (T007–T011) — 4 dedup tests PASS, 17 total progress tests green
  RUN: go test ./internal/mux/ -count 1 -v. Call Skill("code-review", "lite") on changed files.
  CHECK: Confirm AC met. Independent test: real progress → synthetic stops → silence → synthetic resumes.
  ENFORCE: Zero stubs. Zero TODOs. Dedup logic independently testable.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Dedup works — no false/duplicate progress displayed in CC.

## Phase 4: Configurable Interval — US2 Server-Specific Interval (P2)

**Goal:** MCP servers can declare custom progress interval via x-mux.progressInterval.
**Independent Test:** Mock upstream advertises progressInterval: 10, verify reporter ticks at 10s not 5s.

- [x] T012 [US2] Wire ParseProgressInterval into Owner init path in internal/mux/owner.go
  AC: after cacheResponse("initialize"), call ParseProgressInterval on cached raw · if >0, set o.progressInterval = time.Duration(sec)*time.Second · log at debug level "using x-mux.progressInterval: Ns" · values outside 1-60 clamped with warning · swap body→return null ⇒ tests MUST fail

- [x] T013 [P] [US2] Write interval config test in internal/mux/progress_reporter_test.go
  AC: test owner with progressInterval=10s ticks at 10s · test default 5s when capability absent · 2+ test cases · swap body→return null ⇒ tests MUST fail

- [x] G004 VERIFY Phase 4 (T012–T013) — interval config tests PASS, atomic progressIntervalNs wired end-to-end
  RUN: go test ./internal/mux/ ./internal/classify/ -count 1 -v. Call Skill("code-review", "lite") on changed files.
  CHECK: Confirm AC met. Independent test: verify configurable interval.
  ENFORCE: Zero stubs. Capability parsing integrated end-to-end.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Server-specific interval works — capability parsed and applied.

## Phase 5: Regression + Release

- [x] T014 Full test suite regression check
  AC: go test ./... -count 1 passes · all packages green · zero test regressions · go vet ./... clean

- [x] T015 Build and deploy, verify with real CC session
  AC: go build succeeds · mcp-mux upgrade --restart · trigger long-running tool call (e.g. tavily_search) · CC UI shows "{tool}: Ns elapsed" instead of "Running..."

- [x] T016 Update CONTINUITY.md with v0.12.0 progress
  AC: version updated · Sprint 1 marked done in roadmap.md · done items listed

- [x] T017 Commit, push, create PR
  AC: worktree branch · PR created via gh pr create · CI green

- [x] G005 VERIFY Phase 5 (T014–T017) — v0.12.0 released, all tests green, deployed
  RUN: go test ./... -count 1. Live verification with real CC session.
  CHECK: All success criteria from spec.md met. Full regression clean.
  ENFORCE: Zero stubs. Production-verified.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Synthetic Progress Reporter production-ready.

## Dependencies

- Phase 1 (capability parsing) → Phase 4 (wiring into Owner init)
- Phase 2 (core reporter) → Phase 3 (dedup extends reporter tick)
- Phase 2 (core reporter) → Phase 4 (interval config requires reporter to exist)
- Phase 3 and Phase 4 are independent of each other (can parallelize)
- Phase 5 blocked by ALL previous phases
- T003 → T004 (build function before goroutine)
- T004 → T005 (goroutine before wiring)
- T007 → T008 → T009 (tracking → recording → checking)
- G001 blocks Phase 4's T012 (ParseProgressInterval must exist)
- G002 blocks Phase 3 (reporter must exist for dedup)

## Execution Strategy

- **MVP scope:** Phase 1-2 (reporter works with default 5s, no dedup)
- **Parallel opportunities:** T001||T002, T006 alongside T003-T005, T011 alongside T007-T010, T013 alongside T012, Phase 3||Phase 4
- **Commit strategy:** One commit per phase (capability, reporter, dedup, config, release)
- **Estimated tasks:** 17 total (13 implementation + 4 gates + 1 release meta)
