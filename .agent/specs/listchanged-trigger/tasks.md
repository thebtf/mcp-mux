# Tasks: listChanged Trigger Mechanism

**Spec:** .agent/specs/listchanged-trigger/spec.md
**Generated:** 2026-04-14

## Phase 1: Injection Wiring

- [ ] T001 [US2] Wire InjectInitializeCapability into cacheResponse in internal/muxcore/owner/owner.go
  AC: in cacheResponse("initialize"), after storing raw bytes, call listchanged.InjectInitializeCapability · if injection succeeds, replace cached bytes with injected version · remove the NOTE comment at line ~1997 · go build passes · swap body→return null ⇒ tests MUST fail

- [ ] T002 [US1] Implement broadcastListChanged helper in internal/muxcore/owner/owner.go
  AC: new method broadcastListChanged() sends notifications/tools/list_changed + notifications/prompts/list_changed + notifications/resources/list_changed to ALL connected sessions via WriteRaw · skips sessions with errors · logs count of notified sessions · swap body→return null ⇒ tests MUST fail

- [ ] T003 [US1] Call broadcastListChanged on upstream restart in owner.go
  AC: when upstream dies and new upstream spawns (suture restart path or SpawnUpstreamBackground success), call broadcastListChanged() · also invalidate cached tools/prompts/resources lists so next request gets fresh data · swap body→return null ⇒ tests MUST fail

- [ ] G001 VERIFY Phase 1 (T001–T003)
  RUN: go test ./internal/muxcore/owner/ -count 1 -v + go test ./... -count 1
  CHECK: cached init has listChanged:true. Broadcast fires on restart.
  ENFORCE: Zero stubs. Remove the NOTE comment.
  RESOLVE: Fix ALL findings before marking [x].

---

## Phase 2: Tests + Release

- [ ] T004 [P] Write listChanged wiring tests in internal/muxcore/owner/listchanged_inject_test.go
  AC: test cached init has listChanged:true after cacheResponse · test broadcastListChanged sends 3 notifications to each session · test broadcast with 0 sessions is no-op · test injection idempotent (already true) · 4+ test cases · swap body→return null ⇒ tests MUST fail

- [ ] T005 Full regression + deploy
  AC: go test ./... -count 1 passes · go vet clean · go build clean · mcp-mux binary deployed

- [ ] T006 Update roadmap, CONTINUITY, version
  AC: roadmap Sprint 2 marked done · CONTINUITY updated · version tagged

- [ ] G002 VERIFY Phase 2 (T004–T006)
  RUN: go test ./... -count 1. Live test: mux_restart → CC refreshes tools.
  CHECK: all tests pass, roadmap updated, version tagged.
  RESOLVE: Fix ALL findings before marking [x].

## Dependencies
- T001 before T003 (inject before broadcast)
- T002 before T003 (helper before caller)
- T004 can run alongside T001-T003

## Execution Strategy
- MVP: T001-T003 (injection + broadcast)
- Total: 6 tasks + 2 gates
