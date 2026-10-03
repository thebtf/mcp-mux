# mcp-mux v0.31.0

**Prepared:** 2026-10-03

**Type:** Additive, optional minor release for the binary and muxcore library

**Publication targets:** `v0.31.0` and `muxcore/v0.31.0`

These notes prepare the #135 release annotation. Publication, final
delivered-artifact verification, and the required Engram consumer handoff remain
pending. Accepted-source validation is not a claim that consumers have received
this release.

## Summary

v0.31.0 adds managed upstream maintenance so an installer can replace an
executable without new managed starts racing the replacement. Hold closes
admission, drains already-forwarded work, and proves full scoped process-tree
death before granting a usable lease. The installer owns the actual file
replacement. Resume or safe expiry permits fresh demand on the existing host
transport; mcp-mux never replays held or terminated requests.

Legacy remains the default. The explicit MCP `2026-07-28` R1 route retains
same-era admission, forced isolation, cache-off/replay-off behavior, and
snapshot/handoff quarantine.

## Optional control APIs

Use the exact local `server_id` from status and the returned opaque `hold_id`:

```text
mcp-mux hold <exact-server-id> --ttl 5m --drain-timeout 10s --json
mcp-mux renew <returned-hold-id> --ttl 5m --json
mcp-mux resume <returned-hold-id> --json
```

MCP tools `mux_hold`, `mux_resume`, and `mux_renew` delegate to the same local
daemon. `mux_hold` accepts integer `hold_seconds` in 1..3600, default 300,
and nonnegative `drain_timeout_ms`, default 10000. `mux_renew` uses the same
TTL bounds. There is no substring, PID, foreign-engine, or direct-owner fallback.
`mux_restart` uses daemon-owned `restart_owner` with the original exact context
and era, rather than rebuilding a launch from ambient adapter credentials.

