# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.31.0] - 2026-10-03

Prepared for publication as `v0.31.0` and `muxcore/v0.31.0`; tag publication,
delivered-artifact proof, and the required Engram consumer handoff remain pending.

### Added

- Added optional managed upstream maintenance for #135 through CLI `hold`,
  `resume`, and `renew`, MCP tools `mux_hold`, `mux_resume`, and `mux_renew`,
  and `control.SendMaintenance`. Existing control interfaces retain their
  signatures, with optional handlers, typed errors, and redacted lease readback.
- Added a durable finite-context admission fence for the selected owner's
  already-admitted namespace, CWD, era, and security/configuration contexts.
  A usable hold requires durable, unexpired `HELD` and proven death of every
  scoped managed process tree. This is not a host-wide executable lock.

### Changed

- Aware managed shims preserve host pipes and reject held requests before cache
  or forwarding with original numeric/string-ID `-32005` maintenance errors.
  Held and unfinished retired work never replays; notifications get no invented
  replies. Fresh demand after durable release can start one replacement generation.
- Controlled restart, handoff, shutdown, downgrade, idle exit, and update-helper
  fallback refuse terminally under a fence. Unplanned aware-daemon recovery loads
  durable authority before admission. Incomplete or blocked authority fails closed.
- Controlled install, launcher swap, layout/bootstrap mutation, and active-pointer
  changes serialize with hold-ledger mutation under the existing daemon namespace
  lock. Status and pure startup inspection remain read-only. Offline/old activation
  requires locked persisted-clear proof.
- TTL defaults to 5m and is positive and at
  most 1h. Exact-current-lease renewal sets expiry from serialized acceptance,
  without changing retirement state or reviving an expired/released lease.
  Resume and safe expiry require tree death and durable release. Blocked retirement
  never TTL-clears. Drain defaults to 10s; zero skips grace, not
  tree proof. CLI durations require whole milliseconds.
- Acquisition durably writes a provisional `HOLDING` seed, then samples `T` once
  after the first complete writer acknowledgment and persists clocked `HOLDING`
  once. Subsequent writes, retirement, HELD, and response consume that original
  TTL/drain window. Write failure retains the seed fence; incomplete recovery
  remains blocked without expiry/resume. No schema or state is added.
- Neutral control defaults give `hold`/`restart_owner` 180s plus one drain;
  explicit positive caller timeouts remain unchanged and other commands retain
  5s. This is finite exchange headroom, not universal completion. On timeout,
  inspect status for an unknown durable outcome, without automatic retry/resume
  or stop fallback.

### Fixed

- Concurrent legacy `engine.New` cold starters wait for the winning daemon's
  bounded readiness check on namespace-lock contention instead of returning
  terminal `ErrFileLocked`; other lock errors remain unchanged.
- Native retirement now accounts for notification, connect/disconnect lifecycle,
  authentication, and frame-hook callbacks through actual return, while retaining
  native sessions as teardown producers. Ordinary removal and maintenance both
  require quiescence. Cancellation is not settlement; active work keeps retirement
  blocked without TTL/resume bypass. Public `PendingRequests` remains request-only.
- Plain sessions and accepted pre-token connections remain owned through actual
  reader/remove cleanup. Accept completion is reserved before launch; reader
  startup is serialized with admission closure and full join precedes process
  proof within the existing budget, without new public state/counter or timeout.
- Retirement publication retries namespace-lock contention through the existing
  exact-lease timer without a reaper. Successful blocked renewal preserves pending
  retry intent on the replacement lease's guarded timer, retaining the original
  drain deadline and legitimately accepted expiry; stale callbacks stay fenced.
- Recovered lease timers and control request serving activate only after daemon
  construction succeeds. Failed registry/control setup cannot leave a stale
  expiry timer that later overwrites renewed authority; accepted expiry is unchanged.
- CLI `stop` returns status 1 on a contacted-daemon error or invalid/untyped
  failure response, without per-owner or legacy data-channel fallback. Successful
  shutdown and the genuinely absent-daemon path retain their existing behavior.
