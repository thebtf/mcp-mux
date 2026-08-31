# R1 Consumer-Handoff Preparation

**Status**: Candidate-owned preparation only. This document does not publish a release, resolve a module version, create or update an Engram issue, contact a consumer, or record completed adoption.

## Scope

R1 adds a native MCP `2026-07-28` route to `mcp-mux`. The current CLI selector is `--mcp-protocol=2026-07-28`. An invocation without that selector keeps the released legacy path.

For an embedded `muxcore` consumer, set `engine.Config.ProtocolPolicy` to `era.PolicyModern20260728` before `engine.New` or `(*engine.MuxEngine).Run` begins admission. The zero value, `era.PolicyLegacyOnly`, keeps the released legacy ingress and daemon ordering.

The modern route is limited to a known MCP `2026-07-28` host and a same-era upstream. It is forced isolated. Response cache, template reuse, and replay are off. `mcp-mux` does not automatically fall back to legacy. A valid request-scoped standard log can reach the sole downstream after request opt-in. Upstream JSON-RPC requests remain contained.

The existing `mcp-mux` process-generation lifecycle and `RetirementProven` authority remain the only authority for owner retirement. A loss requires the host to make a fresh exact-era admission or to re-listen after a new route is established. R1 does not preserve live modern subscriptions across loss.

## Impacted consumers

| Consumer | Current impact | Candidate-owned preparation | Future consumer decision |
| --- | --- | --- | --- |
| `mcp-mux` | Provides the selector, the embedded `ProtocolPolicy`, forced isolation, lifecycle quarantine, and four readback facts. | Record the exact candidate source and binary identities, modern and legacy proof, and rollback evidence after the required release checks run. | Release the candidate through the release process. No consumer adoption is implied by this preparation. |
| `aimux` | Keeps its current behavior unless it updates to the released `muxcore` version and explicitly selects the modern policy for a known same-era route. | Identify the affected revision and retain the required adoption record fields below. | Keep the legacy default, or make an explicit route-by-route opt-in after the released module version is known. |
| `engram` | Keeps its current behavior unless it updates to the released `muxcore` version and explicitly selects the modern policy for a known same-era route. | Identify the affected revision and retain the required adoption record fields below. | Keep the legacy default, or make an explicit route-by-route opt-in after the released module version is known. |
| Other `muxcore` consumers | The release inventory determines the complete set. An unknown consumer does not silently enter the modern route because legacy remains the default. | Add every consumer named by release inventory, test evidence, an issue, a release note, or an operator directive. | Keep the legacy default, or complete the same explicit opt-in and evidence record. |

## Additive adoption rules

A consumer can adopt R1 only when all of these conditions hold:

1. The consumer identifies a known MCP `2026-07-28` host and same-era upstream.
2. The consumer selects `--mcp-protocol=2026-07-28`, or sets `engine.Config.ProtocolPolicy` to `era.PolicyModern20260728`, before admission starts.
3. Control echoes `protocol_era=2026-07-28` before the route attaches.
4. Owner readback reports `protocol_era=2026-07-28`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine`.
5. The consumer proves its released legacy path before and after its adoption change. Legacy identity remains byte-identical to the released baseline.

Sharing inputs do not select the modern route: `MCP_MUX_ISOLATED`, `--isolated`, `MCP_MUX_STATELESS`, `--stateless`, and `x-mux.sharing`. `clientInfo`, discovery responses, upstream names, and a failed modern attempt also do not select it.

## Prohibited consumer-local workarounds

Consumers must not add local owner lifecycle controllers, process cleanup, process-generation tracking, forced retirement, snapshot recovery, reconnect controllers, automatic re-listen, request replay, subscription replay, or a legacy fallback for modern traffic. These mechanisms would conflict with `mcp-mux` owner authority and `RetirementProven`.

After a loss, consumers must either make a fresh exact-era admission or report the new-launch failure. For `subscriptions/listen`, the consumer must issue a new native listen request after the new route is established. A consumer must not transfer live modern work to a legacy owner, reuse old opaque state, or simulate a successful continuation.

## Rollback boundary

Rollback stops new invocations that select `--mcp-protocol=2026-07-28` and drains or removes active modern owners through the existing R1 lifecycle-quarantine path. It never downgrades live modern work to legacy, hands it to a legacy owner, or replays unfinished work.

For a consumer dependency rollback, restore the previous compatible consumer or `muxcore` revision after the consumer has stopped modern admissions. Do not perform a mixed-version, mixed-era, or live-state transfer.

## Required release-stage handoff record

For every impacted consumer, the release-stage Engram issue or comment must contain:

- the released `muxcore` tag and module-resolution proof;
- the consumer identity and revision;
- the reason R1 is consumer-impacting, including the protocol-era, owner-lifecycle, and no-replay invariants;
- the explicit opt-in decision, or a statement that the consumer remains on legacy;
- required implementation steps and the prohibited consumer-local workarounds;
- consumer smoke-test and acceptance evidence, including legacy parity and modern proof when the consumer opts in;
- rollback and compatibility notes; and
- provider evidence: exact commit, test evidence, release tag, and candidate source/binary identity.

After the released `muxcore` version is known and resolvable, the release owner creates or updates the Engram issue for `aimux`, `engram`, and every other identified consumer. The release owner rereads each touched issue and records its ID and latest status in release evidence. If Engram is unavailable, release closeout records `CONSUMER_HANDOFF_BLOCKED` and does not call the full critical scope shipped.

`release-evidence.md` records candidate-level proof. `independent-check.md` records the independent boundary check. Neither document, nor this preparation, substitutes for release-stage publication or consumer adoption.
