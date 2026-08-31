# Implementation Plan: MCP 2026-07-28 R1 Native Isolation

**Branch**: `feature/mcp-2026-07-28-r1` | **Date**: 2026-08-31 | **Spec**: [`spec.md`](spec.md)

**Input**: Feature specification from [`specs/001-mcp-2026-07-28-r1/spec.md`](spec.md).

**Candidate source identity**: `939aa694185f3242d755601997da404ef53d41e9`, with `cee5444` confirmed as an ancestor. This plan uses the candidate source at that identity; the separately retained preservation prototype is rejected evidence only.

## Summary

Add one explicit, pinned MCP `2026-07-28` route for a known modern host. Before daemon owner election, the CLI and embedded engine buffer and validate the opening modern request, select an immutable modern protocol era, and require an exact-era control echo. The owner is forced isolated, forwards the original opening bytes to a same-era upstream without legacy bootstrap, cache, template, or replay traffic, and keeps released legacy behavior byte-compatible by default.

R1 is deliberately a safety boundary, not a sharing or persistence release: modern cache/template/replay are off; upstream JSON-RPC requests are contained; standard logging can reach only the sole downstream; and every snapshot, handoff, reaper, zero-session, retry, respawn, loss, and reconnect route preserves the exact era or drains, cold-starts, or fails closed. It does not implement R2 multiplexing/correlation or R3 persisted modern lifecycle.

## Technical Context

**Language/Version**: Go 1.25.4 (`go.mod`).

**Primary Dependencies**: Local `github.com/thebtf/mcp-mux/muxcore` replacement, `golang.org/x/sys`, `github.com/Microsoft/go-winio`, and `github.com/thejerf/suture/v4`; MCP `2026-07-28` is pinned to the normative source revision `5f5440bb26a62e2cf3440b92da5a667efa03b267`.

**Storage**: Local OS IPC endpoints and temporary snapshot files. R1 persists no modern request, progress, subscription, retry, cache, template, opaque MRTR state, handoff state, registry descriptor field, or status counter.

**Testing**: Focused Go unit/integration and customer-fixture scenarios in existing `muxcore`/`cmd` test packages; legacy fixtures remain parity coverage. This Plan stage creates and runs no tests.

**Target Platform**: Windows named pipes/Job Objects and Unix sockets/process groups.

**Project Type**: Go CLI plus consumed `muxcore` library and daemon/control-plane service.

**Performance Goals**: Preserve current streaming behavior; buffer exactly one newline-delimited opening frame only for policy selection, forward accepted opening bytes unchanged once, and introduce no modern cache/template/replay path.

**Constraints**: Explicit pinned modern selection with no automatic fallback; required request metadata is validated per request; zero-valued public configuration remains legacy; modern identity is distinct and filesystem-safe; R1 modern owners are forced isolated; local process-generation and `RetirementProven` fences remain authoritative; direct R1 readback is redacted and limited to exact control echo plus minimal `OwnerInfo` policy facts; no semantic protocol translation.

**Scale/Scope**: One known modern host to one dedicated same-era upstream per R1 owner, while released legacy users retain existing owner sharing/lifecycle behavior. R2 sharing/correlation and R3 persisted lifecycle and extended observability are excluded.

## Constitution Check

*Pre-review constitution record from Phase 0 and Phase 1. The review receipt below is the current correction record.*

