# Phase 1 Data Model: R1 Native Modern Isolation

## Modeling rule

R1 models **protocol era**, **sharing policy**, and **local lifecycle control** as independent dimensions. MCP `2026-07-28` statelessness affects wire semantics; it does not erase daemon ownership, process generations, local token admission, request routing, or whole-tree retirement.

The names below are caller-first design recommendations, not frozen exported identifiers. Any eventual public name must preserve the listed behavior, use additive evolution, and retain a zero-valued legacy default.

## Caller-first flows

### A. Explicit known-modern caller

1. A caller selects the pinned modern policy before writing its first MCP frame.
2. The ingress buffer reads one newline-delimited frame, parses just enough to validate modern request metadata, and retains the original bytes.
3. The daemon accepts a spawn/attach only when control echoes the exact modern era. It creates one isolated modern owner; it never selects a legacy owner or cache/template owner.
4. The original frame is written to the same-era upstream unchanged. No mux-generated `initialize`, `initialized`, list, cache, template, or replay frame precedes it.
5. The caller receives native response/MRTR/eligible notification traffic on its sole route. It may issue a fresh request after a loss; it must issue a new `subscriptions/listen` after reconnect.

### B. Existing legacy caller

1. A caller supplies no new policy value.
2. The public zero value selects released legacy behavior.
3. Existing identity bytes, bootstrap, cache/template/replay, sharing, reconnect, and lifecycle semantics are unchanged.

### C. Operator

