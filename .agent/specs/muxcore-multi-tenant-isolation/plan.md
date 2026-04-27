---
feature_id: F-021
slug: muxcore-multi-tenant-isolation
plan_version: 1
created: 2026-04-27
spec_ref: spec.md
arch_ref: architecture.md
clarifications_ref: clarifications/2026-04-27-auto.md
---

# Plan — muxcore Multi-Tenant FS Isolation

## Strategy

One arc, one muxcore breaking release (`muxcore/v0.22.0`), one mcp-mux binary release (`v0.22.0`), shipped together. Two failure modes (#102 + #103) collapse into one PR because the propagation gap is the same in both. Three implementation phases, gated by green CI; phase 4 (downstream adoption) is post-merge and runs against external repos.

## Implementation Phases

### Phase 1 — `muxcore/serverid` rename + `daemon.Config` extension (foundation)

**Scope:**
- `muxcore/serverid/serverid.go`:
  - Change signatures: `IPCPath(baseDir, name, id)`, `ControlPath(baseDir, name, id)`, `LockPath(baseDir, name, id)`. Format: `"{name}-{id}.{ext}"`.
  - Empty `name` is allowed at L0 (`serverid` stays a pure pkg). Validation lives at L2.
- `muxcore/daemon/daemon.go`:
  - Add `Config.Name string`, `Config.Persistent bool`.
  - Internal `*Daemon` stores `name string`. Replace every `serverid.{IPC,Control}Path(sid, "")` call with `serverid.{IPC,Control}Path(d.baseDir, d.name, sid)`.
  - Spawn path constructs `OwnerEntry{Persistent: d.cfg.Persistent || classifyResult.Persistent, ...}` for SessionHandler topology.
  - `cleanStaleSockets` accepts the daemon's name and only matches own-prefix files.
- `muxcore/daemon/snapshot.go`: same call-site updates as daemon.go for serverid functions.
- `muxcore/owner/resilient_client.go:570` — `TrimPrefix("mcp-mux-")` becomes `TrimPrefix(name + "-")` parameterized.
- `muxcore/engine/engine.go`:
  - `engine.New` validates `cfg.Name != ""` → return error.
  - Pass `cfg.Name` and `cfg.Persistent` into `daemon.Config{Name, Persistent}` in `runDaemon`.
- New control RPC `daemon.HandleListOwners(req control.Request) ([]control.OwnerInfo, error)` — returns ALL owners the daemon manages, in stable iteration order.

**Deliverables:**
- Updated source files + `muxcore/control/protocol.go` extended with `OwnerInfo` schema and `Cmd: "list_owners"`.
- Migration block at top of `muxcore/serverid/serverid.go` with one-paragraph "Why the breaking signature".
- Unit tests:
  - `serverid_test.go` covers new format, empty-name behaviour at L0 (returns `"-{id}.ext"` — invalid but library is pure; engine layer is the gate).
  - `engine_test.go` confirms `Config.Name == ""` returns error from `engine.New`.
  - `engine_test.go` confirms `Config.Persistent: true` arrives at `OwnerEntry.Persistent` (R1 from #103).
  - `reaper_test.go` confirms reaper does not evict `OwnerEntry{Persistent: true}` past IdleTimeout (R2 from #103).
  - `daemon_test.go` confirms `cleanStaleSockets` only removes own-prefix unreachable sockets when called against a daemon with `Name="aimux-test"` and pre-seeded `mcp-mux-stale-X.ctl.sock` (foreign-prefix, must NOT be touched).

**Gate:** all `go test ./muxcore/...` green; existing tests untouched outside the rename.

### Phase 2 — mcp-mux consumer adoption + `internal/mcpserver` refactor

**Scope:**
- `cmd/mcp-mux/main.go` and `cmd/mcp-mux/daemon.go`:
  - Replace every `serverid.DaemonControlPath("", "")` with `serverid.DaemonControlPath("", "mcp-mux")`. Pass `Name: "mcp-mux"` explicitly to engine Config.
  - Replace every `serverid.IPCPath(sid, "")` and `serverid.ControlPath(sid, "")` with the three-arg form including name `"mcp-mux"`.
  - Remove FS-scan filter logic (`strings.HasPrefix(name, "mcp-mux-")`) at lines 396, 401, 438, 446, 710, 715, 742, 749. Where the operation is "list / stop / restart", route through the new daemon `list_owners` RPC. Where the operation is "cleanup" (e.g. `mcp-mux stop --all`), keep filtering but parameterize prefix from a single source.
- `internal/mcpserver/server.go`:
  - `toolMuxList`: replace TEMP scan with `control.Send(daemonCtl, Cmd: "list_owners")`. Render result. If RPC fails (daemon down): return `[]` + `note: "local mcp-mux daemon not running ..."`.
  - `toolMuxStop`: resolve `server_id`/`name` via `list_owners`, then `control.Send(ownerCtl, Cmd: "shutdown")` only if the owner appears in the daemon's own list.
  - `toolMuxRestart`: same resolve-then-act pattern. Use new `daemon` RPC `restart_owner` (or compose stop + spawn through the daemon control plane).
  - Session-cwd filter (existing `ownerHasCwd` logic) operates on RPC result, not on raw socket files — same filter logic, different data source.

**Deliverables:**
- Updated source files.
- `internal/mcpserver/server_test.go` — existing `mux_list`/`mux_stop`/`mux_restart` tests rewritten against a fake daemon control endpoint instead of TEMP fixtures.
- New integration test: spin up two mcp-mux daemons in distinct `BaseDir`s; first daemon's `mux_list` returns ONLY its own owners; second daemon's owners are invisible.

**Gate:** `go test ./...` green; manual smoke (`mcp-mux serve` → `mux_list` against this workstation's TEMP) shows only mcp-mux-managed entries.

### Phase 3 — release + ship + verify

**Scope:**
- AGENTS.md `## muxcore Library API` block:
  - Add v0.22.0 entry with breaking-changes table (signatures, daemon.Config additions, list_owners RPC).
  - Migration steps for aimux + engram (one-paragraph each).
  - Bump "Upgrade" snippet to `go get github.com/thebtf/mcp-mux/muxcore@v0.22.0`.
- README.md:
  - Update "Commands" section if it mentions FS scan or `mcp-mux-` literal prefix. (Audit pass.)
  - "Session-scoped control plane" subsection: confirm wording aligns with daemon-RPC reality.
- Tag + push:
  - `git tag muxcore/v0.22.0 -m "muxcore v0.22.0 — breaking: engine-name-scoped FS namespace + Persistent propagation"`.
  - `git tag v0.22.0 -m "mcp-mux v0.22.0 — adopts muxcore/v0.22.0; mux_list now daemon-authoritative"`.
  - Both with `git push --follow-tags`.
- GitHub releases (both tags) with body derived from spec + ADRs.
- Close GitHub issues #102 + #103 with commit + tag references.
- Engram store: decision + adoption matrix.
- Workstation deploy: `mcp-mux upgrade --restart` with the new binary; verify `mux_list` no longer shows aimux entries.

**Gate:** both tags live on GitHub Releases; both issues closed with verified-fix evidence; engram observation stored.

### Phase 4 — downstream adoption (post-merge, external repos)

**Out-of-arc but tracked here for handoff:**
- aimux: bump muxcore dep, run `mcp-launcher persist` regression, post comment on #103 confirming PASS.
- engram: bump muxcore dep, smoke test, no code change expected.
- Update CONTINUITY.md across all three repos when each adoption completes.

## Risk Analysis

| Risk | Severity | Mitigation |
|------|----------|------------|
| `daemon.HandleListOwners` returns inconsistent shape vs. existing `status` RPC | MED | New schema declared explicitly in `control/protocol.go`; reuse existing per-owner status fields rather than inventing new ones. |
| Phase 2 refactor of `internal/mcpserver` breaks the cwd-scoping behaviour (`ownerHasCwd`) | MED | RPC result includes `cwd_set` per owner (already present in status); filter logic moves verbatim from current site. |
| Snapshot restore from v0.21 onto v0.22 daemon | LOW | Snapshot version stamp already exists; bump it; v0.22 rejects mismatched snapshot and starts fresh. Snapshot is perf optimization. |
| aimux v5.0.2 keeps breaking on stale `mcp-mux-*` sockets after upgrade because aimux still on v0.21 | LOW | Phase 4 closes; until then, aimux's own daemon's cleanup logic handles its own files. |
| Two regression tests (R1 + R2) over-fit to current daemon internals | LOW | Tests assert observable behaviour (`OwnerEntry.Persistent` value; reaper.go log absence), not implementation details. |
| TOCTOU between `list_owners` response and subsequent `mux_stop` on a vanished owner | LOW | Daemon `HandleStop(server_id)` already handles "not found" gracefully; mux_stop tool surfaces "not found" to the agent without retry. |

## Test Strategy

- **Unit tests:** all serverid renames + propagation paths (Phase 1 coverage targets above).
- **Integration tests:**
  - `daemon_list_owners_integration_test.go` — spawn daemon, register 3 owners with distinct cwds, call `list_owners`, assert shape + cwd filtering.
  - `mux_list_cross_engine_test.go` (new file under `internal/mcpserver/`) — start two engines with names `"mcp-mux"` and `"aimux-test"`, both spawn an owner with the same upstream binary, run `toolMuxList` against the mcp-mux daemon, assert only `"mcp-mux"`-engine owners are returned.
- **End-to-end:**
  - `mcp-launcher persist -binary aimux-test.exe -mode persist -watch 30` against a freshly built aimux-test using muxcore/v0.22.0. PASS line required.
  - Workstation manual: `mux_list` from CC, observe absence of foreign aimux entries.
- **Coverage gates:**
  - `go test -cover ./muxcore/serverid/...` ≥ 90%.
  - `go test -cover ./muxcore/daemon/...` ≥ 75% (existing baseline).
  - `internal/mcpserver/...` regression suite green.

## Pipeline-Internal Mode Detection

Mode hints (Sonnet vs. Codex routing for task-builder):
- Phase 1 = mostly mechanical signature change + new field hydration → Sonnet handles cleanly.
- Phase 2 = `internal/mcpserver` refactor + new RPC client logic → Codex (TDD-friendly, multi-file).
- Phase 3 = orchestrator-only (release management) → no delegation.

## Done Definition

All FRs map to passing tests. All NFRs measurable via at least one CI gate or smoke procedure. spec.md `## Success Criteria` checklist 6/6 ticked before Phase 3 ships.

## Pipeline Forward

Next: `/nvmd-tasks muxcore-multi-tenant-isolation` to break this plan into atomic, dependency-ordered tasks with acceptance criteria.
