# Tasks: Inflight Request Observability

**Spec:** .agent/specs/inflight-observability/spec.md
**Plan:** .agent/specs/inflight-observability/plan.md
**Generated:** 2026-04-07

## Phase 1: Inflight Tracker

- [x] T001 Add InflightRequest struct and inflightTracker sync.Map to Owner in internal/mux/owner.go
  AC: struct has Method, Tool, SessionID, StartTime fields · sync.Map field on Owner · go build passes · swap body→return null ⇒ tests MUST fail

- [x] T002 Track inflight requests in handleDownstreamMessage when forwarding to upstream in internal/mux/owner.go
  AC: stores InflightRequest in inflightTracker keyed by remapped ID · extracts tool name from tools/call params.name · session ID from s.ID · StartTime = time.Now() · swap body→return null ⇒ tests MUST fail

- [x] T003 Untrack inflight requests in handleUpstreamMessage on response arrival in internal/mux/owner.go
  AC: deletes from inflightTracker when response received (same place as pendingRequests.Add(-1)) · also untrack in drainInflightRequests · swap body→return null ⇒ tests MUST fail

- [x] T004 Expose inflight array in Owner.Status() in internal/mux/owner.go
  AC: Status() includes "inflight" key with []map[string]any when tracker non-empty · each entry has method, tool, session, started_at (RFC3339), elapsed_seconds · omitted when empty · swap body→return null ⇒ tests MUST fail

- [x] G001 VERIFY Phase 1 (T001–T004) — BLOCKED until T001–T004 all [x]
  RUN: go test ./... -count 1. Call Skill("code-review", "lite") on owner.go changes.
  CHECK: Confirm AC met for each task. Anti-stub check.
  ENFORCE: Zero stubs. Zero TODOs.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Inflight data tracked and exposed in Status().

## Phase 2: Tests + Verification

- [x] T005 Write tests for inflight tracking in internal/mux/mux_test.go or coverage_test.go
  AC: test track → Status shows inflight → untrack → empty · test tools/call extracts tool name · test malformed params → empty tool · test concurrent track/untrack safe · 4+ test cases · swap body→return null ⇒ tests MUST fail

- [x] T006 Full test suite regression check
  AC: go test ./... -count 1 passes · all packages green

- [x] T007 Build, deploy, smoke test with real server
  AC: mux_list --verbose with serena pending shows method + tool + elapsed · go build succeeds

- [x] G002 VERIFY Phase 2 (T005–T007) — BLOCKED until T005–T007 all [x]
  RUN: go test ./... -count 1. Live smoke test.
  CHECK: All tests pass. Smoke test confirms inflight visible.
  RESOLVE: Fix ALL findings before marking [x].

---

**Checkpoint:** Inflight observability production-ready.

## Phase 3: Release

- [x] T008 Commit, push, tag release
  AC: PR created · CI green · merged · tagged

## Dependencies

- T001 → T002 → T003 → T004 (sequential, same file)
- T005 after T004 (needs implementation to test)

## Execution Strategy

- **MVP:** Phase 1 (T001-T004) — inflight data available
- **Commit strategy:** Single commit for Phase 1, one for tests
- **Estimated:** 8 tasks (4 impl + 2 gates + 1 test + 1 release)
