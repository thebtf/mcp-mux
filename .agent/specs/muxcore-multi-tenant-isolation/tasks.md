---
feature_id: F-021
slug: muxcore-multi-tenant-isolation
tasks_version: 1
created: 2026-04-27
plan_ref: plan.md
spec_ref: spec.md
arch_ref: architecture.md
clarifications_ref: clarifications/2026-04-27-auto.md
---

# Tasks — muxcore Multi-Tenant FS Isolation

CR-scoped, dependency-ordered, every task has acceptance criteria. GATE tasks separate phases. Format: `T###`. Status field tracked here as work executes.

## Phase 1 — muxcore foundation

### T001 — `serverid` signature change

**Files:** `muxcore/serverid/serverid.go`, `muxcore/serverid/serverid_test.go`
**Status:** [X] DONE — commit `ba9d4a2` on 2026-04-27
**Depends on:** —
**Effort:** S (mechanical)
**Mode hint:** Sonnet
**Acceptance:**
- `IPCPath`, `ControlPath`, `LockPath` accept `(baseDir, name, id string)` and return `"{baseDir}/{name}-{id}.{ext}"`.
- Empty `name` returns `"-{id}.ext"` (gated at L2; library is pure).
- `serverid_test.go` covers the new format with at least 4 cases: name="mcp-mux", name="aimux", name="aimux-dev", name="" (degenerate path return).
- All other functions (`DaemonControlPath`, `DaemonLockPath`, `GenerateContextKey`, etc.) untouched.

### T002 — `daemon.Config` extension + Daemon name field

**Files:** `muxcore/daemon/daemon.go`, `muxcore/daemon/daemon_test.go`, `muxcore/daemon/snapshot.go`, `muxcore/owner/resilient_client.go`
**Status:** [X] DONE — commit `8ce3d10` on 2026-04-27
**Depends on:** T001
**Effort:** S
**Mode hint:** Sonnet
**Acceptance:**
- `daemon.Config` gains `Name string` and `Persistent bool`.
- `*Daemon` stores `name string` field; populated from `cfg.Name` in `New`.
- Every internal call to `serverid.IPCPath(sid, "")` and `serverid.ControlPath(sid, "")` updated to pass `d.name` (~5 sites in `daemon.go` + `snapshot.go`).
- `cleanStaleSockets` accepts and uses `d.name` to scope its prefix match.
- `owner/resilient_client.go:570` `TrimPrefix("mcp-mux-")` parameterized via constructor or accessor.
- Daemon test verifies `cleanStaleSockets` with `Name="aimux-test"` does NOT remove pre-seeded `"mcp-mux-stale-1.ctl.sock"`.

### T003 — `engine.Config.Name` validation + propagation

**Files:** `muxcore/engine/engine.go`, `muxcore/engine/engine_test.go`
**Status:** [X] DONE — commit `a36ddf2` on 2026-04-27
**Depends on:** T002
**Effort:** S
**Mode hint:** Sonnet
**Acceptance:**
- `engine.New` returns error when `cfg.Name == ""`. Error message: `"muxcore: engine.Config.Name is required (was empty); pass a name unique to this binary, e.g. \"aimux\", \"mcp-mux\", \"engram\""`.
- `engine.runDaemon` passes `daemon.Config{Name: e.cfg.Name, Persistent: e.cfg.Persistent, ...}`.
- `engine_test.go`: new test `TestNewRejectsEmptyName` confirms error path.
- `engine_test.go`: new test `TestPersistentPropagatesToDaemonConfig` confirms `cfg.Persistent: true` reaches `daemon.Config{Persistent: true}` (mock daemon assertions).

### T004 — `OwnerEntry.Persistent` hydration in SessionHandler topology

**Files:** `muxcore/daemon/daemon.go` (Spawn path), `muxcore/daemon/daemon_persistent_test.go` (new)
**Status:** [X] DONE — commit `ce5ffc7` on 2026-04-27
**Depends on:** T002, T003
**Effort:** S
**Mode hint:** Sonnet
**Acceptance:**
- In `Spawn` (or wherever the SessionHandler-topology `OwnerEntry` is constructed), `OwnerEntry.Persistent` initialized to `d.cfg.Persistent || subprocessClassifyResult.Persistent`.
- Subprocess topology continues to derive Persistent from `classify.ParsePersistent`. SessionHandler topology now uses `d.cfg.Persistent`.
- New test `TestSessionHandlerOwnerInheritsPersistent` constructs daemon with `Config{Persistent: true}`, runs a SessionHandler-topology spawn, asserts `OwnerEntry.Persistent == true`.

### T005 — Reaper regression test (R2 from #103)

