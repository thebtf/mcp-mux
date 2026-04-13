# Feature: muxcore Phase 4-6 — Core Migration, Engine, Cleanup

**Slug:** muxcore-library (supplementary spec for Phases 4-6)
**Created:** 2026-04-13
**Status:** Draft
**Author:** AI Agent (reviewed by user)
**Parent:** .agent/specs/muxcore-library/spec.md

> **Provenance:** Specified by claude-opus-4-6 on 2026-04-13.
> Evidence from: Phase 0 exploration of owner.go (2000 LOC), daemon/ (895 LOC),
> dependency graph analysis (zero circular deps confirmed), existing 12 muxcore packages.
> Confidence: Dependency graph VERIFIED via grep analysis. Migration safety VERIFIED.

## Overview

Complete the muxcore library extraction by moving the remaining core packages
(owner, daemon, client) into internal/muxcore/, creating the engine package
for daemon/client/proxy modes, and cleaning up old packages.

## Context

**Already extracted (T001-T018):** 12 packages in internal/muxcore/:
busy, classify, control, ipc, jsonrpc, listchanged, procgroup, progress, remap, serverid, session, snapshot.

**Remaining in old locations:**
- `internal/mux/owner.go` (2000 LOC) — core multiplexer routing logic
- `internal/mux/client.go` + `resilient_client.go` — CC-to-owner connection
- `internal/daemon/` (895 LOC) — global daemon managing multiple owners
- `internal/upstream/` — process management (now wraps procgroup)
- `internal/mcpserver/` — MCP control plane tools

**Key finding from Phase 0: Zero circular dependencies.**
- `daemon` imports `mux` (creates Owner) but `mux` does NOT import `daemon`
- `mcpserver` imports `mux` and `daemon` but neither imports `mcpserver`
- Migration order: owner → daemon → client → engine (each step safe)

## Functional Requirements

### FR-1: Owner Package Migration
Move internal/mux/owner.go and related files to internal/muxcore/owner/.
Owner struct, OwnerConfig, NewOwner, NewOwnerFromSnapshot, and all routing
methods must work from the new location. Type aliases in internal/mux/ for
backward compatibility until all callers updated.

### FR-2: Daemon Package Migration
Move internal/daemon/ to internal/muxcore/daemon/. Only one external caller
(cmd/mcp-mux/daemon.go) — trivial import path change.

### FR-3: Client Package Extraction
Move client.go and resilient_client.go to internal/muxcore/client/.
RunClient and RunResilientClient consumed by cmd/mcp-mux/main.go.

### FR-4: Upstream Package Migration
Move internal/upstream/ to internal/muxcore/upstream/. upstream.Process stays
as a separate package (distinct stdio concern from procgroup.Process lifecycle).

### FR-7: mcpserver Stays In Place
internal/mcpserver/ is mcp-mux-specific (control plane tools). It is NOT part
of the muxcore library. Its imports of mux.Owner will use type aliases until
cleanup phase updates them to muxcore/owner directly.

### FR-5: Engine Package
New internal/muxcore/engine/ providing unified entry point:
- `engine.Run(config)` — detects mode (daemon/client/proxy) and runs
- Config struct with Command, Args, BaseDir, Mode options

### FR-6: Old Package Cleanup
Delete internal/mux/ (replaced by muxcore/owner + aliases), internal/daemon/
(replaced by muxcore/daemon), internal/upstream/ (absorbed).

## Non-Functional Requirements

### NFR-1: Zero Regression
Every intermediate step must pass `go build ./...` and `go test ./... -count 1`.
No test can be deleted or skipped. Feature parity verified.

### NFR-2: Git History Preservation
Use `git mv` for file moves. Squash merges in PR acceptable but individual
commits should use `git mv` for traceability.

### NFR-3: Incremental Migration
Each task produces a buildable, testable intermediate state. No "big bang"
multi-package moves.

## User Stories

### US1: Owner Migration (P1)
**As a** muxcore consumer, **I want** Owner in muxcore/owner/,
**so that** I can import the multiplexer without pulling the entire mcp-mux binary.

**Acceptance Criteria:**
- [ ] internal/muxcore/owner/ contains owner.go + related files
- [ ] internal/mux/ contains only type alias shims
- [ ] All 13+ existing owner tests pass from new location
- [ ] daemon, cmd, mcpserver compile with no changes (via aliases)

### US2: Daemon Migration (P1)
**As a** muxcore consumer, **I want** Daemon in muxcore/daemon/,
**so that** daemon lifecycle management is part of the library.

**Acceptance Criteria:**
- [ ] internal/muxcore/daemon/ contains all daemon files
- [ ] cmd/mcp-mux/daemon.go updated with new import
- [ ] All daemon tests pass from new location

### US3: Engine API (P2)
**As a** MCP server author, **I want** a single `engine.Run(config)` entry point,
**so that** I can add muxcore to my server with minimal integration code.

**Acceptance Criteria:**
- [ ] engine.Run detects and runs correct mode (daemon/client)
- [ ] Config struct is the only public API surface
- [ ] Integration test: spawn engine in daemon mode, connect client, exchange messages

## Edge Cases

- Owner tests reference `../../testdata/mock_server.go` via relative path — fix with `testProjectRoot()` helper using `runtime.Caller(0)` to compute absolute path
- Snapshot deserialization references mux.Version — must be accessible from muxcore/owner
- Session type aliases (Session = session.Session) may cause confusion with `go doc`

## Out of Scope

- Standalone muxcore repository (deferred to after API freeze)
- Consumer playbooks (aimux, engram integration guides)
- Proxy mode implementation in engine (deferred to post-extraction)

## Dependencies

- Phase 1-3 muxcore packages (DONE)
- procgroup integration (T014 DONE)
- Session/busy/progress/listchanged extraction (T015-T018 DONE)

## Success Criteria

- [ ] All code in internal/muxcore/ — no business logic in internal/mux/ or internal/daemon/
- [ ] internal/mux/ contains only re-export aliases (or is deleted)
- [ ] `go test ./... -count 1` passes with 0 failures
- [ ] `go build ./...` clean
- [ ] mcp-mux binary functions identically (status, restart, progress reporter)

## Clarifications

### Session 2026-04-13

| # | Category | Question | Resolution | Date |
|---|----------|----------|------------|------|
| C1 | Integration | What happens to mcpserver? | Stays in internal/mcpserver/ — mcp-mux-specific, not muxcore | 2026-04-13 |
| C2 | Edge Cases | How to fix testdata relative paths? | testProjectRoot() helper via runtime.Caller(0) | 2026-04-13 |
| C3 | FR-4 | Move upstream or absorb into procgroup? | Move as-is — distinct stdio concern | 2026-04-13 |

## Migration Order (from Phase 0 findings)

1. **owner.go → muxcore/owner/** — largest move, no circular deps
2. **daemon/ → muxcore/daemon/** — trivial (1 external caller)
3. **client + resilient_client → muxcore/client/** — 1 external caller
4. **upstream/ → muxcore/upstream/** — already wraps procgroup
5. **engine/ creation** — new code on top of migrated packages
6. **cleanup** — delete old packages, remove aliases
