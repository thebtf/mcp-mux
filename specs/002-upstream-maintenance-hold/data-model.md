# Maintenance data model

## Context identity and selected scope

`MaintenanceContextKey` is a private versioned full digest of canonical namespace/control endpoint, exact protocol era, command, length-delimited argv, canonical CWD, and normalized strict effective security/configuration environment fingerprint from `envidentity.Build`. It excludes product version, nonce, retry suffix, lease/generation IDs, and transient sharing promotion. Compute at admission and keep immutable on placeholders before publication.

An owner has a finite already-admitted context set, including exact reconnect contexts. Shared cross-CWD admission records each context through the same gate. A hold resolves one exact current owner into that complete set, matches extant generations, and pins OwnerEntry pointer plus OwnerGeneration. Reject incomplete or ambiguous scope before committing. Do not infer a command wildcard or disclose context keys.

## Lease and persistent ledger

One lease contains:

- random opaque hold ID;
- private finite context-key set and schema/key versions;
- state `HOLDING`, `HELD`, or `RETIREMENT_BLOCKED`;
- provisional timing for the incomplete durable seed, then UTC expiry and the one drain deadline derived from `T` after seed acknowledgment;
- safe timing metadata needed for deterministic readback/recovery.

The schema-2 logical ledger comprises mandatory `ledger.json` and `transaction.json`. The ledger member contains schema version, endpoint/namespace scope, transaction ID, and current leases. The transaction member contains the matching identity and target digest, plus predecessor leases only while PREPARED. Neither contains raw command/env/credentials, MCP frames, arbitrary reasons, public generation maps, or modern snapshot state. Public target server ID and pinned generations remain in daemon memory; after recovery, unavailable display metadata must not be reconstructed from private keys.

Versioned ledger directory: current-user persistent configuration root, product subdirectory, and full canonical endpoint/namespace digest. Canonicalization resolves the endpoint parent, not the socket leaf. Aggregate lookup validates both members, canonical lowercase keys, and unambiguous JSON; missing, pending, mismatched, or invalid pairs fail closed. Each member uses restrictive same-directory temp write, flush, platform replacement, and required Unix directory durability, not atomic two-file replacement. PREPARE retains predecessor leases and the target digest; PUBLISH follows acknowledged durable preparation; FINALIZE writes COMMITTED only after acknowledged durable target publication. Errors never acknowledge the new state or open live admission. A finalize error can nevertheless leave a valid certificate that recovery verifies as proof of earlier durable publication. See [phase outcomes](contracts/maintenance.md#authority-and-lifecycle-boundaries). Do not erase authority or truncate in place. A committed HOLDING fence persists even if the caller disconnects.

Acquisition first persists a complete durable `HOLDING` seed with provisional timing and incomplete retirement proof, never a usable grant. Sample `T` once after the first complete writer acknowledgment, then persist clocked `HOLDING` once with expiry `T + TTL` and drain deadline `T + drain`. The second write, retirement, HELD publication, and response consume the original window without resetting `T`. Any acquisition write failure retains the conservative seed fence; incomplete recovery is `RETIREMENT_BLOCKED` with no expiry/resume. Existing schema/states suffice; renewal still uses serialized exact-lease acceptance.

## In-memory process authority

Exact pinned OwnerEntry/OwnerGeneration and every matched process authority remain in existing daemon/owner/upstream objects, not the ledger. A start transaction acquires the shared gate before owner locks and holds it until every nonnil returned process is installed, including failed starts. Exclusive acquisition waits for those transactions and captures all scoped processes/placeholders before fencing. No handoff detach can occur during maintenance.

`TreesRetired=true` in public MaintenanceResult means all selected trees are dead and all starts/failed-start authority/placeholders are accounted for. It is stronger than handoff-capable `RetirementProven`. Persisted HELD was published only after this proof; an incomplete record cannot recover that proof merely from expired time or absence of owner records.

## State transitions

| Current state | Event | Result |
| --- | --- | --- |
| No fence | Valid exact-target hold, scope pinned, durable seed acknowledged | HOLDING; admission fenced, provisional timing is not a grant. |
| HOLDING seed | Sample `T` once after first complete writer acknowledgment, persist clocked HOLDING once | HOLDING; expiry/drain derive from `T`, later writes and retirement consume that window. |
| HOLDING | In-flight work finishes or single drain deadline arrives; full tree death and durable usable lease confirmed | HELD. |
| HOLDING | Retirement proof fails, incomplete authority/placeholder, or unplanned loss | RETIREMENT_BLOCKED; keep fence. |
| HELD | Exact-ID valid renewal | HELD with durable expiry = renewal acceptance + TTL. |
| Clocked HOLDING or RETIREMENT_BLOCKED | Exact-ID valid renewal before expiry | Same state, new durable expiry; no tree-proof substitution. |
| HELD | Exact-ID resume, or accepted expiry | Durably remove/release lease, then open admission; return RELEASED for resume. |
| Incomplete seed/recovery, or HOLDING/RETIREMENT_BLOCKED without tree death proof | Resume or expiry | Refuse and stay fenced; provisional elapsed timing never opens admission. |
| RETIREMENT_BLOCKED | Original live authority later proves all trees dead | Durably HELD only with usable expiry, otherwise safely release after proof without issuing an expired installer grant. |
| Any active fence | Controlled restart/handoff/shutdown/downgrade/idle exit | Terminal refusal, no state change/fallback. |
| Persisted HELD | Unplanned aware startup | Load fence before admission; release only if safely expired and durable release succeeds. |
| Persisted incomplete seed/record | Unplanned aware startup | RETIREMENT_BLOCKED; no lost-tree authority reconstruction, expiry, or resume. |
| Unknown/corrupt/unreadable ledger | Startup or mutation | Fail closed in namespace; no claimed successful hold/start. |

Resume/renew/expiry serialize against the exact current lease. Stale IDs never clear a replacement lease. Released leases need no history/tombstone subsystem; missing IDs return not-found unless a current conflicting lease is known. An elapsed acquisition window never returns HELD as a usable installer grant.

## Request disposition

New aware requests after fence commitment return one maintenance error with original raw ID before cache, queue, or forwarding. Notifications and response frames are dropped without fabricated replies. Already-forwarded requests may complete only before the drain deadline; terminated work receives one terminal outcome and never replays. Shim ingress and connection transitions linearize held acceptance so a reconnect win cannot forward a held frame. Queue capacity uses backpressure or direct error disposition, not silent request loss.

Fresh released legacy demand may reconnect on original host pipes and create one new generation. Native modern demand requires fresh same-era admission or explicit new-launch-required refusal, never legacy bootstrap, cache, replay, or subscription restoration.

## Public readback

Use the exact `MaintenanceResult` and neutral error contract in [maintenance.md](contracts/maintenance.md). OwnerInfo carries the optional current lease projection; daemon status includes active leases even after owner removal. UTC expiry, safe drain deadline, state, and tree-retired result are public. Private context sets, environment fingerprints, raw secrets, request bodies, and arbitrary diagnostic reasons are not.
