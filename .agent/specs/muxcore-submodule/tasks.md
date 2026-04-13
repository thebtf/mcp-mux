# Tasks: muxcore Sub-Module Extraction

**Spec:** .agent/specs/muxcore-submodule/spec.md
**Architecture:** .agent/specs/muxcore-library/architecture-monorepo.md
**Generated:** 2026-04-14

## Phase 1: Move + Sub-Module

- [ ] T001 [US2] Move internal/muxcore/ to muxcore/ at repo root
  AC: `git mv internal/muxcore/* muxcore/` · 16 directories in muxcore/ · internal/muxcore/ empty/deleted · all package declarations unchanged · swap body→return null ⇒ tests MUST fail

- [ ] T002 [US1] Create muxcore/go.mod with sub-module declaration
  AC: muxcore/go.mod exists with `module github.com/thebtf/mcp-mux/muxcore` · go directive matches root · requires: suture/v4, golang.org/x/sys, google/uuid · go.sum generated via `go mod tidy` · swap body→return null ⇒ tests MUST fail

- [ ] T003 [US1] Update all intra-muxcore imports from internal/muxcore/X to muxcore paths
  AC: zero occurrences of `internal/muxcore` in muxcore/**/*.go · all imports use `github.com/thebtf/mcp-mux/muxcore/X` · go build ./muxcore/... passes · swap body→return null ⇒ tests MUST fail

- [ ] T004 [US2] Update root module: go.mod require + replace, cmd/ and mcpserver/ imports
  AC: root go.mod has `require github.com/thebtf/mcp-mux/muxcore` + `replace => ./muxcore` · cmd/mcp-mux/*.go imports use `github.com/thebtf/mcp-mux/muxcore/X` · internal/mcpserver/*.go imports updated · go build ./cmd/mcp-mux/ passes · swap body→return null ⇒ tests MUST fail

- [ ] T005 [US2] Fix testdata paths and run full regression
  AC: testProjectRoot() helper works from muxcore/ location · all mock_server.go references correct · go test ./... -count 1 passes (both modules) · go vet ./... clean · swap body→return null ⇒ tests MUST fail

- [ ] G001 VERIFY Phase 1 (T001–T005)
  RUN: go build ./... && go test ./... -count 1 from repo root
  CHECK: zero files in internal/muxcore/. All imports clean. Both modules build+test.
  ENFORCE: Zero stubs. Binary identical behavior.
  RESOLVE: Fix ALL findings before marking [x].

---

## Phase 2: Tag + Verify Consumer

- [ ] T006 Commit, push, create PR
  AC: worktree branch · PR created · CI passes

- [ ] T007 Tag muxcore/v0.15.0 and verify external go get
  AC: `git tag muxcore/v0.15.0` · pushed to origin · from a temp module: `go get github.com/thebtf/mcp-mux/muxcore@v0.15.0` succeeds · import compiles

- [ ] T008 Update CONTINUITY.md + roadmap
  AC: CONTINUITY reflects sub-module extraction · version notes

- [ ] G002 VERIFY Phase 2 (T006–T008)
  RUN: external go get test. mcp-mux binary smoke test.
  CHECK: consumers can import. Binary works.

## Dependencies
- T001 → T002 → T003 → T004 → T005 (sequential — each depends on previous)
- T006 after G001
- T007 after T006 merged