- Unix stale-socket cleanup excludes the daemon's exact canonical bound control
  path before ping/unlink, preserving paused publication without early serving.
- Resolved MCP `mux_stop` always uses daemon authority. Soft/forced owner claims
  check maintenance atomically; uncertainty is terminal, and only the exact known
  old unsupported-stop response permits legacy compatibility fallback.
- `restart_owner` rejects drain milliseconds that overflow `time.Duration` before
  mutation, including raw control and explicit-timeout callers. Zero drain remains
  a real replacement; stop defaults remain 30s soft and 0ms forced.
- Exact already-owned whole-daemon cleanup retains its private removal claim
  under persistence failure; new external stop/shutdown/restart still fail closed.
  No failure-latch clearing, wire flag, or admission-proof weakening is added.
- Configured authorization callbacks are accounted through actual return and
  registration/rejection in every owner mode, including subprocess and HandlerFunc.
  Retirement/handoff cannot bypass their settlement; `PendingRequests` stays request-only.
- Signal/context shutdown callers wait for daemon `Done` after refusal, retaining
  reaper, daemon reference and control service. Clearing a lease does not retry the
  refused request; a separately admitted explicit shutdown is required.
- Configured frame hooks retain private work until actual callback return in every
  owner mode; the 1ms verdict timeout cannot discharge settlement authority.
- In-process `HandlerFunc` retirement requires actual body/pipe completion via
  `Done`, never bookkeeping close. Owned child context cancellation follows the
  existing EOF/drain grace in Close/SoftClose; ignored cancellation stays blocked.
- CLI stop preserves daemon authority on uncertain/invalid ping, not only shutdown
  errors; only proven absence permits existing owner/data fallback. Real read/EOF
  uncertainty is covered without inventing an independent write-timeout fixture.
- Maintenance authority is anchored beside the canonical namespace lock at
  `.maintenance/<scope digest>`, not mutable UserConfig. Native path/owner/ACL
  guards reject unsafe ancestry and foreign alias retargeting before canonicalizing.
  Existing scope/schema/state/protocol unchanged; endpoint/lock directory must persist.
- A stale regular-file endpoint is absence only for native Darwin dial ENOTSOCK;
  contacted ping/read uncertainty remains terminal. Control Start reserves the
  accept producer under the existing Close mutex before launching it, so Close
  waits for real accepted-handler settlement rather than racing Add/Wait.
- Activation probes preserve the raw status error and accept offline absence only
  through exact native dial classification, after namespace lock/persisted-clear
  proof; malformed/contacted/busy/timeout outcomes cannot authorize a swap.
- MCP `mux_restart` consumes its returned reservation with one native token-only
  IPC connection, confirms exact current owner/token history before success, then
  closes only that connection. Library `restart_owner` token semantics are unchanged;
  no MCP bootstrap, new wire command/token, or eviction permission is added.
- Fresh demand after durable release rechecks admission before writing to retired
  IPC and activates the admitted successor for connected and parked shims. The
  parked path consumes the existing private IPC-EOF wake; no new transport,
  controller, era fallback, or held/unfinished-work replay is added.

### Compatibility

- Ordinary legacy `engine.New` consumers need no source changes. Maintenance
  adoption is optional and requires aware managed binary/daemon/shim cooperation.
  Legacy remains the default; modern R1 remains explicit, same-era, forced-isolated,
  cache-off, and replay-off, with fresh admission after release.
- Old daemons and uncoordinated standalone paths are `maintenance_unsupported`,
  without stop/kill/direct-exec fallback. Old managed shims receive physical start
  fencing only, not immediate-error/non-replay guarantees. Arbitrary old binaries,
  foreign engines, unmanaged processes, and manual pointer swaps are not controlled.

### Validation