| Principle | Pre-Phase 0 evidence | Pre-review disposition |
| --- | --- | --- |
| I. MCP Protocol Spec Is Authoritative | The root normative cache and the primary-source packet pin MCP `2026-07-28` at `5f5440bb…`. | **PASS** — `research.md` separates observed protocol requirements from R1 product policy and cites the pinned sources. |
| II. Single Process-Tree Authority | Existing daemon/owner generation and whole-tree finalization remain the only lifecycle authority. | **PASS** — R1 reuses generation inflight fences and `RetirementProven`; it adds no parallel spawn, replay, cleanup, or retry mechanism. |
| III. No Stubs, No Guessing, Reasoning First | Candidate source, the primary MCP source revision, and coordination-root context were read before design. | **PASS** — no source implementation is proposed as complete; all decisions and rejected alternatives are recorded in the Phase 0 artifact. |
| IV. Every Fix Ships a Regression Test | No fix is being implemented in this Plan stage. | **PASS FOR DESIGN** — `quickstart.md` and the test plan name the focused RED/GREEN and customer proof required before implementation/release. |
| V. Consumer-Compatible Versioning | `engine.New`, `MuxEngine.Run`, `RunResilientClient`, `control.Request`, `GenerateContextKey`, `Owner.Status`, and `HandleListOwners` call sites were mapped with LSP references. | **PASS** — public changes are additive recommendations; zero value remains legacy and legacy identity/bootstrap remain unchanged. |


## Project Structure

### Documentation (this feature)

```text
specs/001-mcp-2026-07-28-r1/
├── plan.md                         # This Plan-stage implementation plan
├── research.md                     # Phase 0 decisions and source rationale
├── data-model.md                   # Phase 1 caller-first model and states
├── quickstart.md                   # Future customer-validation guide
├── contracts/
│   ├── public-policy.md            # Additive library/CLI policy contract
│   ├── daemon-control.md           # Spawn selection and exact-era echo
│   ├── owner-status.md             # Minimal redacted admission/rollback truth
│   └── lifecycle-quarantine.md     # R1 boundary outcomes and errors
└── tasks.md                        # Deliberately absent; Phase 2 only
```

### Source Code (candidate repository root)

```text
cmd/mcp-mux/
└── main.go                         # CLI ingress, daemon spawn, reconnect closure, status

muxcore/
├── engine/engine.go                # Embedded ingress, daemon spawn, resilient-client wiring
├── control/
│   ├── protocol.go                 # One-request/one-response control DTOs
│   ├── client.go
│   └── server.go
├── daemon/
│   ├── daemon.go                   # Election, spawn/reuse, status/list, retries, loss
│   ├── snapshot.go
│   ├── handoff.go
│   ├── reaper.go
│   └── owner_lifecycle.go
├── owner/
│   ├── owner.go                    # Owner config, ingress, upstream routing, status, teardown
│   ├── materialization.go           # Process generation/materialization authority
│   ├── resilient_client.go          # Shim reconnect and legacy replay seams
│   ├── handoff.go
│   └── handoff_from.go
├── serverid/serverid.go             # Stable identity and IPC-path inputs
├── snapshot/snapshot.go             # Snapshot descriptor/version IO
└── session/                         # Local token admission/reconnect registry

internal/mcpserver/server.go         # `mux_list` OwnerInfo projection where available
testdata/mock_server.go              # Existing legacy parity fixture
```

**Structure Decision**: Keep the existing Go CLI → engine/control → daemon → owner → upstream architecture. Add one narrow typed protocol-era selector at the pre-spawn boundary; propagate its confirmed result through existing control, identity, owner, lifecycle, and status seams. No parallel modern daemon, new process authority, or semantic adapter is introduced.

## R1 Boundary Contract

