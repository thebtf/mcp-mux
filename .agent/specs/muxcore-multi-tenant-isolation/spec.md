---
feature_id: F-021
slug: muxcore-multi-tenant-isolation
title: muxcore Multi-Tenant FS Isolation
status: ACTIVE
created: 2026-04-27
updated: 2026-04-27
authors:
  - orchestrator
parent_specs: []
linked_issues:
  - thebtf/mcp-mux#102
  - thebtf/mcp-mux#103
linked_artifacts:
  - architecture.md
  - user_job_statement.md
muxcore_release_target: muxcore/v0.22.0
mcp_mux_release_target: v0.22.0
breaking: true
---

# Feature: muxcore Multi-Tenant FS Isolation

**Slug:** muxcore-multi-tenant-isolation
**Created:** 2026-04-27
**Status:** ACTIVE (CR-001 Initial Scope)
**Author:** orchestrator (in-session)

> **Provenance:** Specified by orchestrator (Opus 4.7) on 2026-04-27.
> Evidence from: GitHub issues #102 + #103, aimux investigation session `019dcf25-d22c-7893-91bb-6e958f05bd02`, mcp-launcher persist reproducer (`thebtf/mcp-launcher@d8f9f7c`), in-session FS verification (`TEMP` socket inventory of host `D:\Dev\mcp-mux`), source grep of `muxcore/serverid/serverid.go:189-201` and call-site enumeration (Serena `find_referencing_symbols`).
> Confidence: VERIFIED — every claim traces to a tool call this session.

## Overview

muxcore must isolate per-engine state on the shared host filesystem so multiple muxcore-based binaries (mcp-mux, aimux, engram, future third-party) coexist without cross-tenant interference. Today every consumer creates owner sockets under the literal prefix `"mcp-mux-"`, hardcoded in `muxcore/serverid/serverid.go:189-201`. mcp-mux's control-plane tools (`mux_list`/`mux_stop`/`mux_restart`) walk that shared namespace and treat any matching file as their own. Operators kill foreign processes by accident; downstream consumers (aimux) cannot honour their own `engine.Config.Persistent` contract because the same propagation gap that hides ownership also drops the persistence flag.

This arc fixes both symptoms by making engine.Name authoritative for FS namespacing, propagating engine.Config fields fully to OwnerEntry, and replacing pattern-based FS scans with daemon-authoritative ownership queries. Ships as muxcore/v0.22.0 (breaking) + mcp-mux v0.22.0 (consumer adoption).

## Context

mcp-mux is open-source infrastructure that several downstream binaries embed via the `muxcore` Go library. Two of those binaries — aimux and engram — are developed in this user's ecosystem; third-party adopters are anticipated.

Today the public muxcore engine API accepts `Config.Name` (used for daemon control socket names) but does NOT use that name when constructing per-owner sockets. Per-owner sockets fall back to a hardcoded `"mcp-mux-"` prefix at the L0 (`serverid` package) layer. The result is a single shared FS namespace across all engine instances on a host.

Two production failures observed during 2026-04 sprint:

1. **Operator footgun (#102):** mcp-mux's `mux_list` enumerates `TEMP/mcp-mux-*.ctl.sock` and presents the results as mcp-mux-managed. On the user's workstation this list contains aimux's own owner sockets. `mux_restart <foreign-id>` kills the aimux process; the CC session attached to that aimux dies; user must `/mcp → Reconnect` manually.
2. **Documented contract violated (#103):** `engine.Config.Persistent: true` is documented as "the daemon stays alive even with zero sessions". Source inspection (`grep -rn Persistent muxcore/engine/`) shows the field is declared in `engine.go:113` and read nowhere. aimux daemon dies five minutes after every Ctrl+C in CC, contradicting the contract.

> **Evidence anchor:** Verbatim quotes from `user_job_statement.md`:
> - "`mux_restart` on this `server_id` **kills** the CC-owned process, breaking the CC session" (#102)
> - "documented public API contract not honored" (#103)
> - "Production aimux daemon kept dying after CC Ctrl+C → reconnects required" (aimux investigation)

Both symptoms share a root: muxcore does not propagate engine identity (Name) and engine intent (Persistent) through the layered facade (`engine.Config → daemon.Config → OwnerEntry → FS path`). Every Config field that is supposed to influence runtime behaviour must reach the layer where that behaviour is enforced.

## Domain Modeling

DDD evaluated — not needed. muxcore is infrastructure; entities are stateless filesystem primitives (paths, sockets, locks) and process-lifecycle records (`OwnerEntry`, `Daemon`). No bounded contexts beyond the existing `serverid` / `daemon` / `engine` / `owner` package partitioning. <3 project-owned domain entities; no aggregate roots; existing technical decomposition is the correct one.

## Functional Requirements

### FR-1: Engine-name-scoped owner socket paths

`muxcore/serverid` MUST construct owner socket paths as `{name}-{id}.{sock|ctl.sock|lock}` where `{name}` is the engine.Name passed by the engine. The literal prefix `"mcp-mux-"` MUST NOT appear in `serverid` outputs except when an engine instance was explicitly named `"mcp-mux"`.

> Evidence anchors: `user_job_statement.md` Current Struggle ("operator thinks mcp-mux manages the server, uses mux tools, causes damage") + Friend-Summary ("each tenant gets their own labeled prefix").

### FR-2: Empty engine name is rejected, not defaulted

If `engine.Config.Name == ""` at `engine.New` or `engine.Run`, the engine MUST return an error naming the missing field. Silent defaulting to `"mcp-mux"` is forbidden — that is the v0.21 behaviour that produced the bug.

> Evidence anchors: `user_job_statement.md` Workaround ("none clean. Three half-measures") + Friend-Summary ("contract that says 'don't auto-empty this box' must be read at the building level").

### FR-3: `engine.Config.Persistent` propagates to `OwnerEntry.Persistent`

Every `OwnerEntry` created in SessionHandler topology MUST have its `Persistent` field initialized from the live engine `Config.Persistent`. The reaper (`muxcore/daemon/reaper.go`) consults `entry.Persistent` and is unchanged.

> Evidence anchors: `user_job_statement.md` Current Struggle ("documented public API contract not honored") + Workaround ("`AIMUX_NO_ENGINE=1` environment override … bypasses muxcore engine entirely").

### FR-4: Subprocess topology persistence path is preserved

For subprocess (non-SessionHandler) topology, `OwnerEntry.Persistent` continues to be derived from the upstream's `x-mux.persistent` capability via `classify.ParsePersistent`. FR-3 is additive — both paths converge on `OwnerEntry.Persistent` before reaper reads it.

> Evidence anchors: `user_job_statement.md` Current Struggle (per #103: "The capability-based path … does NOT cover this topology"); architecture.md ADR-013.

### FR-5: `mux_list` reads from daemon, not from FS

`internal/mcpserver/server.go` `toolMuxList` MUST query the local mcp-mux daemon via a new `list_owners` control RPC and return only the owners the daemon reports. The TEMP-directory scan MUST be removed from the primary path.

> Evidence anchors: `user_job_statement.md` Current Struggle ("operators are advised — informally, in chat — 'don't trust `mux_list` on a host with aimux'") + Friend-Summary ("Tenant A only sees Tenant A's boxes").

### FR-6: `mux_stop` and `mux_restart` resolve targets via daemon

`toolMuxStop` and `toolMuxRestart` MUST resolve `server_id` and `name` arguments through the same daemon `list_owners` RPC, then dispatch the action. They MUST NOT operate on a socket file unless the daemon confirms ownership.

> Evidence anchors: `user_job_statement.md` Current Struggle ("`mux_restart` on this `server_id` **kills** the CC-owned process") + Friend-Summary ("trash Tenant B's mail").

### FR-7: `cleanStaleSockets` is daemon-name-scoped

`muxcore/daemon.cleanStaleSockets` MUST only consider socket files whose prefix matches the daemon's own `cfg.Name`. Foreign-prefix sockets (e.g. an aimux daemon's `aimux-dev-*.sock` files when mcp-mux daemon runs cleanup) MUST be left untouched.

> Evidence anchors: `user_job_statement.md` Friend-Summary ("each tenant gets their own labeled prefix") + Workaround ("manual, fragile, and fails when an aimux owner has the same age as an mcp-mux owner").

### FR-8: New `daemon.HandleListOwners` control RPC

`muxcore/daemon` MUST expose a new control command `"list_owners"` returning a JSON array of owner records: `[{server_id, command, args, cwd, sessions, classification, mux_version, persistent, ...}, ...]`. Consumers (`internal/mcpserver`, future tools) call this in lieu of FS scanning.

> Evidence anchors: `user_job_statement.md` Workaround ("operators run `mcp-mux status` (FS-scan style listing) and cross-reference TEMP socket timestamps against process lists") + Friend-Summary ("the contract … must be read at the building level").

### FR-9: Backward-incompatible muxcore release tagged `muxcore/v0.22.0`

The `serverid` signature change is a breaking Go API change. The release MUST be tagged `muxcore/v0.22.0`. Release notes in AGENTS.md MUST list the migration path for consumers (mcp-mux, aimux, engram).

> Evidence anchors: `user_job_statement.md` Workaround ("Three half-measures, all dirty") — driver of the breaking-version decision; architecture.md ADR-014.

### FR-10: mcp-mux binary release `v0.22.0` adopts new muxcore in lockstep

`cmd/mcp-mux` and `internal/mcpserver` MUST adopt muxcore/v0.22.0 in the same release that ships the muxcore tag. mcp-mux v0.22.0 binary uses the new `serverid` signatures and the new `list_owners` RPC; it does not need to support both shapes.

> Evidence anchors: `user_job_statement.md` Current Struggle (the bug exists in mcp-mux's `mux_list`); architecture.md Phase 2 of deployment strategy.

### FR-11: Cross-version coexistence does not corrupt either side

When mcp-mux v0.22 (new prefix scheme) runs on the same host as a v0.21 consumer (legacy `mcp-mux-*` prefix), neither daemon's `cleanStaleSockets` MUST destroy the other's live sockets. Liveness is the gate, not naming convention.

> Evidence anchors: `user_job_statement.md` Friend-Summary (multi-tenant building metaphor); architecture.md "Migration safety" section.

### FR-12: Internal call sites of `serverid.{IPC,Control,Lock}Path` updated atomically

Every internal call to the three renamed functions (~10 sites in `cmd/mcp-mux/{main,daemon}.go`, `muxcore/{daemon,owner,snapshot,engine}/*.go`, `testdata/`) MUST pass an explicit `name` argument in the same PR. No call site MAY default to empty string after this release.

> Evidence anchors: `user_job_statement.md` Friend-Summary ("each tenant gets their own labeled prefix"); architecture.md Component Map.

## Non-Functional Requirements

### NFR-1: Performance — control-plane RPC overhead

`mux_list` end-to-end latency (CC → mcp-mux serve stdio → daemon control RPC → response) MUST remain under 50 ms p99 on a workstation-class machine with up to 50 owners registered. The replacement of FS scan by RPC MUST NOT regress observed latency.

### NFR-2: Test coverage — propagation regressions

The PR landing this arc MUST include three concrete regression tests (R1-R3 from #103 issue body):

- R1: unit test in `muxcore/engine` or `muxcore/daemon` confirming `Config.Persistent: true` propagates to `OwnerEntry.Persistent`.
- R2: unit test in `muxcore/daemon/reaper_test.go` confirming the reaper does NOT evict a `Persistent: true` owner past `IdleTimeout`.
- R3: integration test driven by `mcp-launcher persist` mode against a binary built on this release. Test passes when `persistent: true` survives a `daemon` watch ≥ 30 s with zero sessions.

### NFR-3: Deterministic FS namespace partitioning

For any two engine instances `A` (Name=`"mcp-mux"`) and `B` (Name=`"aimux"`) running on the same host, the set of socket files created by A and the set created by B MUST be disjoint. There MUST exist no file path that A could create AND B could create.

### NFR-4: No silent defaults

If a required engine-identity field (`Name`) is missing, behaviour MUST be a hard error returned to the consumer at `engine.New` or first `engine.Run` call, not a silent fallback. Diagnostic message MUST name the missing field.

### NFR-5: Documentation parity

AGENTS.md MUST document the new `serverid` signatures, the `list_owners` RPC, and the migration path for downstream consumers in the release block for `muxcore/v0.22.0`. README.md "Commands" / "For MCP Server Authors" sections MUST be updated where they reference the old prefix scheme.

### NFR-6: Build cleanliness

After the change, `grep -rn '"mcp-mux-"' --include='*.go'` outside of (a) `serverid` defaulting tests and (b) integration tests that explicitly construct an mcp-mux engine MUST return zero results.

## User Stories

### US1: Operator runs `mux_list` on a multi-tenant host (P0)

**As an** operator running mcp-mux alongside aimux on one machine,
**I want** `mux_list` to show only mcp-mux-managed servers,
**so that** I never accidentally `mux_restart` an aimux daemon and break someone else's CC session.

**Acceptance Criteria:**
- [ ] Workstation has at least one mcp-mux daemon and at least one aimux daemon running concurrently.
- [ ] `mux_list` (default scope, no flags) returns only servers whose owner was spawned by the local mcp-mux daemon.
- [ ] `mux_list` does NOT include aimux's owner sockets.
- [ ] No `mcp-mux-*.ctl.sock` files originate from aimux on this host post-upgrade.
- [ ] If the local mcp-mux daemon is not running, `mux_list` returns an empty array plus a one-line note ("local mcp-mux daemon not running"), not a partial FS-derived list.

### US2: aimux team relies on `Config.Persistent: true` (P0)

**As an** aimux maintainer using muxcore via `engine.New(Config{Name: "aimux", Persistent: true, SessionHandler: srv})`,
**I want** the daemon to survive zero-session windows past `IdleTimeout`,
**so that** the in-process state (loom workers, context caches, indices) is not lost on every Ctrl+C in CC.

**Acceptance Criteria:**
- [ ] `mcp-launcher persist -binary aimux-test.exe -mode persist -watch 30` reports `[persist] PASS: daemon pid X stayed alive for 30s and reconnect reused same pid` against a binary built on muxcore/v0.22.0.
- [ ] `daemon.Status` returns `persistent: true` for owners spawned with `engine.Config.Persistent: true`.
- [ ] Reaper logs do NOT show eviction of persistent owners past `IdleTimeout` in the regression test for R2.

### US3: Maintainer ships breaking-but-clean muxcore release (P1)

**As the** muxcore maintainer,
**I want** `muxcore/v0.22.0` to be a single coherent release with both #102 and #103 fixed and adopted in mcp-mux v0.22.0 atomically,
**so that** downstream consumers (aimux, engram) execute exactly one version bump and one round of testing.

**Acceptance Criteria:**
- [ ] `muxcore/v0.22.0` and `v0.22.0` (mcp-mux binary) tags are pushed in the same release window.
- [ ] AGENTS.md `## muxcore Library API` block contains a v0.22.0 entry with breaking-changes table and migration steps.
- [ ] Both GitHub releases (`muxcore/v0.22.0`, `v0.22.0`) are published with body content derived from the spec + ADR list.

### US4: Operator upgrades on a host with running v0.21 consumer (P1)

**As an** operator with mcp-mux on workstation and aimux still on muxcore v0.21,
**I want** the mcp-mux v0.22 upgrade to coexist cleanly with the older aimux,
**so that** I can roll out muxcore v0.22 across binaries on my own schedule.

**Acceptance Criteria:**
- [ ] After mcp-mux v0.22 starts, the running v0.21 aimux's owner sockets in TEMP are not removed by mcp-mux's `cleanStaleSockets`.
- [ ] `mux_list` from mcp-mux v0.22 does not show aimux's `mcp-mux-*` legacy sockets even though they match the old global pattern (because mux_list now consults the daemon, not TEMP).
- [ ] aimux v5.0.2 (still on muxcore v0.21) continues to function until its own owner schedules an adoption bump.

## Edge Cases

- **Concurrent daemons of the same Name** (two mcp-mux daemons on the same host, e.g. test fixture leftover): one binds first, second fails to bind control socket. `cleanStaleSockets` of the live daemon detects unreachable own-prefix sockets and removes them. Behaviour unchanged from v0.21 except prefix-scoping.
- **Engine.Name with FS-unfriendly characters** (slashes, spaces, control chars): out of scope for this arc; NFR-4 only requires non-empty. Validation of name characters is a follow-up if a third-party consumer tries something exotic.
- **`mux_list(all: true)` semantics post-change**: still scoped to mcp-mux daemon's owners across all projects (CC sessions). Does NOT widen scope to foreign daemons. Agents that want host-wide socket inventory must call OS-level tools, not muxcore.
- **Daemon snapshot restore across version boundary**: snapshot from v0.21 (no Name field) restored under v0.22 — daemon must reject or migrate. Default: reject + log + start fresh. Snapshot is a perf optimization, not a durability contract.
- **`HandleListOwners` under load with 100+ owners**: returned JSON ≥10KB. Stdio MCP transport handles this, but `internal/mcpserver` must paginate or cap. Cap at first 200 owners + a `truncated: true` flag — adequate for foreseeable scales.
- **Zero-owner daemon**: `HandleListOwners` returns `[]`. `mux_list` returns `[]`. Distinct from "daemon down" case (FR-5 acceptance criterion).

## Out of Scope

- Engine.Name input validation (character set, max length): follow-up if a real consumer needs exotic names.
- Migration of stale `mcp-mux-*` files left by ANY old consumer (not just self): cosmetic; can be cleaned manually.
- A v0.21-compatibility shim in v0.22 (dual prefix scheme): rejected — adds complexity, hides the fix, slows future evolution. Clean break.
- Renaming `mux_list/mux_stop/mux_restart` tool names: separate UX concern. The bug is FS namespace, not tool naming.
- Cross-host federation (muxcore daemons on different machines coordinating): never been in muxcore's scope.
- Replacing AF_UNIX with a different IPC transport (named pipes only on Windows, gRPC, etc.): orthogonal.

## Dependencies

- Go 1.25+ (existing project requirement).
- mcp-launcher commit `d8f9f7c` or later, with `persist` mode, available locally for NFR-2 R3 regression.
- aimux v5.0.2 source available for adoption verification (Phase 3 of deployment).
- engram source available for adoption verification (Phase 3 of deployment).
- Existing CI green on master at the start of the arc.

## Success Criteria

- [ ] Both #102 and #103 closed on GitHub with reference to commits + release tag.
- [ ] aimux team confirms `Config.Persistent: true` works after `go get muxcore@v0.22.0` (verification posted as a comment on #103).
- [ ] `grep -rn '"mcp-mux-"' --include='*.go'` outside the two allowlisted contexts returns zero results.
- [ ] On the user's workstation (D:\Dev\mcp-mux as primary CC project), `mux_list` shows only mcp-mux-managed entries; the 6 foreign entries observed during triage today (`056351243aa4230d`, `714a4bd785ac8829`, etc.) disappear from the list after the upgrade.
- [ ] `mcp-launcher persist` regression integrated into CI as `make persist-regression` or equivalent target. Green on master.
- [ ] Engram observation stored with key decisions and adoption matrix.

## Open Questions

These were emitted by `nvmd-architect` Section 13 and remain open for `/nvmd-clarify`:

- **[NEEDS CLARIFICATION] Q1 — Empty `Config.Name` policy.** Hard-error at `engine.New` (current ADR-010 + FR-2 default) vs default to `"mcp-mux"` for v0.21 compatibility. Recommendation: hard-error with named-field message. Confirm before plan.
- **[NEEDS CLARIFICATION] Q2 — `cleanStaleSockets` cross-prefix policy.** v0.22 mcp-mux daemon detects orphan `mcp-mux-*` files left by a previous v0.21 mcp-mux daemon (no live v0.21 daemon). Should it clean them? Recommendation: clean only if liveness probe fails (existing logic, works correctly post-rename).
- **[NEEDS CLARIFICATION] Q3 — `mux_list` "daemon down" UX.** Empty list + one-line note ("local mcp-mux daemon not running") vs explicit error returned to CC agent. Recommendation: empty + note. Avoids agent hallucinating servers and matches the "scoped to managed owners" mental model.

## Pipeline State

Phase 0: PHASE_0_COMPLETE (`user_job_statement.md`)
Phase 1: complete (`architecture.md` doubles as exploration output for this arc)
Phase 2: this document (`spec.md`)
Next: `/nvmd-clarify muxcore-multi-tenant-isolation` to resolve Q1-Q3.
