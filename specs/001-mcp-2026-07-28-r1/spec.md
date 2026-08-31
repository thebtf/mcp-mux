# Feature Specification: MCP 2026-07-28 R1 Native Isolation

**Feature Branch**: `feature/mcp-2026-07-28-r1`  
**Created**: 2026-08-31  
**Status**: Draft  
**Input**: User description: "A known MCP 2026-07-28 host can use mcp-mux natively through one forced-isolated owner while released legacy behavior remains unchanged."

## Purpose and release outcome

mcp-mux must let an operator connect a known Model Context Protocol (MCP) 2026-07-28 host to one native, same-era upstream through an isolated owner. The host receives native modern protocol behavior. Existing legacy users keep their released behavior.

R1 is a safety-contained release. It establishes a strict protocol-era boundary before owner selection, forwards the opening modern message unchanged, and prevents modern state from entering legacy lifecycle paths. R1 does not make modern traffic shareable, cacheable, persistent, or automatically recoverable across an unsafe lifecycle boundary.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Use a known modern host natively (Priority: P1)

An operator selects the R1 modern path for a known MCP 2026-07-28 host. The host sends a valid opening request and receives the upstream's native response through one dedicated modern owner.

**Why this priority**: This is the release outcome. Without it, R1 provides no usable modern-host path.

**Independent Test**: Run one known modern host against a same-era upstream. Record the opening message and all mux-generated traffic. The host completes one ordinary request through exactly one isolated owner.

**Acceptance Scenarios**:

1. **Given** a known modern host has selected the R1 modern path and sends valid required metadata, **When** it opens a session, **Then** mcp-mux selects one isolated modern owner before it starts or attaches an upstream, forwards the original opening message unchanged, and returns the upstream's native result.
2. **Given** the modern owner is ready, **When** the host sends ordinary native modern requests, **Then** the owner forwards those requests without mux-generated legacy initialization, list, cache, template, or replay traffic.
3. **Given** an existing legacy host uses the released legacy path, **When** it opens and uses a session, **Then** its externally observable behavior and identity remain unchanged.

---

### User Story 2 - Reject unsafe modern admission (Priority: P1)

An operator or host receives a clear refusal instead of an unsafe mixed-era session when the opening message cannot establish one valid supported modern era.

**Why this priority**: A wrong selection could route modern traffic through a legacy owner or start the wrong upstream. Safe refusal protects both the modern host and existing legacy users.

**Independent Test**: Submit missing, null, malformed, unsupported, conflicting, and era-mismatched opening metadata. Confirm that each case produces a refusal before an upstream is started or an existing owner is attached.

**Acceptance Scenarios**:

1. **Given** an opening request omits, nulls, malforms, or conflicts on required modern metadata, **When** mcp-mux evaluates the request, **Then** it rejects the request before unsafe owner selection and does not treat the request as legacy by default.
2. **Given** a selected modern request receives an absent, unknown, or mismatched era confirmation from the target control path, **When** the host attempts admission, **Then** mcp-mux fails closed and does not attach the host to a legacy owner.
3. **Given** a modern host encounters a legacy owner or legacy upstream identity, **When** it attempts to connect, **Then** mcp-mux refuses the mixed-era route rather than translating messages between protocol eras.

---

### User Story 3 - Keep modern work safe during lifecycle events (Priority: P2)

An operator can allow expected cleanup, restart, and reconnect events without a modern owner silently becoming legacy or replaying stale work.

**Why this priority**: R1 must be safe during lifecycle re-entry, not only on its first request. A controlled cold start or explicit refusal is safer than an ambiguous restore.

**Independent Test**: Exercise snapshot, handoff, reaper, zero-session removal, retry rehydration, respawn, daemon loss, upstream loss, and downstream reconnect for an isolated modern owner. `Retry rehydration` is the bounded restoration of retry-family identity, counter, and eligibility after lifecycle re-entry. Inspect the resulting owner era, existing ephemeral-state clearing after loss, and forwarded traffic.

**Acceptance Scenarios**:

1. **Given** a modern owner reaches a snapshot or live-handoff boundary without a safe explicit modern transfer, **When** the boundary is attempted, **Then** mcp-mux excludes the owner and cold-starts or fails closed instead of restoring it as legacy.
2. **Given** an isolated modern owner is removed, retried, or respawned, **When** a lifecycle path re-enters, **Then** it preserves the selected modern era or ends safely without replaying requests, retries, progress, or subscriptions.
3. **Given** the daemon survives while a modern upstream generation dies, **When** mcp-mux processes that loss, **Then** it clears existing ephemeral route state once, rejects old-route delivery and replay, and permits only a fresh exact-era admission.
4. **Given** a modern downstream reconnects after a loss or replacement, **When** it seeks readmission, **Then** it receives the exact selected era with existing ephemeral state cleared or an explicit failure that requires a new launch. It never attaches to a legacy owner or inherits a prior subscription state.

---

### User Story 4 - Inspect an isolated modern owner safely (Priority: P3)

An operator can use an existing owner, daemon, list, CLI, or `mux_list` projection that carries `OwnerInfo` to identify a modern R1 owner as isolated, cache-off, and subject to lifecycle quarantine without exposing request content, credentials, or identifiers that link users.

**Why this priority**: Operators need minimal truthful readback to diagnose an R1 boundary and make a safe rollback decision.

**Independent Test**: Inspect direct owner status, daemon status/list, CLI status, and `mux_list` when it carries `OwnerInfo`. Confirm the exact modern control echo separately. Verify the four required policy fields, any existing readiness field, runtime logging behavior, and redaction.

**Acceptance Scenarios**:

1. **Given** an isolated modern owner is active, **When** an operator reads an existing `OwnerInfo` projection, **Then** it reports `protocol_era=2026-07-28`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine`, retains any existing readiness field, and exposes no payloads, credentials, opaque state, or linkable compatibility material.
2. **Given** a valid request-scoped standard logging message is emitted on an isolated route after request opt-in, **When** mcp-mux receives it, **Then** mcp-mux forwards it to the sole downstream once and never synthesizes or broadcasts it. Logging is not required as a status field.
3. **Given** a modern upstream emits a JSON-RPC request toward the host, **When** mcp-mux receives it, **Then** mcp-mux does not forward that request to the downstream host.
### Edge Cases

- A first request with only part of the required modern metadata, a null metadata object, an unsupported version, or conflicting era signals is refused before it can select an owner.
- A modern host receives an absent, unknown, or mismatched era confirmation only as an explicit failed admission. It never receives an implicit legacy fallback.
- An unsafe lifecycle record without an explicit modern era is excluded. R1 cold-starts or fails closed rather than interpreting the record as legacy.
- If the daemon or upstream disappears while modern work is active, mcp-mux ends the current generation once and does not replay the request, retry, progress, or subscription.
- If a modern host reconnects after a loss, the host begins a fresh route. A former subscription does not resume until the host sends a new native listen request.
- If an upstream sends an unrouteable JSON-RPC request, mcp-mux contains it. It does not choose a downstream recipient by recency or broadcast it.

## Authority, terminology, and normative references

This specification follows MCP 2026-07-28 at the official source revision `5f5440bb26a62e2cf3440b92da5a667efa03b267`, cited in [`research.md`](research.md). The protocol defines modern behavior per request. It does not remove mcp-mux's local responsibilities for owner selection, process safety, admission, response delivery, lifecycle control, or truthful status.

- **Legacy**: The released MCP behavior selected by the legacy initialization path. Legacy remains the default for existing users.
- **Modern**: Native MCP 2026-07-28 behavior selected from valid per-request protocol metadata.
- **Protocol era**: The single selected legacy or modern behavior for one owner. It remains fixed for that owner's lifetime.
- **Isolated owner**: An owner that serves one downstream host and is never reused by another host in R1.
- **Native pass-through**: Same-era message behavior without legacy-to-modern or modern-to-legacy semantic conversion.
- **Cold start**: A new modern owner with no transferred live request, progress, retry, or subscription state.
- **R1 lifecycle quarantine**: The rule that an unsafe modern lifecycle transition must preserve the exact selected era, cold-start, or fail closed. It must never downgrade a modern owner to legacy.
- **Retry rehydration**: The bounded restoration of retry-family identity, counter, and eligibility after lifecycle restore or re-entry. It requires an explicit modern era; an era-less candidate cold-starts or fails closed and never reconstructs a legacy default.

## Compatibility matrix: host era × upstream era × policy

| Downstream host | Upstream era | R1 policy | R1 outcome |
| --- | --- | --- | --- |
| Existing legacy host | Legacy | No explicit R1 modern policy | Supported. Released legacy behavior remains unchanged, and legacy identity is byte-identical to the released baseline. |
| Known MCP 2026-07-28 host | MCP 2026-07-28 | Explicit modern selection with forced isolation | Supported. One dedicated native modern owner handles the host. Modern cache, template, and replay behavior are off. |
| Known MCP 2026-07-28 host | Existing legacy owner or legacy upstream identity | Explicit modern selection | Unsupported. Refuse the route. R1 does not translate between eras. |
| Existing legacy host | Modern owner or modern upstream identity | Any R1 selection | Unsupported. Refuse the route. R1 does not convert legacy traffic to modern traffic. |
| Known MCP 2026-07-28 host | Unknown, absent, or control-era-mismatched upstream identity | Explicit modern selection | Unsupported. Refuse before unsafe attachment or fallback. |
| Dual-era host that discovers then initializes on the same shim | Any | Automatic fallback | Unsupported. The operator uses an explicit known-legacy or known-modern path. |
| Any R1 host/upstream/policy combination omitted above | Any | Explicit modern selection | Unsupported. Refuse fail-closed before unsafe attachment. |

## Public API contract: library, CLI, control, and minimal readback

R1 gives a known modern host an explicit, documented selection path. Existing callers that do not select modern support retain the legacy default. The product must not repurpose historical sharing terminology as a protocol-era selection.

A modern selection is accepted only after the target control path confirms the exact selected era. Existing owner status, daemon status/list, CLI status, and `mux_list` projections that carry `OwnerInfo` expose the four R1 policy facts. Existing readiness fields retain their current meaning where present. R1 does not require a logging status field or a registry, `mux_engines`, topology, lifecycle-state, or counter contract. Readback remains redacted and does not disclose request bodies, tokens, environment values, opaque request state, tenant or authorization values, compatibility keys, digests, or linkable fingerprints.

## Owner identity and pre-spawn compatibility contract

R1 selects the protocol era before it starts, reuses, or attaches an owner. A malformed or ambiguous selection is rejected at that boundary.

Protocol era and sharing policy are separate facts. In R1, every selected modern host receives one isolated owner. No command similarity, working context, prior legacy owner, or post-connect authorization decision may turn a modern R1 owner into a shared owner.

Legacy identity is byte-identical to the released baseline. Modern identity remains distinct from legacy and safe on every supported local platform. Unknown or malformed era values never fall through to legacy.

## Legacy native path

The released legacy path remains native legacy behavior. It retains the released initialization, cache, template, replay, reconnect, lifecycle, and identity semantics.

R1 must not change a legacy host's observed request sequence, result sequence, or identity solely because modern support exists. A modern feature failure must not alter a legacy route.

## Modern native path, isolated logging, and directionality

A valid selected modern opening message reaches its same-era upstream unchanged. R1 injects no legacy `initialize` or `initialized` message before or after that opening message. R1 also injects no mux-generated legacy list, roots, or list-change traffic, cache response, template response, or reconnect replay into a modern upstream.

The R1 modern owner is isolated. After request opt-in, mcp-mux MUST forward a valid request-scoped standard log to its sole downstream once. It never synthesizes or broadcasts that log. R1 does not claim a shared modern logging route.

Modern MCP does not allow an upstream JSON-RPC request to become a downstream host request. If an upstream sends such a request, mcp-mux contains it and does not forward it through legacy callback, last-active, or broadcast behavior.

## Causal correlation: existing single-recipient state and loss cleanup

R1 does not claim shared modern correlation. It reuses current single-recipient routing, request-ID remap, process-bound inflight and generation fences, and existing ephemeral session, progress, and subscription state. `ExistingEphemeralRouteState` is a conceptual name for that reuse. It is not a new map, API, or route authority.

After a modern loss, mcp-mux clears existing ephemeral state and suppresses replay of open requests, multi-round-trip retries, progress, and subscriptions. A host that needs to continue after reconnect sends a fresh native request. A host that needs a subscription sends a new `subscriptions/listen` request and receives a new acknowledgement. R1 does not create, replay, or continue a subscription on the host's behalf. Opaque request state remains opaque. R1 neither interprets it nor creates a retry lineage. Collision-safe shared causal correlation is deferred to R2.

## Lifecycle: R1 quarantine; generation, respawn, reconnect, snapshot, handoff, reaper; host re-listen

R1 applies lifecycle quarantine to every named re-entry path for a modern owner.

| Lifecycle event | Required R1 result | Prohibited result |
| --- | --- | --- |
| Snapshot export or restore | Exclude modern state from the current era-less payload. A later modern launch cold-starts or fails closed. | Restoring an omitted modern era as legacy or adding R1 schema/versioning to persist it. |
| Live handoff | Refuse the unsafe era-less transfer and cold-start or fail closed. | Attaching a modern owner through a legacy handoff or adding R1 transfer persistence. |
| Reaper or zero-session removal | Drain and remove the modern owner. | Rebuilding it from an implicit legacy default. |
| Retry rehydration or respawn | Retry rehydration restores retry-family identity, counter, and eligibility only with the exact explicit modern era. Respawn preserves that era, cold-starts, or fails closed. | Reconstructing modern work as legacy. |
| Daemon or upstream loss | End the current modern generation once and clear live work. If the daemon survives an upstream-generation loss, only a fresh exact-era admission may continue. | Replaying requests, retries, progress, or subscriptions. |
| Downstream reconnect | Admit the host only to the exact selected era after clearing existing ephemeral state, or require a new explicit launch. | Reattaching to a legacy owner or reusing old ephemeral state. |

The R1 quarantine remains active until a separately released lifecycle contract proves safe same-era persistence and handoff. A later release must not weaken the R1 safety result by treating absent era information as legacy.

## Security, redaction, and authorization partitioning

The product must reject invalid era selection before unsafe owner election. Required R1 control and OwnerInfo readback must redact sensitive values. The product must not expose raw credentials, raw environment values, request content, opaque request state, tenant or authorization values, compatibility keys, digests, or linkable identifiers through those readbacks.

## Minimal admission and rollback readback

The exact modern control echo proves admission. Each existing owner status, daemon status/list, CLI status, or `mux_list` projection that carries `OwnerInfo` must expose these facts for an active R1 owner:

- `protocol_era=2026-07-28`;
- `sharing_policy=forced-isolated`;
- `cache_policy=off`; and
- `lifecycle_policy=r1-quarantine`.

Where a projection already exposes readiness, it retains that readiness field with its existing meaning. Standard logging remains a tested runtime behavior, not a mandatory readback field. R1 does not add a registry descriptor schema or capability, a `mux_engines` or topology contract, a lifecycle-state taxonomy, or a safe-counter model. Those observability extensions are deferred to R3.

Readback must not expose request content or private compatibility material.

## Consumer migration, release, and rollback

Existing legacy consumers require no configuration or behavior change. A known modern consumer explicitly selects the R1 modern path and supplies valid modern request metadata. R1 documentation must distinguish protocol era from historical owner-sharing choices and disclose R1 isolation, cache-off behavior, request-scoped logging, lifecycle quarantine, host re-listen requirements, and unsupported automatic fallback.

Implementation prepares the candidate-owned [`consumer-handoff.md`](consumer-handoff.md) artifact for `aimux`, `engram`, and other `muxcore` consumers. It does not publish or send that handoff externally. External consumer coordination remains a release-stage boundary.

Release acceptance requires customer-facing proof from a built deliverable: one known modern host completes native same-era traffic, one legacy host retains released behavior, and the candidate records the prepared consumer handoff plus a rollback path.

To roll back, operators stop admitting new modern owners and drain or remove existing modern owners through the quarantine path. Rollback never converts live modern work to legacy, hands it to a legacy owner, or replays unfinished modern traffic.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001 (MPE-CORE-001)**: The product MUST assign one protocol era before owner selection, keep that era immutable for the owner's lifetime, and retain legacy as the default for existing users.
- **FR-002 (MPE-CORE-002)**: The product MUST preserve released legacy native behavior and a legacy identity byte-identical to the released baseline.
- **FR-003 (MPE-CORE-003)**: The product MUST keep protocol era separate from sharing policy. Every R1 modern owner MUST be forced isolated.
- **FR-004 (MPE-CORE-004)**: The product MUST provide native same-era behavior only. It MUST NOT translate methods, results, errors, initialization behavior, or callbacks between legacy and modern eras.
- **FR-005 (MPE-R1-001)**: The product MUST validate all required modern opening metadata, including a supported protocol version and valid client-capability declaration. Missing, null, non-object, malformed, unsupported, conflicting, or ambiguous modern metadata MUST be rejected before an unsafe upstream start or owner attachment.
- **FR-006 (MPE-R1-002)**: After valid modern selection, the product MUST forward the opening modern message to the same-era upstream unchanged and without inserting, removing, or rewriting a message ahead of it.
- **FR-007 (MPE-R1-003)**: A modern R1 owner MUST emit no mux-generated legacy `initialize`, `initialized`, list, roots, list-change, cache, template, or replay traffic. Modern response caching, template reuse, and reconnect replay MUST remain off.
- **FR-008 (MPE-R1-004)**: The product MUST carry era selection through admission strictly. A modern host MUST receive an exact modern era confirmation. An absent, unknown, or mismatched confirmation MUST fail closed.
- **FR-009 (MPE-R1-005)**: The product MUST keep modern and legacy owner identities distinct through every admission path, use an identity safe on supported local platforms, and keep legacy identity byte-identical to the released baseline.
- **FR-010 (MPE-R1-006)**: The product MUST NOT forward a modern upstream JSON-RPC request to a downstream host through a legacy callback, last-active recipient, or broadcast path.
- **FR-011 (MPE-R1-007)**: When a valid request-scoped standard logging message is emitted on an isolated route after request opt-in, the product MUST forward it to the sole downstream once. It MUST NOT synthesize or broadcast it.
- **FR-012 (MPE-R1-008)**: The product MUST exclude a modern R1 owner from every snapshot or handoff path that cannot carry an explicit safe modern era. The resulting action MUST cold-start or fail closed. It MUST NOT restore or attach that owner as legacy. R1 does not add a snapshot/handoff era field or schema version.
- **FR-013 (MPE-R1-009)**: For reaper, zero-session removal, retry rehydration, respawn, daemon loss, and upstream loss, the product MUST preserve the exact selected modern era or drain, cold-start, or fail closed. Retry rehydration is bounded restoration of retry-family identity, counter, and eligibility after lifecycle re-entry; it requires explicit modern era and MUST NOT reconstruct a legacy default. The product MUST NOT replay a request, multi-round-trip retry, progress update, or subscription.
- **FR-014 (MPE-R1-010)**: A modern downstream reconnect MUST receive the exact selected era with existing ephemeral state cleared or an explicit failure requiring a new launch. When a daemon survives an upstream-generation loss, the next successful route MUST be fresh exact-era admission with no old route/replay. It MUST NOT attach modern work to a legacy owner or reuse prior subscription state.
- **FR-015 (MPE-OBS-001)**: A successful modern spawn control response MUST echo the exact selected era. Each existing owner status, daemon status/list, CLI status, or `mux_list` projection that carries `OwnerInfo` MUST expose `protocol_era=2026-07-28`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine`. Existing readiness fields MUST retain their current meaning where present. R1 MUST NOT add a registry descriptor schema or capability, a `mux_engines` or topology contract, a lifecycle-state taxonomy, or a safe-counter model. Required R1 readbacks MUST NOT expose secrets, payloads, opaque state, keys, digests, or linkable fingerprints.
- **FR-016 (MPE-OBS-002)**: Public R1 documentation MUST distinguish protocol era from historical sharing flags and state R1 isolation, logging behavior, host re-listen behavior, exclusions, and safe fallbacks.
- **FR-017 (MPE-OBS-003)**: The release candidate MUST produce built-deliverable customer evidence for one known modern host and one legacy host, plus a candidate-owned consumer-handoff preparation and rollback record. External consumer handoff publication remains release-stage work.

