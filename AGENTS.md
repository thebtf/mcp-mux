# mcp-mux — MCP Stdio Multiplexer

## STACKS

```yaml
STACKS: [GO]
```

## PROJECT OVERVIEW

A local stdio multiplexer/proxy for MCP (Model Context Protocol) servers. Allows multiple Claude Code sessions to share a single instance of each MCP server, reducing process count and memory usage by ~3x.

### Problem

Each Claude Code session spawns its own copy of every configured MCP server (stdio transport). With 4 parallel sessions × 12 MCP servers = ~48 node processes consuming ~4.8 GB RAM. Most of these servers are stateless — they don't need per-session isolation.

### Solution

`mcp-mux` is a transparent command wrapper. User prefixes their MCP server command with `mcp-mux`:

```json
{ "command": "mcp-mux", "args": ["uvx", "--from", "...", "serena", ...] }
```

First mcp-mux instance for a given server becomes the "owner" (spawns upstream, listens on IPC). Subsequent instances connect as clients.

```
CC 1 ──stdio──> mcp-mux ──IPC──┐
CC 2 ──stdio──> mcp-mux ──IPC──┤──> mcp-mux (owner) ──stdio──> engram (1×)
CC 3 ──stdio──> mcp-mux ──IPC──┤
CC 4 ──stdio──> mcp-mux ──IPC──┘
```

### Key Concepts

- **Upstream**: A real MCP server process (e.g., engram, tavily)
- **Downstream**: A CC session connecting via mcp-mux wrapper
- **Owner**: First mcp-mux instance — spawns upstream, accepts IPC connections
- **Client**: Subsequent mcp-mux instances — connect to owner via IPC
- **Shared** (default): One upstream serves all clients
- **Isolated** (`--isolated`): Each client gets its own upstream


## MCP 2026-07-28 native boundary (R1 default, R2 opt-in)

R1 is an additive opt-in for a known MCP `2026-07-28` host and a same-era upstream. It preserves the existing legacy default, legacy identity, and legacy route. It is not protocol negotiation or a legacy-to-modern bridge.

### Select the protocol era before owner election

- CLI callers select R1 with `--mcp-protocol=2026-07-28`.
- Library callers set `engine.Config.ProtocolPolicy` to `era.PolicyModern20260728`.
- The zero value, `era.PolicyLegacyOnly`, keeps existing legacy ingress.
- `MCP_MUX_ISOLATED`, `--isolated`, `MCP_MUX_STATELESS`, `--stateless`, and `x-mux.sharing` remain sharing inputs. They never select an MCP protocol era.
- `clientInfo`, discovery responses, upstream names, and failed modern attempts also never select an era.

The selected `era.ProtocolEra` travels through `control.Request.ProtocolEra`, `control.Response.ProtocolEra`, `owner.OwnerConfig.ProtocolEra`, and `owner.ResilientClientConfig.ProtocolEra`. Modern admission requires an exact `2026-07-28` control echo before attachment. The opening frame is one newline-delimited JSON-RPC request with `params._meta.io.modelcontextprotocol/protocolVersion` set to `2026-07-28` and an object-valued `params._meta.io.modelcontextprotocol/clientCapabilities`. `io.modelcontextprotocol/clientInfo` is optional, but valid string `name` and `version` values are required when it is present. On isolated R1, the same frame reaches the same-era upstream unchanged; R2 rewrites correlation fields as described below. `server/discover` is valid when the host sends it; mcp-mux never generates it.

### Keep modern traffic native and isolated

R1 forces one downstream recipient per modern owner. Its `sharing_policy` is `forced-isolated`, regardless of command, arguments, CWD, environment, historical sharing flags, or an existing legacy owner. A modern route never falls back to, attaches to, or reuses a legacy owner.

Do not add legacy bootstrap or recovery behavior to this route. A modern owner sends no mux-generated `initialize`, `notifications/initialized`, roots/list, list-change, cache, template, or replay traffic. It does not use response caching, template reuse, cache invalidation, or reconnect replay. A modern upstream JSON-RPC request is contained rather than forwarded to the host. On isolated R1, after request opt-in with `io.modelcontextprotocol/logLevel`, a valid request-scoped standard log reaches the sole recipient once. mcp-mux never synthesizes or broadcasts that log.

### Preserve owner and process authority