Library consumers can adopt `control.SendMaintenance`, `Response.Err()`,
`MaintenanceResult`, and typed `MaintenanceError` handling with `errors.Is` and
`errors.As`. Optional `MaintenanceHandler`, `ShutdownWithErrorHandler`, and
`OwnerRestartHandler` preserve existing consumer interfaces. See the
[public library contract](muxcore/README.md#upstream-maintenance-control).
Ordinary legacy `engine.New` users require no source changes. Products adopt
the new control operations only when they need maintenance.

## Lease and replacement safety

- Replace a file only after a successful hold result reports durable `HELD`,
  `trees_retired=true`, and a future `expires_at`. `HOLDING` and
  `RETIREMENT_BLOCKED` remain fenced and do not authorize replacement.
- Scope is the selected owner's finite already-admitted context set, partitioned
  by daemon namespace, CWD, protocol era, and strict security/configuration
  identity. Other contexts and unmanaged processes can still lock the file.
  The lease is not a host-wide executable lock.
- TTL defaults to 5m and must be positive
  and at most 1h. Renew changes only the exact current unexpired lease and sets
  expiry from serialized renewal acceptance plus TTL. It does not change the
  retirement state or revive released/expired authority. Stale identities cannot
  clear a replacement lease.
- Resume requires full tree death and durable release. Safe HELD expiry also
  requires a successful durable release commit. Blocked retirement never clears
  merely because its TTL elapsed.
- Drain defaults to 10s and uses the same `T` as TTL without restarting. Zero skips grace,
  not tree-death proof. CLI TTL and drain durations require whole milliseconds;
  sub-millisecond values are rejected rather than truncated.
- Native retirement accounts for notifications, connect/disconnect lifecycle,
  authentication, and frame hooks through actual return, retaining sessions as
  teardown producers. Ordinary removal and maintenance require quiescence before
  completion, so a later hold cannot miss callbacks whose owner entry disappeared.
  Notification cancellation follows session/owner closure but does not prove
  settlement. Active work remains blocked without TTL/resume bypass; existing
  retry uses the original clock. `PendingRequests` stays request-only; no public
  metric, state, schema, topology exclusion, or modern notification dispatch added.

The durable `HOLDING` seed has provisional timing and never grants replacement.
Sample `T` once after its first complete writer acknowledgment, then persist clocked `HOLDING` once.
That write, retirement, HELD persistence, and response consume the original TTL/drain window; write failure retains the seed fence.
Incomplete recovery remains `RETIREMENT_BLOCKED`, without expiry/resume. No schema or state is added.

Neutral control's default for `hold`/`restart_owner` is 180s plus one drain; CLI/MCP request it with a zero caller timeout.
Explicit positive budgets remain unchanged; other commands retain 5s. This finite exchange allowance does not promise full-pin/storage completion.
Timeout leaves outcome unknown: a durable lease/restart may remain. Inspect status, without automatic retry/resume or stop fallback.

## Host transport and lifecycle

Aware managed shims keep the original host pipes open. Held requests receive
JSON-RPC `-32005`, message `upstream held for update`, and
`data.error_code=maintenance_held`, preserving the original numeric or string
ID. The fence wins over cached success. Unfinished work terminated by retirement
gets one terminal error. Held and terminated work never replays, and
notifications receive no invented response.
Pre-fence unfinished work may receive the existing original-ID `-32603`
reconnect error. `-32005` applies to requests received under the fence; this
correction does not change that distinction or add replay.

After durable release, fresh legacy demand reaches a new generation on the same
pipes. Modern demand uses fresh exact-era isolated admission with required
metadata, without legacy bootstrap, cache, progress, or subscription restoration.

Controlled restart, handoff, shutdown, downgrade, and idle daemon exit refuse
terminally while any fence remains. Launcher and library update helpers retain
typed refusal instead of falling back to shutdown or starting a successor.
Aware unplanned recovery loads durable authority before listener/restore/spawn.

Maintenance storage is one schema-2 authority with mandatory `ledger.json` and
`transaction.json`. Missing, pending, corrupt, or mismatched authority fails
closed. A persistence error is not a usable hold or release acknowledgment and
does not promise storage rollback. Recovery accepts only a matching COMMITTED
certificate proving previously acknowledged durable publication. A successful
durable release cannot resurrect the old lease.

Controlled installation, launcher swap, layout/bootstrap mutation, and
active-pointer changes serialize with hold-ledger mutation under the existing
daemon namespace file lock. `daemon.CheckMaintenanceForActivation` is read-only.
Status and pure startup inspection do not acquire/write that lock or proactively
start a daemon. Offline/old activation requires locked persisted-clear proof.

## Compatibility and rollback

Use maintenance-aware managed binaries, daemons, and shims together. An old
daemon or uncoordinated standalone path returns `maintenance_unsupported`; do
not substitute stop, kill, PID cleanup, or direct execution. An aware daemon
physically fences old managed shims' starts, but cannot promise those shims
immediate errors or non-replay behavior. Arbitrary old binaries, foreign engines,
unmanaged processes, and manual active-pointer replacement are not controlled.

Before restoring a compatible previous binary or pinning `muxcore/v0.30.0`, use
the current aware version to durably resume each exact retired lease or observe
its safely committed expiry. Incomplete or retirement-blocked authority prevents
downgrade and must be retained, even after TTL. Do not delete either authority
member or bypass admission. Stop new explicit-modern admissions and retire modern
owners through their existing quarantine path; never transfer live modern work
to legacy or replay unfinished work.

## Prior accepted-source technical checks

Before the durable-clock and RPC-budget corrections, the release root recorded:

- Actual held executable overwrite, resume/TTL recovery, and scoped cleanup on
  Windows and Linux, with 1,158 and 1,191 maintenance checks respectively.
- R1 parity on each OS, including the 100-frame native opening corpus and eight
  scenarios, with legacy-default and modern-isolation behavior preserved.
- Root and muxcore Go test and vet suites, 143 focused maintenance checks, and
  full race coverage across the five selected packages.
- Native consumer Scenario 5b with two sessions, six Unix lifecycle cases, and
  the complete critical suite, 5/5 with exit zero. The seven known review
  corrections were closed in the accepted source.

Later timing/RPC and cold-start checks have source-bound receipts in
[release evidence](specs/002-upstream-maintenance-hold/release-evidence.md).
Native-family source `6fc7eb44853a6446283dc1c1fad7ab4db874f4f5` has actual
Windows original-overlay RED and normal/race GREEN: four top-level tests and
28 named rows fail before repair; seven top-level tests and 45 named rows pass
afterward. Named totals include parents, not independent scenarios. Root tests
and both vet suites pass at their recorded source. Fixture-only successor
`435bcfa70da85f3763f1ddbc861016f2e18b07c4` closes the old owner-test failure
with fresh whole-muxcore PASS, without production changes after 6fc7. Historical
RED remains preserved; Linux/caller/CI and final review/acceptance are pending.

These are technical-check facts, not final release verdicts. The release root
separately proves the actual version-baked artifact, exact merged head, remote
tags, Go module resolution, and fresh-session delivered-artifact canary under
the [release protocol](docs/RELEASE-PROTOCOL.md).

## Upgrade and consumer handoff

After publication and tag resolution, use the `v0.31.0` binary release or the
product's versioned-engine upgrade path. Pin library consumers with:

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.31.0
```

Fresh Engram handoff for aimux, engram, and any other impacted consumer remains
pending. It must include released-version/module-resolution evidence, optional
maintenance adoption instructions, aware/old-shim/old-daemon limits, finite scope,
terminal lifecycle refusal, fresh modern admission, and safe rollback. Ordinary
legacy users do not need maintenance-specific source changes. Publication alone
does not establish consumer adoption or `CONSUMER_HANDOFF_PASS`.

---

# mcp-mux v0.30.0

**Release date:** 2026-08-31

**Type:** Additive, opt-in minor release

## Summary

R1 adds `--mcp-protocol=2026-07-28` for a known MCP `2026-07-28` host and a
same-era upstream. The selected route opens one dedicated native modern owner
and forwards the opening request unchanged. It is a safety-contained boundary,
not a legacy-to-modern gateway, a sharing release, or an automatic upgrade.

## Explicit modern route

The modern route is available only when a host explicitly selects
`--mcp-protocol=2026-07-28` before its opening request. It requires the pinned
modern request metadata and an exact-era admission result; it does not probe,
infer, or fall back to legacy.

- A valid opening request reaches the same-era upstream without mux-authored
  legacy initialization, discovery, list, cache, template, or replay traffic.
- Modern owners are forced isolated: they serve the selected downstream route
  and are never reused as a shared owner.
- Upstream JSON-RPC requests are contained rather than forwarded to a host.
  Eligible standard logs are forwarded only on the opted-in request path to
  its sole downstream recipient; mcp-mux neither synthesizes nor broadcasts
  them.

## Compatibility and migration

Existing installations need no configuration or source changes. Legacy remains
the default, and its observable behavior and owner identity remain unchanged.

To adopt R1, use the explicit selector only for a known modern host and
same-era upstream. Treat a refused, absent, or mismatched era confirmation as
a failed modern admission, not as permission to retry the route as legacy.
R1 deliberately provides no automatic probing, fallback, semantic protocol
translation, modern sharing, or persisted modern handoff.

## Operational readback

Existing owner, daemon, list, CLI, and `mux_list` views that project
`OwnerInfo` identify an active R1 owner with four redacted policy facts:

- `protocol_era=2026-07-28`
- `sharing_policy=forced-isolated`
- `cache_policy=off`
- `lifecycle_policy=r1-quarantine`

Existing readiness information remains available where it already exists. The
R1 readback does not expose request content, credentials, opaque state, or
linkable compatibility material.

## Lifecycle safety

R1 keeps process-generation and `RetirementProven` authority in the existing
owner lifecycle. A modern owner may continue only with its exact selected era;
an unsafe snapshot, handoff, reaper, zero-session, retry, respawn, loss, or
reconnect transition drains, cold-starts, or fails closed instead of becoming
legacy.

After upstream or daemon loss, unfinished modern requests, progress,
subscriptions, and replay state are not restored or replayed. A host uses a
fresh exact-era admission and issues a new retry or listen request when it is
ready to resume work.

## Validation scope

Validation covers the 100-frame native opening corpus, same-era byte
preservation, forced isolation, minimal redacted readback, legacy parity,
lifecycle quarantine and loss behavior, rollback, Windows and Unix customer
proof, full Go test and vet suites, and the repository critical suite.

## Rollback

Rollback stops new explicit-modern admissions and drains or removes modern
owners through R1 quarantine. It never downgrades live modern work to legacy,
hands it to a legacy owner, or replays unfinished work. If a product or
dependency rollback is required, restore the prior compatible revision after
the bounded modern-owner retirement path; do not force a mixed-era live
handoff.

---

# mcp-mux v0.29.1

**Release date:** 2026-07-19

**Type:** Backward-compatible patch release

## Summary

v0.29.1 adds a public, provider-generic start fallback helper to
`muxcore/supervisor` and makes daemon registry mutations exact-generation
transactions. These changes tighten lifecycle authority without changing the
ordinary `engine.New` path.

## `supervisor.StartWithFallback`

`supervisor.StartWithFallback` starts the requested engine first and tries a
distinct fallback only when that attempt fails cleanly with neither child nor
admission authority.

- If a failed attempt retains child or admission authority, the helper returns
  that authority to `supervisor.Run` for finalization instead of starting a
  second generation.
- `ErrStartRollbackUnproven` is terminal even when only admission cleanup is
  available: closing admission is not proof that a process tree was retired.
  The supervisor therefore remains fail-closed rather than overlapping
  authorities.
- Cancellation is preserved. A canceled requested attempt does not start a
  fallback; cancellation from a fallback attempt is also returned, with any
  retained authority still available for supervisor finalization.
- Returned error classifications do not expose product engine identities.

## Exact-generation daemon registry updates

Owner-originated persistence, template-cache, zero-session, and upstream-exit
callbacks now update the daemon registry through one daemon-owned transaction.
The transaction applies only when the originating owner is still the current
registry generation for its server ID. Stale generations are no-ops, and
process-generation authority remains in `muxcore/owner`.

## Compatibility

This patch is backward compatible for ordinary `engine.New` consumers and for
existing supervisor users. Products that need requested/fallback start policy
can adopt `supervisor.StartWithFallback`; they should continue to keep product
engine selection and policy outside muxcore.

## Upgrade

After the tags resolve, upgrade muxcore consumers with:

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.29.1
```

For the product binary, use the versioned-engine upgrade path:

```powershell
.\mcp-mux.exe upgrade --restart
```

## Rollback

To roll back this patch, pin `muxcore/v0.29.0` or restore the previous product
binary. Do not force a mixed-version live handoff; use the product's bounded
replacement path.

## Verification scope

The release verification scope covers focused supervisor fallback behavior
(clean failure, retained authority, rollback-unproven, and cancellation),
exact-generation daemon registry mutation behavior, and the relevant public
API and lifecycle regression coverage. These notes prepare the release; they
do not claim a final tag or publication.

## Post-publication consumer handoff

After publication, Aimux and Engram require fresh handoff against the exact
released version, including their module-resolution, provider commit, and
consumer verification evidence. Their adoption follows publication and is not
represented as completed by these notes.
