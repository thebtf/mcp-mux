# Maintenance design decisions

Authority: accepted root ADR-015, primary checkout `.agent/arch/decisions/015-upstream-maintenance-hold.md`, source base `3881f27b931f6b9d0467c0a15dd4e1824969e125`. Root's acceptance message incorporates FULL-challenge F1-F5. This is source analysis and design, not a current-version runtime reproduction of the original incident.

## F1: Match exact finite admitted contexts

Decision: use versioned length-delimited full digests of namespace/canonical control endpoint, era, command, exact argv boundaries, canonical CWD, and strict normalized effective `envidentity.Build` fingerprint. Persist the selected owner's finite already-admitted context set; refuse incomplete/ambiguous scope. Exclude product version, nonce, retry suffix, generation/lease ID, and transient mode promotion.

Rationale: `daemon.go:1225-1338` derives modern nonce identities and isolated retry suffixes. `serverid.go:197-247` confirms nonce-bound modern IDs. `internal/envidentity/identity.go:19-96` supplies full strict identity separately from relaxed compatibility. `daemon.go:3197-3235` uses compatibility/buckets for sharing; that relation cannot safely define a persisted equivalence class. A shared cross-CWD owner can have a finite set, not a host-wide command wildcard.

Alternatives rejected: visible server ID alone permits retry/nonce bypass; command-only matching crosses CWD/era/security boundaries; truncated environment hash weakens the identity; relaxed compatibility admits transitive ambiguity. No new credential matching policy is introduced.

## F2: Gate starts through authority installation

Decision: one daemon-owned shared start/admission gate acquired before owner locks, held across context election, physical start, and installation of all returned nonnil Process authority. Each retry reacquires it. Exclusive hold commitment waits for prior starts, pins exact entries/generations/contexts, commits HOLDING, and closes new request admission. Short registry checks release before blocking on placeholders/classification, then recheck.

Rationale: `owner/materialization.go:414-505,658-725` can retain a nonnil process on failed start and install it only afterward. A check before start or a gate released when start returns leaves an executable-locking child outside retirement. `daemon.go:1585-1604` publishes placeholders; identity must be complete first. Existing materialization and registry owners remain authoritative.

Alternatives rejected: boolean pre-start check races; acquiring a new gate under owner locks deadlocks commitment; another supervisor duplicates process authority. Lock order is gate before short `d.mu` or existing owner chain; no owner finalization under `d.mu` and no exclusive gate across drain waits.

## F3: Drain real requests and prove tree death

Decision: reject new ingress before cache/queue/forwarding; allow already-forwarded work until one deadline starting at fence commitment. Carry remaining time explicitly and finalize exact current generations afterward. HELD requires dead trees, accounted placeholders/failed starts, no detach, durable state, and unexpired TTL.

Rationale: `daemon.go:1881-1903` reports caller drain milliseconds while `owner_lifecycle.go:32-34` uses a fixed 30s finalization timeout. Existing soft teardown is not request drain. `materialization.go:683-725,1855-1882` proves retirement but accepted ADR notes committed handoff can satisfy RetirementProven while a process remains alive. Maintenance must require actual death, not transfer.

Alternatives rejected: stop acknowledgement, SoftClose-only grace, fixed timeout, deadline reset per retry, or HELD after an already expired lease. Blocked retirement does not auto-resume at TTL.

## F4: Durable startup authority; refuse controlled lifecycle

Decision: store an opaque versioned ledger under the current user's persistent config root, scoped by canonical endpoint/namespace digest. Restrictive same-directory temp write/flush and atomic replacement commit every fence/state/renew/release change. Load before listeners or any restore/start, independently of SkipSnapshot. Unknown/unreadable authority fails closed. Aware unplanned recovery reloads HELD; incomplete state becomes blocked without inventing process authority.

Rationale: snapshots and advisory registry records are not durable lease authority. Product `cmd/mcp-mux/replacefile_windows.go` uses write-through MoveFileEx; `replacefile_unix.go` uses rename. Adapt this pattern within muxcore rather than importing package main. The accepted first release refuses controlled restart/handoff/shutdown/downgrade and empty-daemon idle exit while fenced.

Alternatives rejected: temporary snapshot ledger, in-place truncate, deleting old authority to replace it, active-lease transfer negotiation, or inferring awareness from handoff v2. Lost-tree recovery and manual pointer swaps are not promised. Release write failure retains the fence.

## F5: Neutral errors and ingress disposition

Decision: freeze additive optional control types/methods in `contracts/maintenance.md`; use errors.Is/errors.As through both engine and CLI spawn/refresh. Immediately reject held buffered/new requests and serialize the ingress/maintenance-to-connected transition. Existing host pipes stay open; no queue overflow may silently lose a promised error ID. Lifecycle refusal is terminal through update helpers and launcher fallbacks. Standalone product paths require managed admission or explicitly refuse.

Rationale: `control/protocol.go:87-151` has optional extension precedent; `resilient_client.go:842-1007,1010-1078,1204-1250` currently retries/degrades and uses substring categories. `engine/update.go:269-286` and `cmd/mcp-mux/launcher.go:534-550` currently fall back to shutdown after refusal; those branches would undermine maintenance. MCP restart reconstructing from public owner info plus ambient environment is not exact managed context.

Alternatives rejected: substring detection, adapter-local ledger/DTOs, generic grace delay, queued replay, stop/kill/exec fallback, one-time standalone ledger check, and old-shim immediate-error promises. Native modern same-era isolation/cache-off/lifecycle quarantine stays unchanged per `specs/001-mcp-2026-07-28-r1/spec.md` and its primary protocol references. The local product -32005 maintenance error does not change MCP version negotiation.

## Scope and remaining decisions

No unresolved architecture disagreement remains after root acceptance. Core may choose coherent existing-file placement, but cannot change scope, API contract, key inputs, gate coverage, or refusal boundary without root review. The release version is selected by the root release flow, not invented in this feature. Cross-platform executable replacement remains an implementation-time proof obligation, not a claim from this planning assignment.