R1 retains the existing daemon, owner, `OwnerGeneration`, exact-current-process, in-flight, stale-event, bounded-retry, reaper, and zero-session gates. It does not add a modern daemon, reaper, spawn lock, replay loop, or cleanup authority. `upstream.Process.RetirementProven()` remains the gate before an owner retires process-tree authority or a replacement can proceed. When retirement proof is unavailable, retain the exact owner as authoritative and follow the existing finalization retry path. Do not delete it or start a competing or legacy replacement.

### Apply lifecycle quarantine

Current snapshot and handoff payloads are era-less. Exclude modern owners from those payloads instead of adding an R1 era field or schema version. A later modern launch cold-starts after fresh explicit admission or fails closed. It never restores or attaches as legacy.

For reaper, zero-session removal, retry rehydration, respawn, daemon loss, upstream loss, and downstream reconnect, preserve the exact modern era or drain and remove, cold-start, or fail closed. Retry rehydration may reuse the existing bounded mechanism only when the exact modern era and policy remain present. After a loss, clear existing request, progress, and subscription state once. The host sends fresh native traffic and a new `subscriptions/listen` request when needed. No request, multi-round-trip retry, progress update, subscription, cache state, or legacy bootstrap replays.

### Readback, rollback, and exclusions

The exact control echo proves admission. Existing direct owner status and status/list projections that carry `control.OwnerInfo` report these policy facts for an active isolated R1 owner:

```text
protocol_era = 2026-07-28
sharing_policy = forced-isolated
cache_policy = off
lifecycle_policy = r1-quarantine
```

Existing readiness fields keep their established meaning. The R1 facts never expose request payloads, opaque request state, credentials, token history, raw environment values, authorization material, cache/template data, or route identifiers.

To roll back, stop callers from selecting `era.PolicyModern20260728` or `--mcp-protocol=2026-07-28`, then drain or remove existing modern owners through the existing lifecycle path. Never convert live modern work to legacy or replay unfinished work.

R1 excludes modern sharing or reuse, shared causal correlation, semantic translation, automatic dual-era fallback, modern response caching or template reuse, persisted modern snapshot or handoff transfer, automatic subscription continuation, and new registry, topology, lifecycle-state, or counter readbacks.

### R2 requires explicit exact-context admission

