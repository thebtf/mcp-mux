# Contract: R1 Lifecycle Quarantine and Error Behavior

## Purpose

R1 does not design persisted modern lifecycle transfer. It quarantines every existing legacy-shaped re-entry path so a modern owner either retains its explicit immutable era in a safe in-memory path, drains/removes, cold-starts with no transferred live state, or fails closed. It never restores, attaches, or retries modern work as legacy. The current snapshot/handoff payload is era-less, so R1 excludes it rather than adding an era field or schema version; versioned same-era persistence remains R3 scope.

This contract preserves existing daemon/owner authority:

- current-process generation fences and exact process-bound inflight work;
- stale-event rejection;
- bounded spawn/materialization retry budgets;
- reaper and zero-session CAS/activity gates; and
- whole-tree `RetirementProven` finalization before authority/registry deletion.

R1 adds no independent reaper, spawn lock, replay loop, or cleanup authority.

## Universal modern invariants

1. `ProtocolEra == Modern20260728` is explicit and immutable for the owner lifetime.
2. A current generation retains existing single-recipient routing, request-ID remap, process-bound inflight work, and generation fences. A stale generation has no delivery, cancellation, cache, or replacement authority.
3. A terminal loss clears existing ephemeral session, progress, and subscription state once. Existing stale-event handling drops duplicate or late signals. If the daemon survives an upstream-generation loss, it may continue only through fresh exact-era admission with an empty route set. R1 adds no route map, correlation authority, or counter requirement.
4. A host-issued retry after `input_required` is a fresh ordinary request. mcp-mux does not interpret, persist, or replay opaque `requestState`.
5. A terminated route has no subscription continuity. After reconnect, the host sends a new `subscriptions/listen` request.
6. No lifecycle boundary injects `initialize`, `initialized`, roots/list, list-change, cache, template, or replay traffic into a modern upstream.

## Retry rehydration

`Retry rehydration` is bounded restoration of retry-family identity, counter, and eligibility after lifecycle restore or re-entry. It uses the existing bounded retry authority. It may proceed only when the exact explicit modern era and modern policy remain present. An absent, unknown, malformed, or mismatched era cold-starts or fails closed. It never supplies `Legacy`, `ModeGlobal`, or another implicit legacy default, and it does not persist a modern retry record.

## Quarantine matrix

| Boundary | Existing local safety rails retained | R1 required disposition | Prohibited result |
| --- | --- | --- | --- |
| Snapshot export | Snapshot pins, version/age/corruption checks, owner snapshot lock | Omit/exclude modern owner from the current era-less snapshot; retain no cache/token/live-route payload and add no R1 field/schema version. | Serializing a modern record that a legacy reader can hydrate. |
| Snapshot restore / staged restore | Restore health checks, startup staging, materialization barrier | Treat era-less modern record as ineligible; cold-start only after a fresh explicit modern admission or fail closed. | Interpreting absent era as legacy or rehydrating modern cache/token/routes. |
| Live handoff | Version/token hello, process authority transfer, commit/abort and fallback gates | Exclude/refuse modern owner before detach through current era-less handoff payload; cold-start/fail closed without R1 schema change. | Attaching/detaching a modern owner via a legacy payload. |
| Reaper | Pending session/request/progress/busy/persistent/activity gates | When eligible, drain and remove through normal finalization; do not launch a legacy successor. | Reaper recreating modern owner from a bare retry key. |
| Zero-session cleanup | Exact entry/zeroAt CAS and reconnect reservation gates | Keep existing race guards, then drain/remove if no exact-era safe continuation exists. | Old timer removing/replacing with implicit legacy behavior. |
| Retry rehydration | Bounded Spawn/retry counters and circuit breaker | Restore retry-family identity, counter, and eligibility only with exact explicit modern era and policy; otherwise cold-start/fail closed. | Defaulting an omitted era to `ModeGlobal`/legacy. |
| Owner-local respawn | Materialization attempt/current-process/cache-stage fences | Replacement is only same explicit modern era with modern policy; current work is already terminal. | Legacy bootstrap/cache/replay on a replacement or stale-generation delivery. |
| Daemon or upstream loss | Inflight `LoadAndDelete`, process binding, stale-event rejection, retirement proof | End current work once, clear existing ephemeral state, then exact-era fresh admission, safe removal, cold start, or failure. A surviving daemon cannot use an old route. | Replay request/MRTR/progress/subscription or target replacement with old cancellation. |
| Downstream reconnect | Token history/liveness and bounded reconnect failure direction | Exact-era fresh route with no preserved request, progress, or subscription state, or explicit new-launch failure. | `replayInit`, synthesized list change, legacy attach, or reuse of old ephemeral state. |
| Finalization blocked | `RetirementProven`, owner/registry retention, bounded retry | Retain the exact owner authority until proof or explicit failure under the existing lifecycle policy. | Forgetting owner then spawning a competing/legacy replacement. |

## Boundary error behavior

| Error / event | Host or operator result | Required local state |
| --- | --- | --- |
| Modern snapshot/handoff lacks explicit safe era transfer | Cold start or explicit refused operation | No modern cache/token/live route restored; no legacy conversion or R1 transfer schema extension. |
| Control-era mismatch on fresh/reconnect admission | Explicit local admission failure | No IPC attachment and no implicit legacy retry. |
| Upstream loss with active request | One existing-path terminal error/discontinuity by original request where possible | Existing inflight state clears once; stale response/cancel/progress/subscription drops. A surviving daemon admits only fresh exact-era traffic. |
| Upstream JSON-RPC request | Contained; never reaches downstream | Existing redacted diagnostic only; no last-active callback or broadcast. |
| Modern logging without valid request context | Not synthesized or broadcast | No recipient inferred from recency. |
| Reconnect after loss | Fresh host traffic only | No cached initialize/list change, no old listen state. |
| Finalization proof unavailable | Owner remains authoritative/blocked for existing retry policy | No replacement or state deletion that could create a process race. |

Local lifecycle errors are not protocol version errors unless they specifically result from a valid unsupported declared modern version. Error/status text must not disclose payloads, opaque state, tokens, credentials, or compatibility material.

## Transition model

```text
ModernReady(generation N)
  -> UpstreamLost / DaemonLost / SessionDetached / LifecycleBoundary
  -> ClaimExistingInflightOnce
  -> ClearExistingEphemeralRouteState
  -> FreshExactEraAdmission
     | ExactEraInMemoryContinuation
     | DrainAndRemove
     | ColdStartAfterFreshAdmission
     | FailClosed

No transition leads to LegacyReady.
No transition reuses ephemeral state from generation N.
```

`InputRequired` does not bypass this model: it is native result data. A host retry with a new ID is ordinary fresh traffic while the current explicit modern owner remains live.

## Legacy compatibility

The matrix applies only to an R1 modern owner. Existing snapshot, handoff, reaper, zero-session, materialization, cache/template, replay, and reconnect behavior remains available for legacy owners. The implementation must prove that adding the modern quarantine does not change legacy bootstrap, a legacy identity byte-identical to the released baseline, request/result sequence, or released recovery semantics.

## Verification obligation

Before R1 release, a modern fixture must exercise every row in the quarantine matrix alongside existing legacy parity fixtures. Each modern assertion proves an allowed outcome and the absence of the prohibited outcome. The fixture must include a surviving-daemon/upstream-loss cleared-state branch, stale-generation response/cancellation case, and attempted blocked-finalization case so retained process authority is tested rather than assumed.