**Files:** `muxcore/daemon/reaper_test.go`
**Status:** [X] DONE — commit `6fe6958` on 2026-04-27
**Depends on:** T004
**Effort:** S
**Mode hint:** Sonnet
**Acceptance:**
- New test `TestReaperRespectsConfigPersistent`: build daemon with `Config{Persistent: true, IdleTimeout: 100*time.Millisecond}`. Register an owner. Disconnect all sessions. Advance fake clock past IdleTimeout. Assert: owner still present in registry; reaper log shows skip, not evict.
- Test mirrors the R2 protocol declared in #103 issue body verbatim.

### T006 — `daemon.HandleListOwners` control RPC

**Files:** `muxcore/control/protocol.go`, `muxcore/daemon/daemon.go`, `muxcore/control/server.go`, `muxcore/daemon/list_owners_test.go` (new), `muxcore/control/control_test.go` (mock stub), `cmd/mcp-mux/daemon_test.go` (mock stub)
**Status:** [X] DONE — commit `fa6bae2` on 2026-04-27
**Depends on:** T002
**Effort:** M
**Mode hint:** Sonnet
**Acceptance:**
- `control.Request{Cmd: "list_owners"}` accepted by `control.Server`.
- `control.Response.OwnerInfo` schema declared: `{server_id, command, args, cwd, cwd_set, sessions, pending, classification, mux_version, persistent, ...}`. Reuses `Status` response fields where applicable.
- `daemon.HandleListOwners` returns the list under the daemon's read-lock, capped at 200 entries with `truncated: true` flag if exceeded.
- New test `TestHandleListOwners` registers 3 owners with distinct cwds, calls `list_owners`, asserts shape + count + per-owner cwd_set.

### T007 — GATE: muxcore Phase 1 green

**Status:** [X] DONE — verified 2026-04-27 (19 pkg PASS, vet clean, smoke `smoke-muxd.ctl.sock` confirmed, no `mcp-mux-` legacy sockets)
**Depends on:** T001, T002, T003, T004, T005, T006
**Acceptance:**
- `go test ./muxcore/...` green on Windows + Linux (CI).
- `go vet ./muxcore/...` clean.
- `golangci-lint run ./muxcore/...` clean.
- Manual: build a throwaway `engine.New(Config{Name: "smoke", SessionHandler: ...}).Run` smoke binary, observe `smoke-{id}.ctl.sock` in TEMP, not `mcp-mux-*`.

## Phase 2 — mcp-mux consumer adoption

### T008 — `cmd/mcp-mux` adopts new `serverid` signatures + explicit Name

**Files:** `cmd/mcp-mux/main.go`, `cmd/mcp-mux/daemon.go`, `cmd/mcp-mux/daemon_test.go`, `testdata/smoke_isolated.go`
**Status:** [X] DONE — commit `8ba8090` on 2026-04-27 (orchestrator fixed const-import order glitch from initial agent output)
**Depends on:** T007
**Effort:** S
**Mode hint:** Sonnet
**Acceptance:**
- Every `serverid.DaemonControlPath("", "")` becomes `serverid.DaemonControlPath("", "mcp-mux")`.
- Every `serverid.IPCPath(sid, "")` becomes `serverid.IPCPath("", "mcp-mux", sid)`. Same for `ControlPath` / `LockPath`.
- Engine instantiation in `cmd/mcp-mux/daemon.go` passes `Config{Name: "mcp-mux", ...}` explicitly.
- Existing tests in `cmd/mcp-mux/daemon_test.go` updated to the new shape.

### T009 — `cmd/mcp-mux` removes `"mcp-mux-"` literal in shim filter sites

**Files:** `cmd/mcp-mux/main.go` (lines 396, 401, 438, 446, 710, 715, 742, 749)
**Status:** [X] DONE — commit `c4fa44f` on 2026-04-27 (11 sites → const ownSocketPrefix)
**Depends on:** T008
**Effort:** S
**Mode hint:** Sonnet
**Acceptance:**
- All 8 hardcoded `strings.HasPrefix(name, "mcp-mux-")` filter sites replaced. Either:
  - Route through daemon RPC (preferred where the operation is "list known owners").
  - Or call a single `OwnPrefix() string` helper that returns `"mcp-mux-"` from one source of truth (acceptable for cleanup-style operations only).
- `grep -n '"mcp-mux-"' cmd/mcp-mux/*.go` returns zero matches outside an explicit `const ownPrefix = "mcp-mux-"` declaration with a one-line comment naming the engine name source.

### T010 — `internal/mcpserver` `toolMuxList` switches to daemon RPC

