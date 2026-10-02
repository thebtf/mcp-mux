# Maintenance data model

## Context identity and selected scope

`MaintenanceContextKey` is a private versioned full digest of canonical namespace/control endpoint, exact protocol era, command, length-delimited argv, canonical CWD, and normalized strict effective security/configuration environment fingerprint from `envidentity.Build`. It excludes product version, nonce, retry suffix, lease/generation IDs, and transient sharing promotion. Compute at admission and keep immutable on placeholders before publication.

An owner has a finite already-admitted context set, including exact reconnect contexts. Shared cross-CWD admission records each context through the same gate. A hold resolves one exact current owner into that complete set, matches extant generations, and pins OwnerEntry pointer plus OwnerGeneration. Reject incomplete or ambiguous scope before committing. Do not infer a command wildcard or disclose context keys.

## Lease and persistent ledger

One lease contains:

- random opaque hold ID;
- private finite context-key set and schema/key versions;
- state `HOLDING`, `HELD`, or `RETIREMENT_BLOCKED`;
- UTC accepted expiry and the one accepted drain deadline;
- safe timing metadata needed for deterministic readback/recovery.

The ledger contains a schema version, endpoint/namespace scope version, and current leases only. It contains no raw command/env/credentials, MCP frame, arbitrary reason, public generation map, or modern snapshot state. Public target server ID and pinned generations can be retained in daemon memory for readback; durable records must retain only the opaque scope and lease/timing facts allowed by ADR-015. After aware recovery, unavailable display target metadata must not be reconstructed from private keys.

Versioned ledger directory: current-user persistent configuration root, product subdirectory, and full canonical endpoint/namespace digest. Use restrictive directory/file permissions and existing platform replacement primitives. Commit by full same-directory temp write, flush, atomic replacement, and required Unix directory durability. Do not erase prior authority or truncate in place. Failed acquisition does not begin drain; failed HELD/renew/release persistence does not acknowledge the new state or open admission. A committed HOLDING fence persists even if the caller disconnects.

## In-memory process authority

Exact pinned OwnerEntry/OwnerGeneration and every matched process authority remain in existing daemon/owner/upstream objects, not the ledger. A start transaction acquires the shared gate before owner locks and holds it until every nonnil returned process is installed, including failed starts. Exclusive acquisition waits for those transactions and captures all scoped processes/placeholders before fencing. No handoff detach can occur during maintenance.

`TreesRetired=true` in public MaintenanceResult means all selected trees are dead and all starts/failed-start authority/placeholders are accounted for. It is stronger than handoff-capable `RetirementProven`. Persisted HELD was published only after this proof; an incomplete record cannot recover that proof merely from expired time or absence of owner records.

## State transitions

| Current state | Event | Result |
| --- | --- | --- |
| No fence | Valid exact-target hold, scope pinned, durable commitment | HOLDING; new requests/starts rejected, drain deadline begins once. |
| HOLDING | In-flight work finishes or single drain deadline arrives; full tree death and durable usable lease confirmed | HELD. |
| HOLDING | Retirement proof fails, incomplete authority/placeholder, or unplanned loss | RETIREMENT_BLOCKED; keep fence. |
| HELD | Exact-ID valid renewal | HELD with durable expiry = renewal acceptance + TTL. |
| HOLDING or RETIREMENT_BLOCKED | Exact-ID valid renewal before expiry | Same state, new durable expiry; no tree-proof substitution. |
| HELD | Exact-ID resume, or accepted expiry | Durably remove/release lease, then open admission; return RELEASED for resume. |
| HOLDING or RETIREMENT_BLOCKED | Resume or expiry without tree death proof | Refuse and stay fenced. |
| RETIREMENT_BLOCKED | Original live authority later proves all trees dead | Durably HELD only with usable expiry, otherwise safely release after proof without issuing an expired installer grant. |
| Any active fence | Controlled restart/handoff/shutdown/downgrade/idle exit | Terminal refusal, no state change/fallback. |
| Persisted HELD | Unplanned aware startup | Load fence before admission; release only if safely expired and durable release succeeds. |
| Persisted incomplete record | Unplanned aware startup | RETIREMENT_BLOCKED; no lost-tree authority reconstruction or automatic expiry. |
| Unknown/corrupt/unreadable ledger | Startup or mutation | Fail closed in namespace; no claimed successful hold/start. |

Resume/renew/expiry serialize against the exact current lease. Stale IDs never clear a replacement lease. Released leases need no history/tombstone subsystem; missing IDs return not-found unless a current conflicting lease is known. An elapsed acquisition window never returns HELD as a usable installer grant.

## Request disposition

New aware requests after fence commitment return one maintenance error with original raw ID before cache, queue, or forwarding. Notifications and response frames are dropped without fabricated replies. Already-forwarded requests may complete only before the drain deadline; terminated work receives one terminal outcome and never replays. Shim ingress and connection transitions linearize held acceptance so a reconnect win cannot forward a held frame. Queue capacity uses backpressure or direct error disposition, not silent request loss.

Fresh released legacy demand may reconnect on original host pipes and create one new generation. Native modern demand requires fresh same-era admission or explicit new-launch-required refusal, never legacy bootstrap, cache, replay, or subscription restoration.

## Public readback

Use the exact `MaintenanceResult` and neutral error contract in [maintenance.md](contracts/maintenance.md). OwnerInfo carries the optional current lease projection; daemon status includes active leases even after owner removal. UTC expiry, safe drain deadline, state, and tree-retired result are public. Private context sets, environment fingerprints, raw secrets, request bodies, and arbitrary diagnostic reasons are not.
