# Implementation Plan: Upstream maintenance hold

**Branch**: `002-upstream-maintenance-hold` | **Date**: 2026-10-03 | **Spec**: [spec.md](spec.md)

**Input**: `specs/002-upstream-maintenance-hold/spec.md`. Source base: `3881f27b931f6b9d0467c0a15dd4e1824969e125`.

## Summary

Implement GitHub #135 as one daemon-owned maintenance lease across the selected owner's finite admitted contexts. A durable fence precedes actual request draining, full-tree death, and HELD acknowledgement. Every start transaction holds the daemon-owned gate through context election, physical start, and installation of every nonnil process authority, including partial failed starts. Exact keys survive modern nonces and isolated retry suffixes without widening to a command wildcard.

Aware host transports stay open. Owner ingress and resilient-client ingress/connection transitions reject held numeric/string IDs without cache success, replay, or ordinary reconnect-grace delay. Resume, renewal, and safe expiry operate on the exact lease. Controlled restart/handoff/shutdown/downgrade and empty-daemon idle exit are refused while fenced. Unplanned aware startup reloads durable HELD before admission; incomplete state remains blocked.

The accepted root D2 authority is primary-checkout `.agent/arch/decisions/015-upstream-maintenance-hold.md`. All FULL-challenge F1-F5 corrections are incorporated; active-lease transfer is excluded. This document and [maintenance contract](contracts/maintenance.md) are self-contained implementation inputs, not claims of implementation acceptance.

## Technical Context

**Language/Version**: Go 1.25.4 from root `go.mod`.

**Primary Dependencies**: Existing local muxcore module, `golang.org/x/sys`, `go-winio`, and suture. Reuse `muxcore/internal/envidentity.Build`, current owner/materialization and upstream tree authority, OS IPC, and product platform replace-file patterns. No new dependency or process supervisor.

