# Feature: muxcore Sub-Module Extraction

**Slug:** muxcore-submodule
**Created:** 2026-04-14
**Status:** Draft
**Author:** AI Agent (reviewed by user)

> **Provenance:** Specified by claude-opus-4-6 on 2026-04-14.
> Evidence from: architecture-monorepo.md (4 ADRs), go.mod analysis, import graph verified via grep.
> Confidence: VERIFIED — all imports mapped, Go sub-module pattern confirmed.

## Overview

Move `internal/muxcore/` to `muxcore/` at repo root as a Go sub-module with its own
`go.mod`. This makes the library importable by external consumers (aimux, engram) via
`go get github.com/thebtf/mcp-mux/muxcore` while keeping everything in one repository.

## Context

muxcore has 16 packages with zero dependencies on non-muxcore code. But `internal/`
prefix prevents external import (Go compiler enforces this). Moving to a top-level
sub-module is a mechanical path change — no logic changes, no API changes.

**Architecture:** `.agent/specs/muxcore-library/architecture-monorepo.md`

## Functional Requirements

### FR-1: Sub-Module at muxcore/
Create `muxcore/go.mod` with module path `github.com/thebtf/mcp-mux/muxcore`.
All 16 packages accessible as `muxcore/engine`, `muxcore/owner`, etc.

### FR-2: Root Module Depends on Sub-Module
Root `go.mod` adds `require github.com/thebtf/mcp-mux/muxcore` with
`replace => ./muxcore` for local development. `cmd/mcp-mux/` and
`internal/mcpserver/` import from `github.com/thebtf/mcp-mux/muxcore/...`.

### FR-3: Internal Import Paths Updated
All intra-muxcore imports change from `internal/muxcore/X` to relative
sub-module paths. No `internal/` prefix remains in any muxcore package.

### FR-4: External Consumers Can Import
`go get github.com/thebtf/mcp-mux/muxcore@latest` works. Consumer can
`import "github.com/thebtf/mcp-mux/muxcore/engine"` and call `engine.New()`.

### FR-5: Testdata Accessible
Test files in muxcore packages reference `testdata/` at repo root. After
move, tests must still find fixtures (via testProjectRoot helper or
embedded testdata).

## Non-Functional Requirements

### NFR-1: Zero Behavior Change
Binary output identical. All existing tests pass. mcp-mux CLI works.

### NFR-2: Single CI Run
`go test ./...` from root with `replace` directive tests both modules.
No separate CI job needed.

### NFR-3: Tagging Convention
Sub-module tags: `muxcore/v0.15.0`. Root tags: `v0.15.1+`.

## User Stories

### US1: External Consumer Imports muxcore (P1)
**As a** Go developer building an MCP server, **I want** to
`import "github.com/thebtf/mcp-mux/muxcore/engine"`,
**so that** my server gets multiplexing without installing mcp-mux binary.

**Acceptance Criteria:**
- [ ] `go get github.com/thebtf/mcp-mux/muxcore` succeeds from a clean module
- [ ] `engine.New(Config{Name:"test", Handler:h}).Run(ctx)` compiles
- [ ] No `internal/` in any muxcore import path

### US2: mcp-mux Binary Unchanged (P1)
**As a** mcp-mux user, **I want** the binary to work identically after refactor,
**so that** this is a transparent infrastructure change.

**Acceptance Criteria:**
- [ ] `go build ./cmd/mcp-mux/` succeeds
- [ ] `mcp-mux status` returns correct data
- [ ] All 18 test packages pass
- [ ] Binary size delta < 1%

## Edge Cases

- `go test ./...` from repo root must test BOTH modules (root + sub-module)
- `testdata/mock_server.go` referenced via relative paths — testProjectRoot must work from new location
- `replace` directive only works locally — CI must handle published version for cross-module deps
- Consumers on Go < 1.18 cannot use sub-modules (minimum Go version: 1.18)

## Out of Scope

- Separate repository for muxcore (deferred)
- API stability guarantees (engine is the only intended public surface)
- Changelog or migration guide for consumers (playbooks already exist)

## Dependencies

- All 16 muxcore packages already extracted (DONE — v0.14.0)
- listChanged wired (DONE — v0.15.0)

## Success Criteria

- [ ] `muxcore/go.mod` exists with correct module path
- [ ] Zero files under `internal/muxcore/` (directory deleted)
- [ ] `go build ./...` and `go test ./...` pass from repo root
- [ ] External `go get` works after tag `muxcore/v0.15.0` pushed
- [ ] aimux or engram can compile with muxcore/engine import