### Key Entities

- **Protocol era**: The fixed legacy or MCP 2026-07-28 behavior selected for one owner.
- **Opening message**: The first host message used to establish a valid era before owner selection. For a modern owner, it reaches the upstream unchanged.
- **R1 isolated modern owner**: A dedicated modern owner with one downstream recipient, cache-off behavior, and lifecycle quarantine.
- **Retry rehydration**: Bounded restoration of retry-family identity, counter, and eligibility after lifecycle re-entry. It requires explicit modern era and never supplies a legacy default.
- **Lifecycle-quarantine outcome**: A truthful safe result for an unsafe modern lifecycle boundary: exact-era continuation, cold start, drain and removal, or explicit refusal.
- **Operational readback**: The exact control echo plus minimal redacted `OwnerInfo` facts that identify era, forced isolation, cache policy, and lifecycle policy. Existing readiness fields retain their current meaning where present.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The deterministic acceptance corpus contains at least 100 valid known-modern opening frames spanning direct and host-sent `server/discover`, absent and present `clientInfo`, JSON property ordering, and whitespace variants. Every accepted frame reaches exactly one isolated same-era upstream with byte-for-byte identical opening-message content.
- **SC-002**: In the malformed and ambiguous admission scenario, 100% of inputs are refused before an unsafe upstream is started or an existing owner is attached, with zero modern-to-legacy fallthroughs.
- **SC-003**: In the released legacy reference scenario, 100% of compared legacy request sequences and result sequences match the released baseline, and every compared legacy identity is byte-identical to the released baseline.
- **SC-004**: Across snapshot, handoff, reaper, zero-session removal, retry rehydration, respawn, daemon loss, upstream loss, and reconnect scenarios, zero modern owners restore or attach as legacy, and zero stale requests, retries, progress updates, or subscriptions are replayed.
- **SC-005**: In the modern native-path scenario, mcp-mux generates zero legacy initialization, list, list-change, cache, template, or replay messages. After request opt-in, every valid request-scoped standard log reaches the sole recipient once, and zero upstream JSON-RPC requests reach a downstream host.
- **SC-006**: In every required owner status, daemon status/list, CLI status, and `mux_list` projection that carries `OwnerInfo`, 100% of inspected records agree on `protocol_era=2026-07-28`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine`, preserve any existing readiness field, and expose zero prohibited sensitive or linkable values.
- **SC-007**: Before release, one known modern host completes a native request through the built deliverable, one legacy host completes its released reference flow without a behavior change, and the candidate records a consumer-handoff preparation and rollback record.

## Assumptions

- The R1 operator knows whether the downstream host and intended upstream use MCP 2026-07-28 and selects the modern path explicitly.
- The known modern host sends valid newline-delimited MCP messages and required modern request metadata.
- The released legacy behavior and identity are the compatibility baseline for this feature.
- A controlled cold start or explicit refusal is acceptable when R1 cannot preserve modern era safely through a lifecycle boundary.
- Because every R1 modern owner is isolated, standard logging has one possible downstream recipient.

## Out of scope

R1 explicitly excludes the following work:

- Modern owner sharing or reuse across two downstream hosts.
- Modern response caching, template reuse, cache invalidation, or cache-policy design.
- Semantic translation between legacy and MCP 2026-07-28 traffic.
- Automatic same-shim `server/discover`-to-`initialize` fallback.
- Transparent subscription continuity, automatic re-listen, or replay after a loss or reconnect.
- Shared modern logging attribution or delivery.
- Persisted modern snapshot or live handoff support, including R1 era/schema versioning, before a separately released versioned same-era contract.
- R2 causal multiplexing, R3 persisted-lifecycle behavior, and R3 registry, `mux_engines`, topology, lifecycle-state, and counter observability.
- Reinterpreting historical sharing terminology as a protocol-era option.

## Verification scenarios and acceptance evidence

The following scenarios define future acceptance evidence. They are not evidence that implementation has already occurred.

| Scenario | Exercise | Observable result |
| --- | --- | --- |
| Native modern opening | Run every frame in the deterministic acceptance corpus through a known modern host and same-era upstream. | Each accepted frame selects one isolated modern owner. The opening message is byte-for-byte unchanged, and the host receives a native result. |
| Legacy parity | A released legacy fixture runs before and after R1. | Legacy request and result sequence match exactly. Legacy identity is byte-identical to the released baseline. |
| Strict admission | Inputs cover missing, null, malformed, unsupported, conflicting, and ambiguous modern metadata. | Each input is rejected before unsafe start or attachment. No input falls through to legacy. |
| Era confirmation | The target control path returns exact, absent, unknown, and mismatched era confirmations. | Only exact modern confirmation admits the modern host. Every other outcome fails closed. |
| Modern readiness | An isolated modern owner receives traffic after opening. | No mux-generated legacy initialization, list, roots, list-change, cache, template, or replay traffic appears. |
| Directionality and logging | The upstream emits a valid request-scoped standard log after opt-in and a JSON-RPC request. | The log reaches the sole downstream once without synthesis/broadcast. The JSON-RPC request never reaches a downstream host. |
| Lifecycle quarantine | Snapshot, handoff, reaper, zero-session removal, retry rehydration, respawn, daemon loss, upstream loss, and reconnect are exercised. | Every outcome preserves exact era, cold-starts, drains and removes, or fails closed. If the daemon survives upstream loss, the next route is fresh exact-era admission with cleared state. No modern owner becomes legacy and no live work replays. |
| Minimal R1 readback | Inspect direct owner status, daemon status/list, CLI status, and `mux_list` projections that carry `OwnerInfo` during normal, removal, and refusal outcomes. | The four required policy facts agree. Existing readiness fields retain their current meaning. No prohibited sensitive/linkable material, registry/capability, `mux_engines`/topology contract, lifecycle taxonomy, or counter model appears. |
| Built-deliverable customer proof | An operator uses the release deliverable with one modern and one legacy host and prepares the consumer handoff. | The modern host completes a native request, the legacy host retains released behavior, and the candidate contains handoff preparation without external publication. |

## Open authority decisions and falsifiers

No clarification marker remains for R1. The accepted R1 defaults are explicit modern selection for a known modern host, forced isolation, cache-off behavior, lifecycle quarantine, sole-recipient logging, and host-issued re-listen after loss.

The following decisions remain outside R1 rather than blocking it:

- A required host may later prove that automatic same-shim discovery fallback is necessary. Until then, R1 does not claim that fallback.
- Modern sharing remains deferred until a trusted pre-selection compatibility and authorization-partition contract exists.
- Shared modern logging remains deferred until an authoritative causal attribution rule exists.
- Persisted modern lifecycle transfer remains deferred until a versioned same-era contract exists.
- Transparent subscription continuation remains deferred until a separately authorized user and lifecycle contract exists.

R1 fails its acceptance boundary if any modern owner attaches or restores as legacy, if any unsafe modern opening starts or attaches an upstream, if modern traffic receives mux-generated legacy bootstrap or replay traffic, if a modern upstream JSON-RPC request reaches a downstream host, if a modern lifecycle loss replays prior live work, or if legacy observable behavior changes.