Before changing modern sharing admission or correlation, read
[the R2 protocol contract](docs/mux-protocol.md#mcp-2026-07-28-r2-exact-context-sharing)
and [consumer API contract](muxcore/README.md#v0320--r2-exact-context-modern-sharing).
R2 requires explicit modern era, `ModernSharing`, and a known non-secret
`control.PartitionAssertion` from `engine.Config.PartitionResolver`; CLI uses
`--mcp-sharing=shared --mcp-partition=<key>`. Absent/unknown authority stays
isolated, and explicit isolation wins. Never substitute a universal partition.

The daemon validates a 30-second binding for the exact daemon, nonce,
normalized launch, canonical CWD, all effective upstream environment, and
partition before existing owner election. CLI/engine explicit sharing normalizes
`cwd`/`git` to internal `global`; exact CWD still binds and isolation wins.
Only the two launcher controller advertisements are absent from upstream env;
do not replace exact-env comparison with a transient whitelist.
`OwnerConfig.ModernShared` is the admitted result, not a bypass. Reconnect
re-resolves and re-admits; raw token refresh cannot attach a shared owner.
Managed restart retains the partition/policy and gets a fresh nonce/binding;
no unfinished work replays.
Keep keys, raw environment, digests, and bindings out of status. Preserve one
generation-owned correlation authority for IDs, progress, subscriptions, and
cancellation; terminal events remove once and unowned events never broadcast.
Shared `logLevel` requests error before upstream work; isolated R1 logging
and bytes stay unchanged. Cache/replay/bootstrap/persistence quarantine and
existing lifecycle authority still bind. R2 does not virtualize application state.

For stable-launcher modern changes, preserve explicit
`supervisor.Config.ProtocolEra` selection (zero stays legacy), raw native
frames, and no legacy init/replay. Fail written/uncertain work once on generation
loss; retain only definitely unwritten FIFO work. Active RPC/subscriptions
block verified dormancy; fresh modern wake sends no initialize. Keep the shared
CLI wrapper parser before child startup and stop parsing at the upstream
command; payloads, `clientInfo`, stateless mode, and upstream flags cannot select era.

## Upstream maintenance hold consumer contract

The binary/library target is v0.32.0. Maintenance was introduced in v0.31.0 and
requires aware binary/daemon/shim versions v0.31.0 or newer; it is optional for
ordinary legacy `engine.New` consumers. Use the exact local status `server_id`, then the returned
opaque `hold_id`:

```text
mcp-mux hold <exact-server-id> --ttl 5m --drain-timeout 10s --json
mcp-mux renew <hold-id> --ttl 5m --json
mcp-mux resume <hold-id> --json
```

Only durable, unexpired `HELD` with `trees_retired=true` permits installer-owned
replacement. `HOLDING` and `RETIREMENT_BLOCKED` stay fenced; blocked retirement
never TTL-clears. TTL defaults to 5m, is positive, and is at most 1h. Drain
defaults to 10s; zero skips grace, not tree-death proof. Resume/renew
affect only the exact current lease; renew sets expiry from serialized acceptance
and cannot revive a released/expired lease. Resume/safe expiry require tree death
and durable release.
CLI TTL and drain durations must be whole milliseconds; reject sub-millisecond
values instead of truncating a positive drain to zero-force retirement.

Native finalization, including ordinary removal, must retain the owner/session
producers until reader/disconnect settlement and actual callback return. Closing
or cancelling is not quiescence; losing the registry entry early can orphan work
before a later hold. Reuse blocked finalization/exact-entry retry, not a new waiter
or scheduler. `PendingRequests` is request-only, not complete native-work proof;
private nonrequest accounting adds no public metric, state, API, or schema.

Retain every session through actual reader removal, including plain/no-hook and
accepted pre-token connections. Publish private acceptDone before launch in all
three constructors; serialize ReaderWG Add/launch with admission closure. Reader
Done follows full deferred close/remove cleanup; accept closes admission before
Wait and acceptDone. Transport completion is separate from request/nativeWork
grace; sample actual process proof after the bounded existing-budget join. Reuse
real reader cleanup, not a redundant cleanup counter, new timeout or fake Done.

Retirement publication retries namespace-lock contention through the existing
exact-lease timer, without a reaper. Successful blocked renewal preserves pending
retry intent on the replacement lease's guarded timer; it changes only accepted
expiry, not the original drain deadline. Publication preserves accepted expiry
and drain authority; stale lease/timer callbacks cannot mutate replacement authority.
Non-contention namespace-lock errors from exact-current pending retirement
callbacks/current retirement timers latch existing `maintenanceFailed` authority.
Direct consumers without a reaper fail closed; contention/renewed-blocked retry
and original lease clocks stay unchanged. Never delete authority to reopen admission.

Configured authorization must retain private callback authority through return
and registration/rejection in every owner mode, not only `SessionHandler`.
Serialize late admission against listener closure; handoff/retirement must join
that settlement without changing public `PendingRequests` or nil-authorizer defaults.

Every configured frame hook reserves through actual return regardless of owner
mode; 1ms verdict timeout is not settlement. For in-process HandlerFunc, closed
bookkeeping or requested cancellation is not retirement proof: actual body/pipe
`Done` is required. Preserve EOF/drain grace before owned child cancellation,
and keep ignoring-cancellation work blocked. Fixture cleanup releases barriers
before Shutdown; never preclose Done, re-pin synthetic proof, or weaken deadlines.

A durable `HOLDING` seed has provisional timing and never grants replacement.
After its first complete writer acknowledgment, sample `T` once and persist clocked `HOLDING` once.
TTL/drain use `T`; that write, retirement, HELD persistence, and response consume the original window.
Any acquisition write failure retains the seed fence; incomplete recovery is `RETIREMENT_BLOCKED`, with no expiry/resume.

Without a positive caller timeout, neutral `control.SendWithTimeout` allows 180s plus one drain for `hold`/`restart_owner`.
Explicit positive budgets are honored; other commands retain 5s. This finite exchange allowance does not promise full-pin/storage completion.
A timeout leaves outcome unknown: a durable lease/restart may remain. Inspect status, with no automatic retry/resume or stop fallback.

CLI `stop` must terminate with status 1 after a contacted-daemon error or invalid/
untyped failure response, never continuing into per-owner/data fallback. Genuine
daemon absence retains its existing path. Keep recovered timers and control
request serving paused until every fallible daemon-construction step succeeds;
failed construction must not retain a stale expiry mutation or admit control work.

Reserve acceptLoop producer synchronously under the existing Start/Close mutex
before launch; accepted handlers remain counted through response/connection/
after-callback completion. ENOTSOCK absence is dial-only, never contacted error.
Alias positives must prove os.SameFile parent identity/canonical scope/member bytes;
absolute/relative aliases execute on both platforms without skips, while owned
inner-link-before-.. traversal is Unix-specific, not a guard relaxation.

All resolved MCP stops use daemon authority; accept legacy owner fallback only
for the exact known old unsupported-stop response, never transport/malformed/
typed uncertainty. New external operator claims take maintenance read admission
before the daemon mutex; release both before finalization/waits. Existing exact
entry already-owned whole-daemon cleanup remains a distinct private claim, not
a failure-latch clear or external bypass. Preserve admitted retry and maintenance
removal. Reject restart drain overflow before
mutation regardless of raw transport or explicit timeout. Unix cleanup skips
only the exact canonical owned bound control path, without early serving.

Activation must preserve raw status errors and authorize offline only with exact
native dial absence plus locked persisted-clear proof, never nondial errno or a
Boolean collapse. MCP restart last-consumer cleanup is one returned token-only
IPC connection plus existing exact current ServerID/PrevToken confirmation before
success and closing only its own connection. Nonidle denial confirms binding,
not eviction; unknown token/read uncertainty is not success. Reuse native history/
EOF authority, not MCP/bootstrap frames, token minting, revoke/fallback manager,
or altered library-consumer semantics.

Process-owning signal/context callers must wait for daemon `Done` after a refused
shutdown, keeping reaper/reference/control authority alive. Request shutdown once;
do not infer a retry from lease clearance. A separately explicit admitted shutdown
is needed before caller return. A direct-method refusal test is not signal/context proof.

Scope is the selected owner's finite admitted CWD/era/security/configuration/
namespace context set, not a host-wide executable lock. Aware shims preserve
host pipes, return original numeric/string ID `-32005` maintenance errors before
cache or forwarding, and never replay held or terminated work. Notifications
have no invented replies. Modern resumed demand uses fresh same-era isolated
admission without legacy bootstrap, cache, or subscription restoration.

Era-less snapshot/handoff payloads do not prove the complete historical environment
inventory: `CwdSet` lacks associated environments and retained `BoundTokens` are
only observations. Ordinary no-lease restore remains available, but hold acquisition
for an incomplete restored owner and restoration under an active lease fail closed
with existing `maintenance_invalid`. Do not add a payload schema/version, API, or
expiry seam. The causal case loses an admitted environment before token consumption;
elapsed TTL is not its runtime oracle.

First post-resume `OnInject` demand uses the same admission/activation path as stdin,
waiting outside ingress/dormant/suspend locks. Healthy injection retains one-pass
lock admission and nonblocking queue-capacity behavior; the callback remains single-fire.
Copy caller bytes before unlocked revalidation, then recheck closed/dormant state and
maintenance sequence before enqueue. Preserve `ErrInjectClosed`, `ErrInjectFull`,
FIFO and suspend accounting; no new queue, goroutine, API, or replay.

Controlled restart/handoff/shutdown/downgrade and idle exit refuse terminally
under any fence, including launcher/library update fallback paths. Aware
unplanned recovery reads durable authority before admission. Old daemons and
uncoordinated standalone paths are `maintenance_unsupported`; never substitute
stop, PID cleanup, or direct exec. Old shims receive physical start fencing only,
not immediate-error/non-replay guarantees. Arbitrary old binaries, foreign
engines, and manual active-pointer changes are unsupported. Before downgrade,
durably resume each exact retired lease through the current aware binary or
observe safely committed expiry. Incomplete/blocked authority prevents downgrade;
preserve both authority members even after TTL. Never delete them to open admission.
Contacted graceful-restart transport uncertainty retains its original cause and
is terminal. Existing shutdown fallback after an explicit ordinary rejection
requires fresh validated maintenance-aware/clear status for the SAME nonempty
daemon generation; failed shutdown is terminal. Old read-only startup compatibility
is unchanged. This is not permission to bypass a typed maintenance refusal.

Maintenance authority is `<canonical namespace-lock path>.maintenance/<scope digest>/`
with ledger/transaction members and unchanged scope/schema/state. Never choose a
new store through UserConfig/HOME or infer clear authority from absence under an
unsafe parent. Preserve raw endpoint-parent components while authenticating every
alias/target before canonicalization; private anchor/scope/members require native
owner/mode/protected-DACL guards. Exact TrustedInstaller trust is ancestors-only.
The endpoint/lock directory must survive every active fence; no cleanup authority.
Windows writable ordinary endpoints require a nonreplaceable incoming binding,
held no-delete directory handles and parent-relative no-follow child opens.
Keep strict ancestors/private authority and existing trusted-alias policy; no
group allowlist, namespace relocation or host/profile ACL repair. Activation uses
`daemon.AcquireMaintenanceForActivation` with an existing read-only regular lock,
the original byte-range primitive and write sharing for older writers. Retain its
closer through live status and pointer commit; preserve pending bytes until commit.
Released v0.30 had no store. Drain/clear older unreleased live candidates with the
same binary/original environment before cutover; keep their files, no scan/migrate/
registry/bypass. Safe verification-root approval is not a production TMP override
or permission to change profile ACLs. A rejected unsafe native test path is not
healthy execution PASS or a user fault.

Controlled update/install/swap, layout/bootstrap mutation, and active-pointer
changes serialize with hold-ledger mutation under the existing daemon namespace
file lock. `daemon.CheckMaintenanceForActivation` is read-only. Status and pure
startup reads do not acquire/write that lock or proactively start a daemon.
Activation also checks live aware status; offline/old endpoints are allowed only
after locked persisted-clear proof. Do not introduce a separate updater lease
or treat namespace coordination as host-wide file-lock authority.

`mux_hold`, `mux_resume`, and `mux_renew` use the local daemon; `mux_restart` uses
daemon-owned `restart_owner`, not ambient-context reconstruction. See
[`muxcore/README.md`](muxcore/README.md#upstream-maintenance-control)
for additive optional APIs and typed errors. Root owns the live cross-platform
proof in production Scenario 11, focused maintenance regressions, existing
Scenario 5b/8 and R1 parity gates, integration, and release. Smoke scratch/output/
evidence must be explicitly supplied beneath PRIMARY `.agent`, never a linked
inner `.agent` or OS temp. No maintenance smoke is implicitly enrolled in the
critical suite.

## CONVENTIONS

- Investigation reports: `.agent/reports/YYYY-MM-DD-topic.md`
- Diagnostic data: `.agent/data/topic.md`
- Action plans: `.agent/plans/topic.md`
- Specifications: `.agent/specs/topic.md`
- Architecture decisions: `.agent/arch/decisions/NNN-title.md`

## RULES

| Rule | Description |
|------|-------------|
| **No stubs** | Complete, working implementations only |
| **No guessing** | Verify with tools before using |
| **Reasoning first** | Document WHY before implementing |
| **Spec compliance** | MCP protocol spec is authoritative — verify all protocol behavior against it |

## RELEASE PROTOCOL

Public releases must follow [`docs/RELEASE-PROTOCOL.md`](docs/RELEASE-PROTOCOL.md).
When a release contains a critical or consumer-impacting `muxcore` update,
the release closeout must include `CONSUMER_HANDOFF_PASS`: fresh Engram
issues or comments for `aimux`, `engram`, and any other impacted muxcore
consumer. If Engram cannot be updated, report `CONSUMER_HANDOFF_BLOCKED` and
do not call the full critical muxcore scope shipped.


## Load reference detail only for the affected branch

- For current muxcore integration, public APIs, SessionHandler, authorization,
  frame hooks, supervisor, or maintenance, read [muxcore/README.md](muxcore/README.md)
  at the relevant section. It is the current consumer reference owner.
- For IPC/framing/protocol changes, read [docs/mux-protocol.md](docs/mux-protocol.md).
- For lifecycle regression or production verification work, read the affected
  scenario in [docs/PRODUCTION-TESTING-PLAYBOOK.md](docs/PRODUCTION-TESTING-PLAYBOOK.md).
- For a release or consumer-impacting library change, load the release protocol
  above; for past-version rationale/migration incidents, read
  [CHANGELOG.md](CHANGELOG.md#historical-migration-and-incident-notes). Historical
  commands and old consumer version pins are dated evidence, not current install
  instructions or permission to mutate another product.

These are triggered reads, not eager imports. The CLAUDE bridge imports only
this canonical contract; do not add `@` imports of history or API catalogs.