1. The operator reads an existing direct owner status, daemon status/list, CLI status, or `mux_list` projection that carries `OwnerInfo`.
2. A modern record reports `protocol_era=2026-07-28`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine`. Existing readiness fields remain unchanged where present.
3. The operator can drain/remove a modern owner for rollback, but cannot cause it to become legacy, reuse old ephemeral state, or expose private compatibility material.

## Independent state domains

| Domain | Lifetime and authority | R1 representation | Must not be confused with |
| --- | --- | --- | --- |
| Protocol era | Fixed before owner election; immutable for owner lifetime | `Legacy` or `Modern20260728` | Sharing mode, an upstream version guess, or an empty string that means legacy |
| Public protocol policy | Caller configuration before the opening frame | `LegacyOnly` (zero) or pinned `Modern20260728` | Historical `cwd`/`git`/`global`/`isolated` sharing flags |
| Opening request metadata | Per modern request | Required version + capabilities; optional validated client info | Owner-level authorization, cache key, or durable identity |
| Sharing policy | Daemon-owned admission result | Released legacy behavior, or `ForcedIsolated` for R1 modern | The wire protocol era |
| Physical owner identity | Daemon/serverid input before IPC bind | Byte-stable legacy identity; domain-separated filesystem-safe modern identity | A public compatibility fingerprint or an authorization key |
| Owner lifecycle | Local daemon/owner authority | Owner, process generation, materialization and finalization states | An MCP protocol session |
| Existing ephemeral route state | In-memory, generation-scoped | Current single-recipient routing, request-ID remap, process-bound inflight/generation fences, and existing ephemeral session/progress/subscription state | A new R1 map, API, correlation authority, or persisted MRTR lineage |
| Operational readback | Existing direct `OwnerInfo` projections | Four R1 policy facts plus existing readiness fields where present | Request content, credentials, tokens, opaque state, or digests |

## Entities and types

### 1. `ProtocolEra`

| Value | Wire/behavioral meaning | Validation and invariants |
| --- | --- | --- |
| `Legacy` | Released initialization-based behavior | Deliberately zero-valued for source compatibility. It is selected only by legacy policy/path, never by an invalid modern attempt. |
| `Modern20260728` | Native MCP `2026-07-28` behavior | Only the exact supported wire version parses to this value. It is immutable after admission. |

**Invariants**:

- Unknown, omitted, malformed, or mismatched era values are errors, not aliases for `Legacy`.
- Era is an explicit input to identity, owner configuration, control echo, status, and each lifecycle decision.
- An owner cannot transition from one era to another.

### 2. `ProtocolPolicy`

| Value | Caller intent | R1 behavior |
| --- | --- | --- |
| `LegacyOnly` | Preserve released path | Default/zero value; existing behavior unchanged. |
| `Modern20260728` | Known host requests pinned modern route | Validate the first request before election; force isolated owner; no fallback. |

`Auto`, `Probe`, `DualSameEra`, or an era encoded in sharing mode are not R1 policy values. A later release may introduce a separately designed policy only with a new explicit contract.

### 3. `ModernRequestMeta`

| Field | Required | Validation | Ownership |
| --- | --- | --- | --- |
| `protocolVersion` | Yes | Non-empty string exactly `2026-07-28` for R1 admission | Per request; never inferred from prior frames |
| `clientCapabilities` | Yes | Object; `{}` is valid | Per request; mcp-mux does not invent capability support |
| `clientInfo` | No | If present, implementation-shaped object with string `name` and `version` | Display/debug metadata only; never sharing/auth/security input |
| `logLevel` | No | Parsed only to honor an existing request-scoped logging route | Does not select owner or recipient |

The selector parses enough to establish this model, then forwards accepted opening bytes verbatim. It does not normalize or reserialize the opening frame.

### 4. `OpeningFrame`

| Field | Meaning | Lifetime |
| --- | --- | --- |
| `raw` | Exact newline-delimited bytes received from the host | Transfer once into the accepted upstream write, then discard |
| `jsonrpcKind` | Request / notification / response classification needed for admission | Ephemeral; no durable copy |
| `method` | Admission-relevant only to reject contradictory legacy/modern claims | Ephemeral; method semantics remain upstream-owned |
| `modernMeta` | Validated `ModernRequestMeta` if pinned modern was selected | Ephemeral; never becomes an authorization/cache identity |

**Opening state**: `Unread → Buffered → ValidatedModern | Rejected`. Only `ValidatedModern` can produce a modern admission request. There is no `Buffered → Legacy` fallback for explicit modern policy.

### 5. `AdmissionRequest` and `AdmissionDecision`

| Entity | Fields | Rule |
| --- | --- | --- |
| `AdmissionRequest` | protocol policy, selected era, opening frame ownership, command/args/CWD/effective environment, existing sharing inputs | Built before owner election. It contains no raw client identity or opaque request state as a compatibility key. |
| `ControlEcho` | selected era, resulting owner path/ID/token on success, local error on refusal | Modern ingress accepts only an exact `Modern20260728` echo. |
| `AdmissionDecision` | era, sharing disposition, identity, owner target, cache policy, lifecycle policy | Modern decision is always `ForcedIsolated`, `CacheOff`, and `R1Quarantine`. |

Admission states:

```text
NewShim
  -> OpeningBuffered
  -> ModernMetadataValid
  -> ControlEchoExact
  -> ForcedIsolatedOwnerSelected
  -> Connected