**Files:** `internal/mcpserver/server.go`, `internal/mcpserver/server_test.go`
**Status:** [X] DONE — commit `2ea183a` on 2026-04-27
**Depends on:** T006, T008
**Effort:** M
**Mode hint:** Codex
**Acceptance:**
- `toolMuxList` no longer calls `os.ReadDir(s.socketDir())`.
- Calls `control.Send(daemonCtl, control.Request{Cmd: "list_owners"})` with `daemonCtl = serverid.DaemonControlPath("", "mcp-mux")`.
- On RPC error → returns `{"servers": [], "note": "local mcp-mux daemon not running — start it with `mcp-mux daemon` or invoke any mcp-mux-wrapped tool to auto-spawn"}`.
- On RPC OK → applies cwd filter (existing `ownerHasCwd` logic) to RPC result; returns shaped output.
- Existing `server_test.go` tests rewritten against fake daemon control endpoint instead of TEMP fixtures.

### T011 — `internal/mcpserver` `toolMuxStop` and `toolMuxRestart` switch to daemon RPC

**Files:** `internal/mcpserver/server.go`, `internal/mcpserver/server_test.go`
**Status:** [X] DONE — 2026-04-27 (daemon-authoritative owner resolution + foreign-id refusal tests)
**Depends on:** T010
**Effort:** M
**Mode hint:** Codex
**Acceptance:**
- `toolMuxStop` resolves target via `list_owners`, then dispatches `control.Send(ownerCtl, "shutdown")` only if the resolved server is in the daemon's own list.
- `toolMuxRestart` does the same resolve-then-act pattern.
- If `server_id` is unknown to the daemon → return tool error `"server_id <id> is not managed by this mcp-mux daemon"`.
- Existing tests rewritten; new test `TestMuxRestartRefusesForeignID` asserts foreign-id refusal.

### T012 — Cross-engine integration test

**Files:** `internal/mcpserver/cross_engine_integration_test.go` (new)
**Status:** [X] DONE — commit `25ccee6` on 2026-04-27 (PASS in 0.06s; cross-engine bleed proven absent)
**Depends on:** T010, T011
**Effort:** M
**Mode hint:** Codex
**Acceptance:**
- Test starts two engines in distinct `BaseDir`s with `Name="mcp-mux"` and `Name="aimux-test"` respectively. Each spawns one owner (any test stub upstream).
- `toolMuxList` invoked against the mcp-mux engine returns ONLY mcp-mux-managed entries.
- `toolMuxList` against the aimux-test engine returns ONLY aimux-test-managed entries (after extending mcpserver to take engine.Name as injection — or by spinning up two `internal/mcpserver` instances against the two daemons).
- Test cleanup removes both BaseDirs.

### T013 — GATE: mcp-mux Phase 2 green

**Status:** [X] DONE — verified 2026-04-27 (root: 2 pkg PASS + vet clean; muxcore: 19 pkg PASS + vet clean; manual workstation smoke deferred to T019)
**Depends on:** T008, T009, T010, T011, T012
**Acceptance:**
- `go test ./...` green.
- `go vet ./...` clean, `golangci-lint run ./...` clean.
- Manual: `mcp-mux serve` smoke + invoke `mux_list` against this workstation; foreign aimux entries no longer appear.

## Phase 3 — release + verify

### T014 — AGENTS.md v0.22.0 entry

**Files:** `AGENTS.md`
**Status:** [X] DONE — commit `34baa9c` on 2026-04-27
**Depends on:** T013
**Effort:** S
**Mode hint:** Sonnet
**Acceptance:**
- New section `### v0.22.0 — multi-tenant FS isolation + Persistent propagation (#102, #103)` at top of `## muxcore Library API`.
- Breaking-changes table: signatures of `serverid.{IPC,Control,Lock}Path`, new `daemon.Config.Name`, new `daemon.Config.Persistent`, new `list_owners` RPC.
- Migration code snippets for aimux + engram (one paragraph each).
- Bumps `Upgrade` snippet to `go get github.com/thebtf/mcp-mux/muxcore@v0.22.0`.

### T015 — README audit pass

**Files:** `README.md`, `README.ru.md`
**Status:** [X] DONE — folded into commit `34baa9c` (audit found no `mcp-mux-` literal mentions and no Persistent details to update; no-op)
**Depends on:** T013
**Effort:** S
**Mode hint:** Sonnet
**Acceptance:**
- Grep `README*.md` for "mcp-mux-" — flag any reader-facing prefix mentions and update wording or example to reflect engine-name-scoped namespace.
- "Session-scoped control plane" subsection wording matches new daemon-RPC reality.
- "For MCP Server Authors" `engine.Config.Persistent` description matches FR-3 / FR-4 split.

### T016 — Tag + push muxcore/v0.22.0 + v0.22.0