| Contract aspect | R1 rule |
| --- | --- |
| Caller input | A known host explicitly selects the pinned modern policy and sends a valid modern request. Required `_meta` contains a string `io.modelcontextprotocol/protocolVersion` of `2026-07-28` and an object `io.modelcontextprotocol/clientCapabilities`; `clientInfo` may be absent but, when present, is validated. |
| Successful output | Exactly one forced-isolated modern owner is elected only after a matching control echo. Its same-era upstream receives the original opening frame byte-for-byte, then native same-era traffic. |
| Unsafe input output | Invalid JSON/JSON-RPC receives the applicable JSON-RPC parse/invalid-request error; malformed required metadata receives `-32602`; a valid unsupported version receives `-32022` with `supported` and `requested`; a local control-era mismatch is a local fail-closed admission error, not automatically `-32022`. |
| Existing attachment points | `cmd/mcp-mux/main.go:main`, `muxcore/engine/engine.go:(*MuxEngine).Run/runClient`, `muxcore/control` request/response dispatch, `daemon.Spawn/spawnOnce`, `serverid.GenerateContextKey`, `owner.OwnerConfig/NewOwner`, and `owner.RunResilientClient`. |
| Explicit non-changes | No automatic `server/discover` probe or fallback, no legacy↔modern translation, no modern sharing/cache/template/replay/persistence, no mux-authored MRTR retry or re-listen, and no public compatibility fingerprint. |

## Integration Points and Source Map