NewShim -> RejectedBeforeElection
OpeningBuffered -> RejectedBeforeElection
ControlEchoExact? no -> RejectedBeforeAttach
```

An absent/unknown/mismatched echo cannot produce `Connected`; it leaves no upstream start/attach attributable to that frame.

### 6. `OwnerIdentity`

| Field | R1 rule |
| --- | --- |
| `legacyBytes` | Existing `serverid.GenerateContextKey` output is unchanged for every legacy input. |
| `eraDomain` | Modern identity includes a typed, domain-separated modern discriminator before physical endpoint derivation. |
| `physicalComponent` | Opaque/encoded and valid on Windows and Unix filesystem/socket naming rules. |
| `publicProjection` | Status may identify an owner by existing safe ID conventions but must not expose a compatibility hash, authorization partition, or raw input material. |

`OwnerIdentity` ensures modern and legacy identities cannot collide through exact lookup, shared lookup, retry-counter rehydration, reconnect, or lifecycle re-entry. It does not grant reuse: forced isolation is a separate decision.

### 7. `R1ModernOwner`

| Field | Semantics |
| --- | --- |
| `era` | Immutable `Modern20260728`. |
| `sharing` | Exactly `ForcedIsolated`; one downstream recipient maximum. |
| `cachePolicy` | `Off`; no hydration, template, response cache, cache invalidation, or replay. |
| `bootstrapPolicy` | `NativePassThrough`; no synthetic legacy handshake/lists/roots/list change. |
| `loggingBehavior` | Existing request-scoped logging may reach the sole recipient. It is runtime behavior, not an `OwnerInfo` field. |
| `lifecyclePolicy` | `R1Quarantine`. |
| `generation` | Current local process generation, governed by existing current-process and finalization fences. |
| `existingEphemeralRouteState` | Existing single-recipient routing and ephemeral session/progress/subscription state. This is a conceptual reuse label, not a new stored field. |

The owner keeps the existing daemon/owner/process authority. R1 does not create a second modern daemon or owner tree.

### 8. `ProcessGeneration` and `ExistingEphemeralRouteState`

| Entity | R1 rule |
| --- | --- |
| `ProcessGeneration` | The existing monotonically current generation remains the concrete process/tree authority. Only the exact current process may accept a response, cancellation, or cache-side effect. `RetirementProven` remains the gate before release or replacement. |
| `ExistingEphemeralRouteState` | The existing single-recipient routing, request-ID remap, process-bound inflight/generation fences, and ephemeral session/progress/subscription state remain in their current owners. R1 clears that existing state at terminal loss and suppresses replay. |

`ExistingEphemeralRouteState` is not a new map, API, or correlation authority. R1 does not introduce `CorrelationSet`, `RequestRoute`, `ProgressRoute`, or `SubscriptionRoute`. Collision-safe shared causal correlation is R2 work.

### 9. Native MRTR and subscriptions

`input_required`, `inputRequests`, `inputResponses`, result types, and opaque `requestState` remain native protocol data. A host retry has a new JSON-RPC ID and is ordinary fresh traffic. R1 does not inspect, persist, cache, or bind opaque `requestState` as mux retry lineage.

After terminal loss or reconnect, existing ephemeral request, progress, and subscription state is cleared. The host sends a new `subscriptions/listen` request when it needs a subscription. R1 does not define a `WorkState`, `SubscriptionState`, or subscription state machine, and it does not authorize automatic re-listen or replay.

### 10. Quarantine disposition

An existing lifecycle path may only preserve the exact modern era in a safe in-memory continuation, drain and remove, cold-start after a fresh explicit modern admission, or fail closed. The following rules describe dispositions, not a new persisted type, registry record, public lifecycle-state taxonomy, or counter model.

| Disposition | Meaning | Permitted boundaries |
| --- | --- | --- |
| Exact-era in-memory continuation | An existing generation/retry mechanism continues only with the immutable modern era and no-legacy policy. | Narrow respawn/retry only when all exact-era facts remain present. |
| Drain and remove | Existing activity, CAS, and finalization gates remove the owner after whole-tree retirement proof. | Reaper, zero-session, rollback, or unsupported continuation. |
| Cold start | A later explicit modern launch begins with no transferred modern route, cache, template, token, snapshot, or handoff data. | Snapshot/handoff exclusion or safe restart path. |
| Explicit refusal | No safe route exists; operator/host receives explicit failure. | Mismatched control, era-less transfer, unsafe reconnect, or unsupported lifecycle state. |

`RestoreAsLegacy`, `AttachToLegacy`, `PersistLiveRoute`, `ReplayRequest`, `ReplayMRTR`, `ReplayProgress`, `ReplaySubscription`, and `AutoRelisten` are illegal outcomes for a modern owner.

### 11. `OwnerInfo` readback

| Public field | Modern R1 value | Never contains |
| --- | --- | --- |
| `protocol_era` | `2026-07-28` | Raw opening/request data |
| `sharing_policy` | `forced-isolated` | Authorization decision inputs |
| `cache_policy` | `off` | Cache key/value, template content, digest |
| `lifecycle_policy` | `r1-quarantine` | Snapshot/handoff internal payload |
| Existing readiness field | Existing value and meaning, where the projection already provides it | Opaque request state or client identity |

Existing owner status, daemon status/list, CLI status, and `mux_list` projections that carry `OwnerInfo` share the four R1 policy facts. The successful control response separately echoes the exact selected era. R1 adds no logging field, registry descriptor schema/capability, `mux_engines` or topology contract, lifecycle-state taxonomy, or safe-counter model.

### 12. `AdmissionError`

| Category | Wire/local result | Side-effect boundary |
| --- | --- | --- |
| `MalformedFrame` | JSON-RPC parse or invalid-request error | No owner election |
| `InvalidModernParams` | `-32602` | No owner election |
| `UnsupportedModernVersion` | `-32022`, `supported` + `requested` | No owner election or fallback |
| `ConflictingEraSignals` | Explicit local R1 refusal | No owner election |
| `ControlEraMismatch` | Explicit local admission failure | No attach; do not falsely reclassify as `-32022` |
| `UnsafeLifecycleBoundary` | Cold start or explicit local failure | No legacy restore/attach/replay |
| `ContainedUpstreamRequest` | Existing redacted diagnostic where the path provides one; no downstream JSON-RPC request | No last-active/broadcast delivery |

## Relationships and ownership

```text
ProtocolPolicy
  -> OpeningFrame
  -> AdmissionRequest
  -> ControlEcho
  -> AdmissionDecision
  -> R1ModernOwner
  -> ProcessGeneration
  -> ExistingEphemeralRouteState (existing, single-recipient, generation-scoped)
  -> OwnerInfo readback