**Storage**: Schema-2 logical lease authority under `os.UserConfigDir()`, scoped by the full digest of canonical daemon endpoint and namespace. Mandatory `ledger.json` and `transaction.json` use restrictive same-directory temporary writes, flush, and platform replacement, not atomic two-file replacement. PREPARE stores predecessor leases and target digest; PUBLISH requires durable preparation; FINALIZE certifies only acknowledged durable target publication. Aggregate lookup rejects missing, pending, invalid, or mismatched pairs. A finalize error remains a typed failure with conservative live memory, even when recovery can verify a matching committed certificate. Persist only opaque identities and safe lease/timing metadata. Load independently of snapshots and `SkipSnapshot` before listeners, restore, or starts. See the [storage contract](contracts/maintenance.md#authority-and-lifecycle-boundaries).

**Testing**: Existing Go package conventions plus focused failing-before/passing-after lifecycle regressions and one live-process executable-replacement smoke. Root runs integrated/full release validation once after both maker surfaces land. This plan stage runs no Go code, tests, builds, or formatters.

**Target Platform**: Windows named pipes/Job Objects and Unix sockets/process groups. Both require actual tree and overwrite proof; one platform does not prove the other.

**Project Type**: Go CLI, MCP control adapter, and consumed muxcore library.

**Performance Goals**: No new polling authority or per-request context hashing; compute immutable normalized identity at admission, reuse it during routing. Preserve current streaming behavior outside maintenance. Hold waits only for prior start/install transactions, the one accepted drain deadline, and existing retirement proof.

**Constraints**: Gate before `d.mu` or owner locks, no gate acquisition from an owner-held lock, no finalization under `d.mu`, and no gate retained across drain/retirement waits. Reject ambiguous context sets before mutation. Tree death excludes committed handoff even if `RetirementProven` is true. No snapshots of modern live work, secret readbacks, cross-engine writes, old-binary control claim, request replay, or stop/exec fallback.

**Scale/Scope**: One current-user engine namespace, finite admitted context sets, existing owner registry and tree authority. Serialized paired-authority mutations govern lease acquisition/renewal/release. Active-lease transfer and reconstruction of lost tree authority are out of scope.

## Constitution Check

| Principle | Before research | After design |
| --- | --- | --- |
| I. Protocol authority | Retain R1's pinned same-era modern contract and legacy defaults. | PASS for design. The product maintenance error is local JSON-RPC; no new MCP method, era translation, bootstrap, or replay. See existing `specs/001-mcp-2026-07-28-r1/research.md` normative references. |
| II. One process-tree authority | Reuse owner/materialization, exact-entry CAS, and upstream Job/PGID authority. | PASS for design. One daemon admission gate; no second supervisor, installer retry, PID sweep, or launcher respawn mechanism. |
| III. Complete source-grounded work | Read exact source, installed commands, constitution, and two prior sets before authoring. | PASS for design. Accepted ADR settles the mechanism. Planned symbols and runner are labeled future implementation. |
| IV. Regression evidence | Bug fix requires focused regression and runtime replacement proof. | PASS for design. Tasks require RED/GREEN, specific races/edges, Windows/Unix tree and file replacement, root release gates. No implementation proof claimed. |
| V. Compatible consumers | Keep existing interfaces/signatures unchanged by default. | PASS for design. Optional maintenance/shutdown handlers and omitted JSON fields; aware-shim adoption and terminal lifecycle refusal documented. Required consumer handoff remains root-owned. |

No constitution exception is requested. SpecKit files live in repository `specs/` as selected by the installed workflow; historical primary `.agent/specs/` examples are read-only precedent, not a second active selector.

## Project Structure

### Documentation (this feature)

```text
specs/002-upstream-maintenance-hold/
  spec.md
  checklists/requirements.md
  checklists/plan.md
  plan.md
  research.md
  data-model.md
  contracts/maintenance.md
  quickstart.md
  tasks.md
```

### Source Code (repository root)

```text
muxcore/control/{protocol.go,server.go,client.go,maintenance.go}
muxcore/daemon/{daemon.go,owner_lifecycle.go,snapshot.go,handoff.go,reaper.go,maintenance.go,maintenance_store.go,maintenance_store_windows.go,maintenance_store_unix.go}
muxcore/owner/{owner.go,materialization.go,resilient_client.go}
muxcore/upstream/process.go and platform containment implementation
muxcore/engine/{engine.go,update.go}
muxcore/internal/envidentity/identity.go             # reuse unchanged policy
cmd/mcp-mux/{main.go,daemon.go,launcher.go}           # CLI and terminal refusal
internal/mcpserver/server.go                         # MCP and managed restart
scripts/lifecycle-smoke-upstream/main.go             # minimal fixture extension
scripts/smoke-upstream-maintenance.ps1               # planned cross-platform proof
README.md, README.ru.md, AGENTS.md, muxcore/README.md
docs/{mux-protocol.md,PRODUCTION-TESTING-PLAYBOOK.md,RELEASE-PROTOCOL.md}
CHANGELOG.md, RELEASE_NOTES.md
```

New names denote proposed files; reuse an existing file where the coherent change fits. Read the exact platform upstream implementation before changing retirement proof. Do not change unrelated R1 specs or the constitution.

**Structure Decision**: Core maker owns muxcore/control, daemon, owner/upstream, and engine. Adapter maker owns cmd, internal MCP, consumer docs, fixture, and the new smoke runner. Core first commits the shared neutral Request/Response/result/error methods from `contracts/maintenance.md`; adapters never invent parallel DTOs or daemon operations. Root owns integrated verification and release. Neither maker self-certifies root acceptance.

## Integration and verification design

1. Publish immutable context keys on placeholders before registry publication; record finite admitted reconnect contexts through the same gate. Scope matching covers all generations or refuses before acquisition.
2. Cover initial/template/persistent/restored starts and every same-owner materialization retry through start-and-install admission. Release gate before waiting on placeholders/classification and recheck afterward.
3. Durably commit HOLDING and close request admission before actual request drain. Carry the single accepted deadline through retirement retries. Mark HELD only after dead trees, settled placeholders, no detach, durable write, and usable TTL.
4. Freeze lease/error types before adapters. Typed refusals traverse both engine and CLI spawn/refresh paths. Held ingress, queue-capacity behavior, and reconnect transition disposition are part of core, not just transport retry classification.
5. Resume/renew/expiry use current lease and durable commit ordering. Persistence failure never opens admission. Startup authority loads before any activation. Lifecycle refusals remain terminal through library update helpers and CLI launcher fallbacks.
6. Prove two shared legacy hosts, a child, genuine bounded in-flight drain, held original IDs, actual byte overwrite, no scoped respawn, and fresh new-version response on unchanged pipes. Add the native modern and explicit old-daemon/old-shim cases. [Quickstart](quickstart.md) maps all seven success criteria to runnable proof and root gates.

## Plan review receipt

Accepted root corrections F1 exact-context sets, F2 start-through-authority-install, F3 request drain/tree death, F4 durable startup and terminal controlled lifecycle refusal, and F5 typed adapters/ingress are traced in `research.md` and tasks. Requirements review was updated after acceptance. Local plan review checks scope, API dependency, lock/storage ordering, and proof completeness in `checklists/plan.md`; it does not replace root review or implementation acceptance.

## Complexity Tracking

No exception. Keep one ledger and one admission gate at the existing authority. Refuse active-fence transfer instead of adding successor negotiation. Reuse fixtures and package tests rather than a new verification framework.