- Prior accepted-source checks include actual Windows and Linux held executable
  replacement and cleanup, focused maintenance/race coverage, root and muxcore
  Go tests and vet, native Scenario 5b, Unix lifecycle coverage, R1 parity on both
  operating systems, and the complete critical suite. These checks do not prove
  publication, the final delivered binary, or consumer adoption.
  Native-family proof at 6fc7 records seven top-level tests and 45 named PASS
  rows, including parents, in normal and race runs. Root tests and both vet suites
  pass at their recorded source. Fixture-only 435bcfa closes the old owner-test
  failure with fresh full-muxcore PASS. Earlier timer/stop/family proof stays
  source-bound. New Unix-endpoint, managed-stop, and drain-validation commits have
  scoped focused/race proof; caaba timeout and the ace2 observer's two P1s stay
  historical. Caller/auth repairs a68aa37/ab28d3d have Windows/native RED/GREEN,
  race and bounded source PASS. All four full/vet gates ran on identical frozen
  precommit bytes, not freshly after ab28. Frame/shared-handler completion repairs
  have focused/race proof; d786 supplies cooperative owned-context cancellation.
  Distinct c585/d786 module timeouts are retained as historical RED. Fixture-only
  3f preserves d786 production and now has fresh postcommit full-module GREEN;
  earlier root/vet retain their scopes. Ping896a and stable-ledger5e49 add focused
  Windows/Linux ping and Linux authority RED/GREEN/race plus source-only assurance.
  Windows unsafe-parent refusals are not healthy positive PASS. Clean Linux root/
  vet/public1182/R1 8+100 preserve old module RED. Later test-only engine/owner
  fixes have consumer-visible proof and native5ea module-race25/25 PASS, skips explicit.
  Alias/control focused proof and exact4e CI37199675520 all5 success are scoped;
  earlier baa full Linux/public1173/R1 and f902 CIall5 retain historical scopes.
  Ceb transport focused43/race43/callback58/preservation23 and final SOURCEPASS
  remain valid. Clean ceb root/vet/actual0.31.0/public1191/R1 PASS, module24/25
  and coverage reserved-write FAIL retained. Historical fixture-only c0 full-module
  race now passed25/25 packages/2021 positive leaves/4 explicit skips and both vet;
  its root/public1191/R1 artifact reuse was byte-bound to unchanged ceb production,
  not a newly built c0 binary. B86a998 adds retirement publication/blocked-renewal
  retry: native Linux original+renewed_blocked2 normal/race, async12 and expiry8
  race PASS with zero skips/data races. Initial c0 RED and first-fix renewal RED
  remain distinct; the exact-hash checker is SOURCE-only. Actual clean b86 Linux
  root-race306 positive/1 SKIP, module-race2023 positive/4 SKIP (25/25 packages),
  both vet0 and Scenario11 1191/1191 PASS are now recorded. The CGO0/trimpath
  artifact has clean embedded b86 and owner0.31.0; R1 100/100+8 PASS uses a separate
  CGO1 script-built binary, not that release artifact. Initial public startup
  refusals remain: exact private ancestor775→0700 and one fresh re-entry per
  script yielded PASS without source changes or suite/vet/release-build repeats.
  [B86 CI37221621672](https://github.com/thebtf/mcp-mux/actions/runs/37221621672)
  and docs-only [44 CI37222579349](https://github.com/thebtf/mcp-mux/actions/runs/37222579349)
  passed all5 jobs; doc44's bounded evidence check is SOURCE-facts PASS.
  Committed c183 reaper repair has causal non-contention failure-latch RED→two
  normal PASS and race3 top-level/5 named PASS rows including parents, zero skips/
  races. Removed independent timer scheduling is SOURCE-only, not a runtime count.
  Earlier reaper/owner disposition closed29 known threads; fresh26f enumeration
  has31 total/29 resolved/2 unresolved P2s: restore-environment completeness and
  first-fresh OnInject revalidation. Their repair/proof remains pending.
  Prior owner repair is committed/frozen at26f:
  original both-era RED→first-fix connected GREEN/race remains scoped to966d.
  Parked SOURCE deadlock and actual first-fix parked RED→final3 leaves plain/race
  (connected legacy/modern, parked legacy) plus8 complement race PASS bindf29,
  zero SKIP/data races. ParkedOwnerSeamRecheck closes F1 with bounded SOURCE-only
  PASS at exact e5/3da5 production hashes, not broad runtime assurance. Actual
  [owner reply4178899382](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4178899382)
  has exact UTF8 readback/native resolution;26f source is pushed to the PR branch.
  [Exact26f CI37226616613](https://github.com/thebtf/mcp-mux/actions/runs/37226616613)
  completed SUCCESS, all5 jobs. Actual immutable26f Linux full root/module race
  and both vets each pass once:27 tested packages PASS/3 no-test,2333 leaf PASS/
  5 named SKIP,0 failures/races. Clean CGO0/trimpath26f artifact/owner0.31.0 gives
  Scenario11 1191/1191 PASS; separate CGO1 R1 binary gives100/100+8/8 PASS, not
  release-version evidence. Supplemental R1 ELF version read failed/UNKNOWN;
  retained without retry, not an owning gate. These receipts do not certify new
  P2 WIP, Windows or delivery. Root's complete3-record runtime-grant recheck finds
  no mcp-mux project grant; only that local Windows positive effect is held,
  with task approval retained. No final all-PR CLEAN or release acceptance.

### Rollback

- Before pinning `muxcore/v0.30.0` or restoring a prior compatible binary, use the
  current aware version to durably resume each exact retired lease or observe its
  safely committed expiry. Preserve incomplete/blocked authority, even after TTL.
  Do not downgrade, delete authority, force PID cleanup, or bypass admission to
  escape a fence. Keep modern-owner quarantine and no-replay rules intact.
- Any live hold from an older unreleased user-config-store candidate must be
  cleared/drained using that binary and original environment before cutover.
  Preserve its private authority files. Released v0.30 had no maintenance store;
  no scan/migrate/new registry or missing-new-store bypass is provided.

## [0.30.0] - 2026-08-31

### Added

- Added the opt-in `--mcp-protocol=2026-07-28` route for a known MCP
  `2026-07-28` host and same-era upstream. A successful admission uses one
  dedicated native modern owner and forwards the opening request unchanged;
  it does not translate the legacy initialization protocol.
- Added minimal redacted R1 owner readback through existing `OwnerInfo`
  projections: `protocol_era=2026-07-28`,
  `sharing_policy=forced-isolated`, `cache_policy=off`, and
  `lifecycle_policy=r1-quarantine`.

### Changed

- Explicit-modern owners are always isolated. Their legacy bootstrap, shared
  response cache, template reuse, and replay paths are disabled. Upstream
  JSON-RPC requests are contained, and eligible standard logs remain
  request-scoped and sole-recipient.
- Modern lifecycle transitions retain the selected era under the existing
  process-generation and `RetirementProven` authority. An unsafe transition
  drains, cold-starts, or fails closed rather than restoring an era-less
  modern owner as legacy. After loss, unfinished modern work is not replayed;
  the host must issue a fresh exact-era retry or re-listen.

### Compatibility

- This is an additive, non-breaking opt-in. The default remains the released
  legacy path, whose externally observable behavior and owner identity remain
  unchanged. R1 does not add automatic protocol probing, fallback, modern
  sharing, or persisted modern handoff state.

### Migration

- Keep existing configurations unchanged to retain legacy behavior. To use
  R1, select `--mcp-protocol=2026-07-28` before the opening request only for a
  known modern host and same-era upstream; validate the modern admission
  result and use the four policy readback facts above for operational checks.

### Validation

- Release validation covers the 100-frame native opening corpus, same-era byte
  preservation, forced isolation and readback, legacy parity, lifecycle
  quarantine/loss behavior, rollback, and Windows and Unix customer proof.

### Rollback

- Stop new explicit-modern admissions, then drain or remove modern owners
  through R1 quarantine. Do not downgrade live modern work to legacy, hand it
  to a legacy owner, or replay unfinished work. Restore the prior compatible
  binary or dependency revision only after that bounded retirement path.

## [0.29.1] - 2026-07-19

### Added

- Added `supervisor.StartWithFallback` for provider-generic requested/fallback
  start policy. It retries only after a clean start failure with no child or
  admission authority, returns retained authority for supervisor finalization,
  treats admission-only rollback-unproven state as terminal even after cleanup,
  and reduces callback errors to fixed classifications that keep product engine
  identities opaque.

### Changed

- Owner-originated persistence, template-cache, zero-session, and upstream-exit
  callbacks now mutate daemon registry state through one exact-generation
  transaction. Stale owner generations remain no-ops, while process-generation
  authority stays in `muxcore/owner`.

## [0.29.0] - 2026-07-19

### Added

- Added the public `muxcore/supervisor` package for products that keep one MCP
  host stdio transport attached while replacing child engine generations. The
  supervisor provides bounded startup/replay/dormancy buffering, strict MCP
  validation, generation-safe request/cancellation/progress/Tasks correlation,
  original-ID errors for delivered requests lost during replacement, and
  capability-gated discovery list-change notifications.
- Added `supervisor.StartCommand`, which gives the supervisor complete Unix
  process-group or Windows Job Object retirement authority for a command child.
- Added the public `muxcore/supervisor/attest` package for one-shot local
  direct-parent and exact-child-PID attestation on Windows, Linux, and Darwin.
  Unsupported platforms fail closed for private lifecycle control while
  ordinary supervision remains available.

### Changed

- The stable `mcp-mux` launcher now delegates generic host transport, protocol,
  replay, correlation, and child-tree lifecycle mechanics to
  `muxcore/supervisor`. The product adapter retains active-engine authorization,
  version-store and fallback selection, bootstrap/update policy, shared-daemon
  ownership, and operator exit behavior.
- Launcher-only dormancy, wake-on-demand, and installed active-engine switches
  now preserve the original host stdio transport. Only the cached
  `initialize` / `notifications/initialized` handshake is replayed; arbitrary
  requests are never replayed.
- Rolling old/new combinations remain ordinary MCP sessions without private
  dormancy unless the exact child generation completes protocol-v2 bilateral
  attestation. Product-private method strings, exit codes, parsers, and adapter
  policy are not consumer APIs.

### Fixed

- Stale child generations can no longer forward or mutate request,
  cancellation, progress-token, or MCP task state after replacement.
  Finalized task status remains immutable when delayed `tasks/get`,
  `tasks/result`, cancel, or status traffic arrives, and retained correlation
  state stays bounded.
- A successor is not started until the previous command child's complete
  process-tree authority is retired. Start rollback that cannot prove authority
  cleanup fails closed instead of admitting an overlapping generation.

### Compatibility

- Ordinary `engine.New` consumers require no source changes. Products that need
  a stable host transport around replaceable engines should adopt
  `supervisor.Run`, `supervisor.StartCommand`, `supervisor.ProtocolV2`, and
  `supervisor/attest` rather than copying the `mcp-mux` product adapter.
- Roll back by pinning `muxcore/v0.28.0` or restoring the previous product
  binary. Do not force a mixed-version live supervisor handoff; use the
  product's bounded replacement path.

## [0.28.0] - 2026-07-17

### Added

- Added demand-driven upstream materialization for compatible template-backed
  owners. Host `initialize` / `tools/list` startup can now complete from cache
  with no upstream process, while the first uncached request materializes one
  generation and succeeds on the same open transport.

### Changed

- Template reuse now requires an exact full SHA-256 identity of the effective
  security-relevant environment, plus the exact canonical working directory
  for isolated templates; a stricter per-CWD isolated entry shadows any later
  relaxed template. Windows environment keys normalize case-insensitively
  before shim override, fingerprinting, or launch. A first template revision
  race performs one fresh lookup; a repeated mismatch takes one bounded
  cold/eager bypass.
- Process retirement, owner removal, snapshot fallback, and mixed handoff now
  retain the installed generation until both process completion and process-tree
  authority retirement are proven. Unproven finalization remains visible as
  `FINALIZE_BLOCKED` and retries retirement proof for that same installed
  generation without allowing a competing generation.
- Restart restore invalidates secondary discovery caches before refresh.
  Failed/rejected local demand clears request-scoped remap, pending, inflight,
  and progress residue instead of replaying later; session-token revocation is
  reserved for isolation eviction.
- Official CI and release artifacts now use Go 1.25.12. Root and muxcore
  `govulncheck` report zero reachable vulnerabilities under that toolchain;
  this does not treat an imported-but-unreached advisory as reachable.
- Graceful restart now treats listener/spawn/accept and exact-Hello negotiation
  failures as pre-detach aborts that retain the predecessor. A post-detach
  protocol failure must prove the failed successor exited, rewrite the pinned
  snapshot, and pre-start exactly one clean snapshot successor before the
  predecessor may shut down.
- Staged snapshot activation is transactional: an owner-construction failure
  rolls back partial registrations, preserves the exact pinned environment in a
  filtered recovery snapshot, and fails before the new control endpoint serves.

## [0.27.2] - 2026-07-17

### Fixed

- Fixed snapshot/template background startup racing the first uncached request
  into a second upstream respawn for the same owner. The request path now joins
  the existing bounded background start until the new generation either
  completes its `initialize` / `notifications/initialized` handshake or
  terminates. A successful generation cannot be overtaken by ordinary requests;
  terminal failure follows the existing explicit error/respawn path while
  preserving one authoritative upstream process tree.
  Proactive discovery IDs and response claims are now owner/generation scoped,
  so dead-generation entries are drained and stale or unclaimed responses are
  dropped before they can change caches, pending state, or session routing.

## [0.27.1] - 2026-07-14

### Fixed

- Fixed a permanent `can_suspend` retry herd during rolling coexistence: a
  v0.27 shim could retry the v0.26.13 `unknown command: can_suspend` response
  every five seconds, driving the daemon to multiple CPU cores when hundreds of
  retained transports were present.
- Fixed malformed, missing, unknown-token, owner-gone, and persistent-owner
  gate outcomes being retried indefinitely. They now keep the data-plane IPC
  connection open and disable idle suspension for that connection.
- Fixed healthy `can_suspend` checks scaling with daemon owner count. Product
  shims now send the exact spawn-returned owner ID, while the owner-local token
  history and current owner entry remain authoritative.

### Changed

- Persistent owner retention and downstream transport retention are separate:
  persistent consumers retain transports by default, while products with an
  explicit no-background-events or buffering contract can opt into
  `AllowPersistentIdleSuspend`.
- `engine.New` now automatically binds positive `IdleSuspendDelay` values to
  the exact spawn-returned daemon owner/token safety gate; direct resilient
  clients still supply their own gate.
- Private dormant frames now require protocol-v2 target-bound launcher
  attestation over a one-shot local IPC endpoint, plus direct-parent executable
  and active-engine proof. Forwarded environments from old launchers fail
  closed; verified active children may bootstrap the stable launcher for future
  invocations after one host/session restart.
- `MCPMUX_LAUNCHER_DORMANT_LEASE` offers explicit bounded full-transport exit
  for hosts proven to relaunch after closure; it is disabled by default.

- Retryable daemon/transport failures now use capped per-token exponential
  backoff with jitter. Busy, pending-request, and active-progress denials remain
  recheckable without a synchronized fixed cadence.

### Verification

- Added the exact v0.26.13 wire response, malformed-response, live cross-owner,
  stale same-ID recreation, deterministic lookup-count, and retry-cap tests.
- Added mixed-version runtime proof with a real v0.26.13 daemon and v0.27.1
  shim: one failed gate probe, no later polling across two former retry windows,
  live host stdio, and zero run-scoped survivors.
- Added live Windows and Linux proof that a direct child accepts launcher
  attestation while the same endpoint forwarded through an intermediate old
  launcher is rejected without writing private bytes to host stdout.
- Added Unix success-path socket removal and command-start cancellation
  regressions so failed supervisor respawn loops cannot accumulate attestation
  endpoints or file descriptors.

## [0.27.0] - 2026-07-13

### Added

- Added bounded shim idle suspension and launcher dormancy. Disposable product
  shims park their daemon session after safe inactivity, retain exact-owner
  reconnect for a grace window, and wake only when the host sends new demand.
- Added handoff protocol v2 for transactional transfer of owner stdin, stdout,
  stderr, and the
  single process-tree authority across same-version engine replacement.
- Added cross-platform process-lifecycle acceptance covering eight parallel
  isolated sessions, launcher-only convergence, demand wake, installed
  active-engine switching, and descendant cleanup.
- Added opt-in `ResilientClientConfig.IdleSuspendDelay`, `IdleSuspendGate`, and
  `IdleDormantGrace` controls for direct muxcore consumers.

### Changed

- Windows subprocesses are contained in Job Objects before user code can run;
  Unix subprocesses use one-shot process-group authority. Leader exit now
  finalizes descendants before replacement or completion is reported.
- The first handoff-v1 to handoff-v2 upgrade uses one bounded snapshot-backed
  restart. Same-v2 replacement retains live process authority and exact-token
  reconnect state.
- Classified isolated owners reject fresh consumers while keeping their
  authenticated listener available for the same consumer's reconnect token.
  Provisional fan-in reservations are revoked without invalidating the creating
  shim.
- Persistent owners and owners with pending requests, progress tokens, or busy
  declarations remain protected from idle cleanup.

### Fixed

- Fixed orphaned Serena, WebView, language-server, and helper descendants
  surviving after their CLI consumer or upstream leader exited.
- Fixed duplicate isolated process trees caused by concurrent startup,
  proactive classification, dormant wake, token-refresh, and cleanup races.
- Fixed dormant wake abandoning successful-but-undelivered spawn reservations:
  the launcher replay budget now exceeds the child spawn budget, and control
  write failure revokes the exact pending token before normal owner cleanup.
- Fixed request-loss ambiguity during reconnect: already-sent requests receive
  an explicit JSON-RPC error with their original ID, while only the cached
  `initialize` handshake is replayed.
- Fixed partial handoff adoption and handle-cleanup paths so uncommitted process
  authorities are aborted and closed.
- Fixed timeout escalation for handoff-adopted processes whose transferred
  tree authority exists without a local `procgroup.Process`, and made legacy
  two-FD public handoff input fail fast with an explicit compatibility error.

### Documentation

- Documented lifecycle defaults and environment overrides, persistence policy,
  v1-to-v2 compatibility, rollback behavior, forbidden local workarounds, and
  the distinction between Serena dashboard configuration and process cleanup.

[Unreleased]: https://github.com/thebtf/mcp-mux/compare/v0.31.0...HEAD
[0.31.0]: https://github.com/thebtf/mcp-mux/compare/v0.30.0...v0.31.0
[0.30.0]: https://github.com/thebtf/mcp-mux/compare/v0.29.1...v0.30.0
[0.29.1]: https://github.com/thebtf/mcp-mux/compare/v0.29.0...v0.29.1
[0.29.0]: https://github.com/thebtf/mcp-mux/compare/v0.28.0...v0.29.0
[0.28.0]: https://github.com/thebtf/mcp-mux/compare/v0.27.2...v0.28.0
[0.27.2]: https://github.com/thebtf/mcp-mux/compare/v0.27.1...v0.27.2
[0.27.1]: https://github.com/thebtf/mcp-mux/compare/v0.27.0...v0.27.1
[0.27.0]: https://github.com/thebtf/mcp-mux/compare/v0.26.13...v0.27.0