LifecycleEvent
  -> existing lifecycle authority
  -> ExistingEphemeralRouteState clearing or a quarantine disposition
```

- `ProtocolPolicy` belongs to the caller configuration.
- `OpeningFrame` belongs to ingress until it is forwarded or rejected.
- `AdmissionDecision`, owner identity, and sharing policy belong to the daemon before start/attach.
- `R1ModernOwner`, generation, and existing ephemeral-state clearing remain with the owner/process lifecycle.
- `OwnerInfo` readback is a read-only projection, not a source of election/reuse authority.

## Persistence boundary

R1 stores no modern owner or route state in snapshots or handoff payloads. R1 does not add a registry descriptor schema/capability or durable status/counter record. Any later persisted descriptor requires an explicit versioned same-era contract and is outside this data model.

## Recommended module ownership

| Module | Planned responsibility | Out of scope in R1 |
| --- | --- | --- |
| new narrow era selector package | Typed era/policy parse; one-frame buffer; strict metadata validation; untouched opening bytes | Daemon election, cache, auth, lifetime management |
| `cmd/mcp-mux` and `muxcore/engine` | Invoke the same selector before spawn and preserve selection through reconnect policy | Duplicate selection logic or fallback inference |
| `muxcore/control` | Additive exact-era request/response echo and rejection | Interpret host protocol traffic or expose private compatibility material |
| `muxcore/serverid` | Filesystem-safe modern domain separation while preserving legacy bytes | Reuse authorization |
| `muxcore/daemon` | Compare era before any attach/reuse; force modern isolation; preserve/reject era on lifecycle entry; project minimal `OwnerInfo` fields through existing status/list seams | R2 sharing/correlation or R3 persistence/observability |
| `muxcore/owner` | Native modern readiness, directionality containment, existing ephemeral-state clearing, and minimal `OwnerInfo` values | Translation, legacy traffic injection, automatic host retry/re-listen, or a new route authority |
| `cmd/mcp-mux` and `internal/mcpserver` | Preserve existing `OwnerInfo` values through CLI status and `mux_list` where that projection exists | Registry descriptor changes, `mux_engines`, topology, state taxonomy, or counter model |
| snapshot/handoff/reaper/lifecycle | Apply the quarantine disposition using existing process authority | Versioned modern transfer |
