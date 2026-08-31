# Phase 0 Research: MCP 2026-07-28 R1 Native Isolation

## Authority, candidate, and evidence labels

- **Candidate**: `feature/mcp-2026-07-28-r1` at `939aa694185f3242d755601997da404ef53d41e9`; `cee5444` is confirmed ancestry.
- **Protocol authority**: [MCP `2026-07-28` source revision `5f5440bb26a62e2cf3440b92da5a667efa03b267`](https://github.com/modelcontextprotocol/modelcontextprotocol/commit/5f5440bb26a62e2cf3440b92da5a667efa03b267), accessed 2026-08-31.
- **Coordination context**: coordination root `.agent/specs/mcp/2026-07-28/normative-contract.md`, `.agent/arch/decisions/014-mcp-2026-07-28-protocol-era-boundary.md`, `.agent/plans/mcp-2026-07-28-adaptation-plan.md`, and `.agent/reports/2026-08-31-mcp-2026-07-28-divergence-audit.md` (not part of the candidate source tree). These records supply parent-decision context only. The decisions below remain understandable from the official protocol revision and candidate citations if the coordination root is unavailable.
- **Candidate-source basis**: `cmd/mcp-mux/main.go:main`; `muxcore/engine/engine.go:457-564`; `muxcore/owner/resilient_client.go:175-247`; `muxcore/control/protocol.go:13-45`; `muxcore/daemon/daemon.go:Spawn/spawnOnce/findSharedOwnerLocked`; `muxcore/serverid/serverid.go:GenerateContextKey`; `muxcore/owner/owner.go:Status`; and `muxcore/daemon/daemon.go:HandleStatus/HandleListOwners`.

`OBSERVED` means a primary protocol or current-candidate fact. `INFERRED` means the R1 product/design choice derived from those facts. The retained preservation prototype is not a source authority and is not used as an implementation base.

## Decision Register

### R-01 — Native, pinned modern route

**Decision**: **INFERRED.** R1 supports one explicit, pinned native MCP `2026-07-28` route for a known modern host and same-era upstream. The selected path forwards native frames; it does not translate legacy initialization, requests, results, errors, callbacks, or transport semantics.

**Rationale**: **OBSERVED.** Modern requests carry protocol facts per request and have no initialization negotiation handshake (`ERA-001`, `ERA-005`, `OBS-006`). The candidate remains legacy-shaped: CLI and embedding ingress spawn before reading host stdin, and owner materialization sends legacy proactive initialization (`cmd/mcp-mux/main.go:main`; `muxcore/engine/engine.go:(*MuxEngine).runClient`; `muxcore/owner/owner.go:sendProactiveInit`). A native pre-election route is therefore the smallest way to preserve the opening frame and prevent accidental mixed-era attachment.

**Alternatives considered**:

- **Translate legacy and modern messages** — rejected. It creates a gateway product with separate method/result/error semantics and exceeds R1.
- **Let the existing legacy path infer modern after attachment** — rejected. Owner election, bootstrap, cache, and reconnect behavior have already selected legacy semantics.
- **Support only legacy until a complete shared modern design exists** — rejected. It withholds the explicitly authorized R1 customer outcome even though isolation provides a bounded safe path.

### R-02 — Public policy is explicit; no automatic fallback

**Decision**: **INFERRED.** Expose an additive, explicit modern policy in both the CLI and public library. Its zero value remains legacy. R1 does not infer a protocol era from historical sharing flags, an upstream response, `clientInfo`, a process name, or a failed modern attempt. Recommended plan vocabulary is `LegacyOnly` and pinned `Modern20260728`; exact exported/flag spelling is implementation-time API review, not a semantic open question.

**Rationale**: **OBSERVED.** A server must implement `server/discover`, but a client may send any modern RPC without calling it first (`ERA-004`, `DISC-001`, `DISC-003`). The standard has dual-era probing guidance, yet the candidate spec explicitly excludes automatic same-shim `server/discover`→`initialize` fallback (`spec.md` out-of-scope list). The official Inspector distinguishes a pinned modern mode from auto fallback, which supports the explicit selection direction without making Inspector behavior protocol authority.

**Alternatives considered**:

- **Auto-probe with `server/discover` then fall back to legacy** — deferred outside R1. A required host must first prove the same-shim need; then it requires a separately designed, permanently isolated pending-era path.
- **Repurpose `--stateless` or sharing mode as era selection** — rejected. Sharing is local owner policy and cannot be conflated with protocol semantics.
- **Default all callers to modern** — rejected. It breaks released callers and contradicts the zero-value legacy compatibility requirement.

### R-03 — Era selection happens before owner election and remains immutable

**Decision**: **INFERRED.** Buffer exactly one newline-delimited opening frame at CLI and embedded-engine ingress, select a typed protocol era before daemon spawn/attach, and carry that selected value through the control request, identity calculation, owner configuration, status, reconnect, and lifecycle decision. A modern R1 owner is immutable for its lifetime. Unknown, absent, malformed, ambiguous, and mismatched era values fail closed; none default to legacy.

**Rationale**: **OBSERVED.** Current CLI/engine flows compute identity, ensure a daemon, and send spawn before host stdin is read; `RunResilientClient` begins host-frame reading only after IPC/token admission (`cmd/mcp-mux/main.go`; `muxcore/engine/engine.go:457-564`; `muxcore/owner/resilient_client.go:175-247`). Current `control.Request`, `OwnerConfig`, snapshot/handoff records, and owner identity have no era field. Current owner election uses mode/command/args/CWD/environment/classification and can reuse an existing owner (`muxcore/control/protocol.go:13-45`; `muxcore/daemon/daemon.go:Spawn/spawnOnce/findSharedOwnerLocked`). The selector must precede those facts.

**Alternatives considered**:

- **Select era inside `Owner.handleDownstreamMessage`** — rejected. The owner may already be legacy, shared, cache-backed, or attached to an upstream.
- **Store an unvalidated string on the owner** — rejected. Unknown values could coerce to legacy and paths would drift.
- **Create a permanent second owner architecture** — rejected for R1. It duplicates mature IPC/materialization/finalization authority before repeated evidence proves the shared core is the wrong boundary.

### R-04 — Modern metadata is validated per request, not cached as identity

**Decision**: **INFERRED.** R1 validates the opening modern request before owner election and validates the required modern envelope on any later modern request that mcp-mux itself consumes or answers. Required `params._meta` fields are a non-empty string `io.modelcontextprotocol/protocolVersion` and an object `io.modelcontextprotocol/clientCapabilities`. `{}` is valid. `io.modelcontextprotocol/clientInfo` is optional; omit it without penalty, but validate string `name` and `version` if present. The mux does not make trust, sharing, authorization, or cache decisions from self-reported client/server information.

**Rationale**: **OBSERVED.** Required fields and `-32602` behavior are specified by `META-001`, `META-002`, and `META-007`; `clientInfo` is a SHOULD, not required (`META-003`). Per-request statelessness prohibits relying on earlier request capability/version/identity data (`ERA-005`). Self-reported identities should not influence security (`IDENT-002`).

**Alternatives considered**:

- **Require `clientInfo`** — rejected. It rejects conforming modern clients.
- **Check only metadata-key presence** — rejected. `null`, wrong type, or an unsupported string could select a modern owner unsafely.
- **Validate every notification as if it were a request** — rejected. The requirement applies to requests; generic notification metadata is optional and R1 must not invent a broader protocol requirement.

### R-05 — Error classification is precise and local failures remain local

**Decision**: **INFERRED.** R1 uses this error partition at the admission boundary:

| Condition | Required outcome |
| --- | --- |
| Invalid JSON / invalid JSON-RPC request | Standard parse or invalid-request error before owner election. |
| Required modern `_meta` absent, `null`, non-object, wrong typed, or malformed present `clientInfo` | JSON-RPC `-32602` (`Invalid params`) when a request ID permits an error response; no spawn or attachment. |
| Validly declared protocol version unsupported by R1 | JSON-RPC `-32022` with `data.supported` and `data.requested`; no fallback. |
| Contradictory legacy/modern opener on the explicit modern route | Safe local refusal before election; do not claim it is necessarily `-32022`. |
| Absent, unknown, or mismatched control era echo | Local fail-closed admission error; do not overload MCP `-32022` unless the declared protocol-version predicate is actually true. |
| Later capability a same-era upstream requires but client did not declare | Pass through the same-era server’s `-32021`/`requiredCapabilities` result; do not invent capability support. |

**Rationale**: **OBSERVED.** Missing required fields require `-32602` (`META-002`). An unsupported declared version requires an `UnsupportedProtocolVersionError` with support data (`ERA-002`). The protocol does not prescribe one universal code for a local daemon echo mismatch or deliberately contradictory opener. Conflating local safety policy with a wire-level unsupported version would mislead clients and hide an operator/control failure.

**Alternatives considered**:

- **Return `-32022` for every refusal** — rejected. It loses the malformed-versus-unsupported distinction and falsely treats local control failures as protocol negotiation.
- **Treat malformed modern input as legacy** — rejected. It creates a modern-to-legacy safety breach.
- **Accept mismatch then let upstream reject it** — rejected. The unsafe owner may already have started or attached.

### R-06 — `server/discover`, directionality, MRTR, and opaque state remain native

**Decision**: **INFERRED.** `server/discover` is a valid opening only when the host sends it; R1 forwards it unchanged and never injects it. Any other valid pinned modern request may open. A modern upstream JSON-RPC request is contained and never forwarded through `handleUpstreamRequest`, last-active routing, or broadcast. Modern `input_required`, `inputRequests`, `inputResponses`, result types, and opaque `requestState` pass natively; a host retry is an ordinary new request with a new JSON-RPC ID, not mux-authored retry lineage. R1 reuses current single-recipient routing, request-ID remap, process-bound inflight/generation fences, and existing ephemeral session, progress, and subscription state. `ExistingEphemeralRouteState` names that reuse only. It is not a new map, API, or correlation authority. R2 owns collision-safe shared causal correlation.

**Rationale**: **OBSERVED.** Servers implement discovery but clients may call another RPC first (`ERA-004`, `DISC-001`, `DISC-003`). A stdio server must not emit JSON-RPC requests; server input uses MRTR (`OBS-004`, `MRTR-004`). `requestState` must be echoed exactly by the client and a retry gets a different ID (`MRTR-008`, `MRTR-009`). Candidate upstream request handling routes legacy server requests to the last-active session (`muxcore/owner/owner.go:handleUpstreamRequest/routeToLastActiveSession`), so it is explicitly unsafe for modern traffic.

**Alternatives considered**:

- **Require discovery as the only opener** — rejected. The protocol permits direct modern RPC and R1 must not manufacture a handshake.
- **Turn MRTR into legacy callbacks** — rejected. It is semantic translation and breaks opaque-state boundaries.
- **Forward modern upstream requests to the only isolated downstream** — rejected. One recipient does not make an illegal server request legal; it must be contained.
- **Add R2 route authorities in R1** — deferred. Shared collision-safe correlation needs its own authorized R2 contract.
### R-07 — Modern logging is request-scoped and sole-recipient only

**Decision**: **INFERRED.** R1 never synthesizes `notifications/message`, never converts stderr into an MCP logging notification, never broadcasts a log, and never uses logging to select an owner. If a valid standard modern log arrives on the sole isolated route for a request that opted in with `io.modelcontextprotocol/logLevel`, it may reach that one recipient at most once. This is runtime behavior tested by R1. R1 does not make logging a required status field.

**Rationale**: **OBSERVED.** Structured logging is deprecated; when retained it is enabled by a particular request’s `_meta` and must be delivered on that request’s response stream, not a subscription stream (`OBS-005`; primary source revision listed above). Stderr is a separate transport concern. R1’s forced isolation makes one recipient safe without inventing shared attribution.

**Alternatives considered**:

- **Broadcast generic logs** — rejected. No causal attribution is available and it leaks across sessions.
- **Drop all logs** — rejected. It needlessly destroys valid isolated native behavior.
- **Reintroduce legacy `logging/setLevel`** — rejected. It is generated legacy traffic and not the modern request-scoped rule.
### R-08 — Cache, templates, synthetic bootstrap, and replay stay off for modern

**Decision**: **INFERRED.** A modern R1 owner emits no mux-generated `initialize`, `notifications/initialized`, roots/list or list-change, cache response, template response, or reconnect replay. It does not hydrate or publish a modern template/cache. Existing legacy bootstrap, materialization, template, cache, and reconnect code remains unchanged behind the legacy branch.

**Rationale**: **OBSERVED.** Modern removes the initialization handshake (`OBS-006`). Correct cache use requires result-affecting parameters, TTL, scope, and authorization context; MRTR retry responses are not cacheable (`CACHE-001` through `CACHE-008`). Candidate materialization proactively initializes and caches (`owner.sendProactiveInit`; `owner/materialization.go`), while resilient reconnect replays initialize and emits list-change notifications (`owner/resilient_client.go:replayInit/sendListChangedNotifications`). These are legitimate legacy behaviors but are not a safe modern default.

**Alternatives considered**:

- **Enable a modern cache with legacy cache keys** — rejected. It cannot prove result/authorization/TTL/scope correctness.
- **Retain template materialization for a faster first response** — rejected. A cache-backed owner can serve frames before native modern opening traffic reaches an upstream.
- **Delete legacy cache paths** — rejected. It breaks released legacy behavior and exceeds R1.

### R-09 — Era and sharing are independent; R1 forces isolation

**Decision**: **INFERRED.** Protocol era is a typed owner fact independent of existing `cwd`/`git`/`global`/`isolated` sharing mode. R1 modern always becomes one forced-isolated owner; its physical identity is distinct from legacy and safe as an IPC path component. Legacy `GenerateContextKey` output remains byte-identical. No raw era suffix, client information, post-connect authorization, command similarity, or unknown mode may make modern traffic shareable.

**Rationale**: **OBSERVED.** Protocol statelessness says a server cannot rely on previous wire messages; it does not remove local ownership or authorize cross-client reuse (`ERA-005`). Candidate owner identity is directly used to form IPC paths (`muxcore/serverid/serverid.go:GenerateContextKey/IPCPath`). Current owner reuse is era-blind and existing `Mode` has legacy fallback behavior. The root audit documented that a raw `|era=modern` suffix would be Windows-invalid; the candidate must instead use an encoded/opaque, filesystem-safe physical identity.

**Alternatives considered**:

- **Treat modern as `ModeIsolated` only** — rejected as a model. It hides protocol era inside sharing and makes lifecycle/control/status ambiguous.
- **Enable modern sharing because the protocol is stateless** — rejected. R2 needs independently proven compatibility, authorization partition, causal routing, and logging policy.
- **Expose an era/compatibility digest in status** — rejected. It creates a linkable public fingerprint.

### R-10 — R1 lifecycle quarantine uses existing process-generation fences

**Decision**: **INFERRED.** R1 keeps current daemon/owner process authority, exact generation inflight binding, stale-event fences, bounded retries, and `RetirementProven` finalization. It applies this quarantine:

| Boundary | R1 disposition |
| --- | --- |
| Snapshot export or restore | Exclude era-less modern records. A later explicit modern launch cold-starts or fails closed; no cache/token/live-route hydration. |
| Live handoff | Refuse/omit a modern owner before detach through the current era-less payload; cold-start or fail closed. |
| Reaper or zero-session removal | Use existing activity/CAS/finalization gates to drain and remove; never reconstruct implicit legacy state. |
| Retry rehydration or in-memory respawn | Continue only with an exact immutable modern era and modern policy; otherwise cold-start or fail closed. |
| Daemon or upstream loss | Terminate the current generation and its routes once; no request, MRTR, progress, subscription, or legacy recovery replay. |
| Downstream reconnect | Exact-era fresh admission with an empty route set, or explicit new-launch failure. No old subscription route survives; the host sends a new `subscriptions/listen`. |

**Rationale**: **OBSERVED.** The candidate has mature current-process/generation and finalization protections (`owner.go:handleUpstreamMessageFrom`, `writeUpstreamFromCurrent`, inflight claim paths; `owner/materialization.go`; `daemon/owner_lifecycle.go`). Yet control, snapshot, handoff, and owner configuration lack an era field; current snapshot/handoff records can hydrate cache/token/classification state and current reconnect replays initialize. MCP requires a new listen after stdio reconnect (`LIFE-011`) and does not authorize replay. R1 therefore uses conservative exclusion rather than designing R3 transfer semantics.

**Alternatives considered**:

- **Persist modern lifecycle state in R1** — rejected. Versioned same-era descriptors and compatibility rules are R3 scope.
- **Drop current process/generation gates as obsolete under statelessness** — rejected. They are local authority, not protocol conversation state.
- **Replay buffered work after a loss** — rejected. A loss ends current work; a host-issued request is fresh traffic.

### R-11 — Minimal R1 OwnerInfo readback proves admission and rollback

**Decision**: **INFERRED.** The modern spawn control response echoes the exact selected era. Each existing direct owner status, daemon status/list, CLI status, or `mux_list` projection that carries `OwnerInfo` exposes `protocol_era`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine`. An existing readiness field retains its current meaning wherever that surface already provides one. Logging remains tested runtime behavior, not a required readback field. R1 adds no registry descriptor schema or capability, `mux_engines` contract, topology contract, lifecycle-state taxonomy, or safe-counter model. All R1 readbacks retain existing redaction.

**Rationale**: **OBSERVED.** The candidate already projects owner data through `muxcore/owner/owner.go:Status`, `muxcore/daemon/daemon.go:HandleStatus/HandleListOwners`, `muxcore/control/protocol.go:13-45`, CLI status, and `internal/mcpserver/server.go` `mux_list`. These seams need minimal admission and rollback truth. A registry descriptor, topology contract, or new lifecycle accounting system would add R3 observability scope without improving the R1 safety boundary.

**Alternatives considered**:

- **Add a registry descriptor capability or schema** — deferred to R3. R1 needs no new descriptor contract to prove admission or rollback.
- **Define a universal lifecycle-state and counter taxonomy** — deferred to R3. Existing readiness data remains sufficient where present.
- **Expose the fields only in CLI output** — rejected. The existing direct projections that carry `OwnerInfo` must not disagree.
### R-12 — Testing separates legacy parity, focused R1 safety, and built customer proof

**Decision**: **INFERRED.** The later implementation must add focused RED/GREEN cases at ingress, control/identity, native owner readiness/directionality/logging, every named lifecycle quarantine boundary, minimal OwnerInfo readback/redaction, and customer workflow. Existing legacy tests remain independent parity tests and are never repurposed as modern proof. The final implementation must have one independent checker run the customer matrix in `quickstart.md` on the exact built candidate.

**Rationale**: **OBSERVED.** Existing tests cover legacy bootstrap/cache/replay, process generations, snapshot/handoff, reaper, zero-session removal, reconnect, status, and registry, but no candidate fixture proves modern pre-spawn admission or quarantine. The adjacent suites are `muxcore/engine/engine_test.go`, `muxcore/control/control_test.go`, `muxcore/serverid/serverid_test.go`, daemon snapshot/handoff/reaper/lifecycle suites, owner materialization/resilient suites, direct status/list tests, CLI status tests, and `internal/mcpserver` tests. R1 proves the exact control echo plus the selected OwnerInfo projections. Descriptor schemas, comprehensive counters, `mux_engines`, and topology remain R3 work.

**Alternatives considered**:

- **Rely on unit tests for metadata helpers** — rejected. They cannot prove pre-spawn election, byte-preserved opener, or customer-visible lifecycle outcomes.
- **Replace legacy fixtures with modern ones** — rejected. It would remove the exact regression baseline R1 promises to preserve.
- **Run a broad suite during this Plan stage** — rejected by scope. No source changed; the Plan stage records future proof rather than creating phantom validation.
## Decision Closure

| Former planning uncertainty | Closed decision |
| --- | --- |
| Is discovery the required opener? | No. It is forwarded unchanged when sent; any valid pinned modern request may open. |
| Is `clientInfo` mandatory? | No. Omitted is valid; present is validated. |
| What is `-32602` versus `-32022`? | Malformed required metadata is `-32602`; a valid unsupported declared version is `-32022` with support/request data; local control mismatch remains local. |
| Does modern statelessness permit sharing or remove lifecycle state? | No. R1 forces isolation and retains local control/generation authority. |
| What happens to modern server requests, MRTR, logging, and opaque state? | Server requests are contained; MRTR and opaque state pass natively; logs are request-scoped and sole-recipient only. |
| What happens after loss/reconnect? | Current work ends; no replay or automatic re-listen; host issues fresh request/listen after exact-era admission. |
| Does R1 implement persistence or handoff? | No. Era-less snapshot/handoff is quarantined through exclusion, cold start, or failure. |
| Does any R1 question remain unresolved? | No. R2 sharing and R3 persistence are explicit exclusions, not unresolved R1 decisions. |
