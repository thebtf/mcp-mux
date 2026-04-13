# Implementation Plan: muxcore Phase 4-6 — Core Migration, Engine, Cleanup

**Spec:** .agent/specs/muxcore-library/spec-phase4-6.md
**Created:** 2026-04-13
**Status:** Draft

> **Provenance:** Planned by claude-opus-4-6 on 2026-04-13.
> Evidence from: spec-phase4-6.md, Phase 0 dependency graph analysis, existing 12 muxcore packages.
> Key decisions by: AI (exploration findings) + user (SpecKit pipeline requirement).
> Confidence: Dependency graph VERIFIED. Migration safety VERIFIED (zero circular deps).

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.25 | Existing project |
| Process management | muxcore/procgroup | Already extracted (T010-T014) |
| Supervisor | thejerf/suture/v4 | Already integrated |
| IPC | Unix domain sockets | Existing pattern |

## Architecture

Migration strategy: **git mv + type alias shim** (REVERSIBLE)

Each package move follows the established pattern from T015 (session extraction):
1. `git mv` files to muxcore/ (preserves history)
2. Update package declaration
3. Export any types needed by external callers
4. Create thin alias file in old location for backward compat
5. Verify build + tests
6. In a later cleanup phase, update all callers to import muxcore/ directly and delete aliases

```
BEFORE:                          AFTER:
internal/mux/owner.go     →     internal/muxcore/owner/owner.go
internal/mux/client.go    →     internal/muxcore/client/client.go
internal/daemon/           →     internal/muxcore/daemon/
internal/upstream/         →     internal/muxcore/upstream/
                                 internal/muxcore/engine/engine.go (NEW)
internal/mux/              →     (thin alias shims only, then deleted)
```

### Reversibility Scoring

| Decision | Tag | Rollback |
|----------|-----|----------|
| git mv files to muxcore/ | **REVERSIBLE** | git mv back, update imports |
| Type alias shims in mux/ | **REVERSIBLE** | Delete shims, restore original files |
| Engine API design | **PARTIALLY REVERSIBLE** | No consumers yet, API can change freely |
| Delete old packages | **REVERSIBLE** | git revert, but must be last step |

## File Structure

```text
internal/muxcore/
  owner/          ← NEW: Owner struct, routing, progress, busy protocol
    owner.go
    busy_protocol.go
    progress_reporter.go
    listchanged_inject.go (if not already in listchanged/)
  client/         ← NEW: RunClient, RunResilientClient
    client.go
    resilient_client.go
  daemon/         ← NEW: Daemon, Reaper, snapshot wrappers
    daemon.go
    reaper.go
    snapshot.go
  upstream/       ← NEW: upstream.Process (wraps procgroup)
    process.go
  engine/         ← NEW: unified entry point
    engine.go
    engine_test.go
  (existing 12 packages unchanged)

internal/mux/     ← becomes alias-only shim, then deleted
```

## Phases

### Phase 4A: Owner Migration (FR-1)
**Scope:** Move owner.go + related files to muxcore/owner/
**Risk:** Medium — largest file, most cross-references
**Strategy:** git mv all mux/ files except session/snapshot aliases → muxcore/owner/

Files to move:
- owner.go (2000 LOC)
- busy_protocol.go (methods on *Owner)
- progress_reporter.go (methods on *Owner)
- client.go + resilient_client.go (separate concern but same package currently)
- All test files for above

Files to keep as shims in internal/mux/:
- session.go (already alias to muxcore/session)
- session_manager.go (already alias to muxcore/session)
- snapshot.go (already alias to muxcore/snapshot)
- NEW: owner_shim.go — type aliases for Owner, OwnerConfig, etc.

**Testdata concern:** Tests reference `../../testdata/mock_server.go`. After moving to
muxcore/owner/, the relative path becomes `../../../../testdata/mock_server.go` (too deep).
Fix: use `runtime.Caller(0)` to compute project root, or pass absolute path via test helper.

### Phase 4B: Daemon Migration (FR-2)
**Scope:** Move internal/daemon/ → internal/muxcore/daemon/
**Risk:** Low — 1 external caller, no circular deps
**Strategy:** git mv, update 1 import in cmd/mcp-mux/daemon.go

### Phase 4C: Client Extraction (FR-3)
**Scope:** Extract client.go + resilient_client.go to internal/muxcore/client/
**Risk:** Low — 1 external caller (cmd/mcp-mux/main.go)
**Strategy:** May move with owner in Phase 4A or separate

### Phase 4D: Upstream Absorption (FR-4)
**Scope:** Move internal/upstream/ → internal/muxcore/upstream/
**Risk:** Low — consumed by muxcore/owner only
**Strategy:** git mv + import update

### Phase 5: Engine Package (FR-5)
**Scope:** New internal/muxcore/engine/ with Run(config) entry point
**Risk:** Medium — new API design, must support daemon + client modes
**Strategy:** Create after all packages migrated. Engine imports muxcore/{owner,daemon,client,upstream}

### Phase 6: Cleanup (FR-6)
**Scope:** Delete old packages, update all callers to import muxcore/ directly
**Risk:** Low after shim period — all callers already tested with aliases
**Strategy:** Remove shims one at a time, update callers, verify

## Library Decisions

| Component | Library | Version | Rationale |
|-----------|---------|---------|-----------|
| Process groups | muxcore/procgroup | internal | Already built (T010-T014) |
| Supervisor | thejerf/suture/v4 | v4.0.6 | Already integrated |
| Windows Job API | golang.org/x/sys | v0.43.0 | Already added |
| All other | Custom/internal | — | Pure refactoring, no new deps needed |

## Unknowns and Risks

| Unknown | Impact | Resolution Strategy |
|---------|--------|-------------------|
| testdata relative paths after owner move | MED | Use projectRoot() test helper |
| Whether client should be muxcore/client or muxcore/owner | LOW | Separate package — different concern |
| Engine API surface stability | LOW | Internal only, can iterate freely |
| mcpserver references to mux.Owner | MED | Update imports or keep alias |

## DX Review (Library/CLI project)

**TTHW (Time to Hello World) for muxcore consumer:**
- Target: `import "github.com/thebtf/mcp-mux/internal/muxcore/engine"` + `engine.Run(config)` = < 2 min
- Currently: must import 5+ packages and wire manually = 10+ min
- Engine package reduces to single import = Champion tier

## Task Generation Notes

Each phase should produce 2-4 tasks max. Total: ~12 tasks for Phase 4-6.
Phase 4A (owner) is the only T3 task — all others are T1-T2.