| Requirement family | Existing candidate seam and mapped callers | R1 design action |
| --- | --- | --- |
| FR-001/003/005/006/008/009 admission | CLI `main`; `(*MuxEngine).Run`/`runClient`; `control.Request`/`Response`; `Daemon.HandleSpawn`/`Spawn`/`spawnOnce`; `GenerateContextKey`. LSP confirms consumers of `engine.New`, `MuxEngine.Run`, `control.Request`, and `GenerateContextKey`. | Select/validate before spawn, demand exact control echo, keep era separate from sharing, force isolation, and derive a safe modern-only identity without changing legacy bytes. |
| FR-002 legacy parity | Current `NewOwner`, legacy materialization, `sendProactiveInit`, cache/template paths, and `RunResilientClient` replay have established callers and legacy fixtures. | Leave legacy defaults and released paths intact; modern branches bypass rather than alter them. |
| FR-004/007/010/011 native modern routing | `Owner.handleDownstreamMessage`, `readUpstream`, `handleUpstreamMessageFromLocked`, `handleUpstreamRequest`, `routeToLastActiveSession`, `broadcast`, `sendRootsListChanged`, materialization, and resilient reconnect. | Reuse current single-recipient routing, request-ID remap, process-bound inflight/generation fences, and existing ephemeral session/progress/subscription state. Contain upstream JSON-RPC requests and never synthesize/broadcast logging or legacy traffic. Add no `CorrelationSet`, `RequestRoute`, `ProgressRoute`, or `SubscriptionRoute`; R2 owns shared causal correlation. |
| FR-012 lifecycle quarantine | `snapshot`/`daemon.snapshot`, handoff, `reaper`, `owner_lifecycle`, materialization, `FinalizeForRemoval`, and process-generation helpers. | Preserve existing generation/current-process/finalization gates; exclude era-less modern snapshot/handoff and use exact-era continuation, drain/removal, cold start, or fail-closed behavior. |
| FR-013/014 loss and reconnect | `onUpstreamExit`, materialization replacement, `RunResilientClient.reconnect/finishReconnect`, engine/CLI reconnect closures, session token history. LSP confirms `RunResilientClient` callers. | End current modern work once, suppress replay, require exact-era fresh admission or an explicit new-launch failure, and require host-issued new listen. |
| FR-015 observability | `Owner.Status`, `Daemon.HandleStatus`, `HandleListOwners`, `control.OwnerInfo`, CLI status, and `internal/mcpserver` `mux_list` where it projects `OwnerInfo`. LSP confirms the direct status/list callers. | Require the exact control-era echo plus `protocol_era`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine` in existing `OwnerInfo` projections. Preserve existing readiness fields. Do not add a registry descriptor schema/capability, `mux_engines` or topology contract, lifecycle-state taxonomy, or counter model. |

## Verification Design

The implementation phase must extend the named adjacent test suites and keep existing legacy fixtures as parity tests. It must add focused cases for:

1. pre-spawn pinned-modern admission with both a direct ordinary opener and a host-sent `server/discover`, byte-preserved opening forwarding, and absent optional `clientInfo` acceptance;
2. malformed/null/non-object metadata, malformed optional `clientInfo`, unsupported version, contradictory opener, and control-echo mismatch with the precise error split and zero upstream attach/start;
3. forced-isolated, cache/template/replay-off readiness; native `input_required`/opaque `requestState`; contained upstream requests; and sole-recipient request-scoped logging;
4. legacy bootstrap, result sequence, cache/replay behavior, and identity-byte parity;
5. every quarantine boundary: snapshot, handoff, reaper, zero-session removal, retry/respawn, daemon/upstream loss, reconnect, stale-generation delivery, and blocked whole-tree finalization;
6. the exact control echo plus minimal `OwnerInfo` agreement and redaction across direct owner status, daemon status/list, CLI status, and `mux_list` where it carries `OwnerInfo`; and
7. a built-deliverable customer run using the scenarios in `quickstart.md`.

## Design-Depth Check

This is a **D1 child feature plan** of the accepted root D2 architecture decision recorded at coordination root `.agent/arch/decisions/014-mcp-2026-07-28-protocol-era-boundary.md` (not part of the candidate source tree). The coordination record supplies parent context. This plan defines R1 from its own boundary contract, primary protocol revision, and candidate source map if that record is unavailable. The unit here is R1 only: an independently releasable feature that fits existing owner/daemon boundaries while carrying a real compatibility and lifecycle risk. Its required D1 artifacts are this boundary contract, the integration/source map, the verification plan, a LITE challenge, and one independent implementation-time checker. It does not create a new D2 ticket decomposition or program roadmap.

### Challenge-LITE

**Pre-correction challenger verdict: REVISE.**

- **Premise**: R1 needs a pre-election immutable era, forced isolation, native opening pass-through, and fail-closed lifecycle quarantine because current ingress elects an owner before reading the host opener and current owner/reconnect paths are legacy bootstrap/cache/replay shaped.
- **Chosen approach**: carry one typed immutable era through the existing daemon/owner flow before election. It reuses established process and lifecycle authority and keeps modern owners forced isolated.
- **Rejected alternative**: a dedicated direct modern subprocess path would duplicate or bypass the repository's single process-tree authority.
- **Deferred alternative**: a separate `ModernOwner` or shared collision-safe causal routing is reconsidered only if implementation proves the existing owner core cannot carry the narrow era gates. R2 owns shared correlation.

### Plan review receipt

| Review source | Incoming verdict | Finding | Correction disposition |
| --- | --- | --- | --- |
| Independent Plan checker | `CHANGES_REQUIRED` | Durable docs used broken candidate-relative coordination links and transient review-provenance references. Quickstart lacked a complete explicit SC mapping. | Replace the links with textual coordination-root citations, retain the official protocol revision and candidate source anchors, remove transient review-provenance references, and add the scenario-to-SC map. |
| LITE challenger | `REVISE` | Full registry/topology/counter observability and named route entities exceeded R1. | Limit readback to the exact control echo plus existing `OwnerInfo` projections, defer registry/topology/taxonomy/counters to R3, and replace named route entities with `ExistingEphemeralRouteState`; defer collision-safe shared correlation to R2. |

**Post-correction verification**: independent factual recheck returned **PASS** (`R1PlanRecheck`), and the second D1 Challenge-LITE returned **GO** (`R1PlanRechallenge`). The corrected package has no unresolved Plan-stage contract gap.

**Independent checker commitment**: before an implementation is accepted, one independent reviewer (not its implementer) must re-derive the selected-era/legacy-parity boundary from the pinned protocol source and run the completed `quickstart.md` scenario matrix against the exact built candidate.

## Complexity Tracking

No constitution violation requires justification. The design deliberately removes scope rather than adding an abstraction: one narrow era dimension, existing route/lifecycle reuse, and minimal readback preserve the current owner/daemon lifecycle authority. R2 sharing/correlation and R3 persistence/extended observability remain excluded.