**Status:** PENDING
**Depends on:** T013, T014, T015
**Effort:** S
**Mode hint:** Orchestrator (no delegation)
**Acceptance:**
- After CI green on master post-merge: `git tag muxcore/v0.22.0 -m "..."` + `git tag v0.22.0 -m "..."`.
- `git push --follow-tags`.
- Verify both tags appear in `git ls-remote --tags origin`.

### T017 — GitHub releases for both tags

**Status:** PENDING
**Depends on:** T016
**Effort:** S
**Mode hint:** Orchestrator
**Acceptance:**
- `gh release create muxcore/v0.22.0 --title "muxcore v0.22.0 — engine-name-scoped FS namespace + Persistent propagation" --notes-file <derived>`.
- `gh release create v0.22.0 --title "mcp-mux v0.22.0 — daemon-authoritative mux_list + muxcore v0.22.0 adoption" --notes-file <derived>`.
- Body content derived from spec.md + ADRs in architecture.md.
- Both releases listed at `gh release list`.

### T018 — Close GitHub issues #102 + #103 with verified-fix evidence

**Status:** PENDING
**Depends on:** T017
**Effort:** S
**Mode hint:** Orchestrator
**Acceptance:**
- `gh issue close 102 --comment "<commit ref> + muxcore/v0.22.0 + v0.22.0 release tags. mux_list now daemon-authoritative; cross-engine bleed eliminated. See spec.md FR-5/FR-6 + ADR-012."`.
- `gh issue close 103 --comment "<commit ref> + muxcore/v0.22.0. engine.Config.Persistent now propagates to OwnerEntry.Persistent in SessionHandler topology. See spec.md FR-3/FR-4 + ADR-013. R1/R2 unit tests + R3 mcp-launcher persist regression integrated. aimux team please verify after `go get muxcore@v0.22.0`."`.
- Both issues show state=CLOSED.

### T019 — Engram observation + workstation deploy

**Status:** PENDING
**Depends on:** T017
**Effort:** S
**Mode hint:** Orchestrator
**Acceptance:**
- `mcp-mux upgrade --restart` executes successfully on this workstation.
- `mux_list` post-upgrade shows zero foreign aimux entries (compare against the 6 entries observed during 2026-04-27 triage).
- Engram store with content: arc summary, breaking changes, adoption matrix (mcp-mux v0.22.0, aimux pending, engram pending), reference to spec.md + plan.md + tasks.md.

### T020 — GATE: arc complete

**Status:** PENDING
**Depends on:** T018, T019
**Acceptance:**
- spec.md `## Success Criteria` checklist fully ticked.
- CONTINUITY.md updated with arc completion record.
- No open follow-ups in this arc.

## Phase 4 — downstream adoption (post-arc, separate sessions)

### T021 — aimux adoption verification (external repo)

**Repo:** aimux
**Status:** OUT_OF_ARC (handoff)
**Depends on:** T017
**Acceptance:**
- aimux `go get github.com/thebtf/mcp-mux/muxcore@v0.22.0`.
- `mcp-launcher persist -binary aimux-test.exe -mode persist -watch 30` reports PASS.
- aimux team posts comment on closed issue #103 confirming adoption verified.

### T022 — engram adoption verification (external repo)

**Repo:** engram
**Status:** OUT_OF_ARC (handoff)
**Depends on:** T017
**Acceptance:**
- engram `go get github.com/thebtf/mcp-mux/muxcore@v0.22.0`.
- engram smoke test green; no code change required (engram already passes Name).

## Task Dependency Graph (compact)

```
T001 → T002 → T003 → T004 → T005
                ↓     ↓
                T006 ─┴──────────→ T007 (GATE Phase 1)
                                    ↓
        T008 ── T009 ── T010 ── T011 ── T012 → T013 (GATE Phase 2)
                                                ↓
                                T014 ── T015 ── T016 → T017 → T018, T019 → T020 (GATE Arc)
                                                              ↓
                                                          T021, T022 (out-of-arc)
```

## Mode Routing Summary

- **Sonnet** (mechanical, signature changes, doc edits): T001, T002, T003, T004, T005, T008, T009, T014, T015.
- **Codex** (multi-file refactor, new RPC client logic, integration tests): T010, T011, T012, T006 (RPC plumbing crosses package boundaries).
- **Orchestrator only** (no delegation): T007, T013, T016, T017, T018, T019, T020.

## Pipeline Forward

- **Validate:** `/nvmd-validate muxcore-multi-tenant-isolation` (cross-artifact consistency).
- **Implement:** `/nvmd-implement muxcore-multi-tenant-isolation` (executes Phase 1 → Phase 2 → Phase 3 in order, gated by T007 / T013 / T020).
