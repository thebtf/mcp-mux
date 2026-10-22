# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.33.0] - 2026-10-23

### Added

- Added opaque per-admission `SessionMeta.SessionID`, optional
  `ProjectLifecycleWithSessionMeta`, and `AuthorizeSessionWithSessionMeta`.
  Upgraded callbacks take precedence without duplicate legacy calls; existing
  interfaces and nil defaults remain compatible.
- Added opt-in per-owner `MaxConcurrentRequests` and
  `MaxConcurrentNotifications` settings across engine, daemon, and owner config.
  Nonblocking admission precedes handler goroutines: request overflow returns
  original-ID `-32004` and notification overflow drops without reply. A positive
  request limit bounds the full dispatch goroutine, including response writing
  and deferred cleanup, not just callback execution. A blocked slow peer can
  intentionally consume owner-wide capacity and reject sibling requests rather
  than allow unbounded queued response writers. Notification slots remain held
  through actual callback return; slot release and work accounting stay deferred
  through actual callback settlement, including error, cancellation, and panic
  paths. Nonpositive values remain unlimited; consumers must explicitly configure
  bounds, such as 64 requests/64 notifications.

### Fixed

- Fixed ghost lifecycle callbacks and sibling-session reservation release:
  reservations use the same exact-session preauthorization ID through teardown,
  and connect settles before dispatch/disconnect. Sessions registered before a
  maintenance fence retain connect-then-disconnect settlement even if the fence
  or `Close` occurs before connect runs. Only `AuthAllow` admissions aborted
  before registration get one matching upgraded disconnect without a late
  connect or releasing a sibling's reservation. Actual callback settlement
  remains part of maintenance and reader/teardown proof.
- A panic in a legacy or metadata-aware connect callback is logged and closes
  only the affected session, without dispatching requests for it. After connect
  settles, its matching disconnect cleanup runs once, including partially
  established lifecycle state; the owner and sibling sessions remain available.

### Compatibility

- Full consumer adoption requires the metadata authorizer and upgraded lifecycle
  methods to reserve/release by `SessionMeta.SessionID`, plus explicit per-owner
  request/notification limits such as 64/64. A dependency bump alone cannot fix
  a project-only FIFO reservation tracker or enable bounds. No AIMUX or Engram
  consumer upgrade is claimed.

## [0.32.0] - 2026-10-22

### Added

- Added opt-in MCP `2026-07-28` exact-context sharing through CLI
  `--mcp-sharing=shared --mcp-partition=<nonsecret-key>` and
  `engine.Config.ModernSharing` with `PartitionResolver`. The daemon validates
  a short-lived binding for the exact launch, CWD, all effective upstream environment,
  and known partition before reusing an owner. Unknown authority retains R1
  isolation; explicit isolation wins. Legacy behavior remains unchanged.
  Explicit CLI/engine sharing normalizes identity mode without relaxing exact
  CWD comparison. Only two launcher controller advertisements, never delivered
  upstream, are excluded from environment comparison; no transient whitelist
  is added. Managed restart preserves sharing policy with fresh admission.
- Added generation-bound session correlation for modern IDs, progress,
  subscription acknowledgements/filters/metadata, and cancellation. Shared
  request-scoped `logLevel` returns an error before upstream work; isolated R1
  logging is unchanged. Modern bootstrap, cache, replay, and persisted recovery
  remain disabled; no arbitrary application-state virtualization is implied.
- Added explicit native-era stable supervision through
  `supervisor.Config.ProtocolEra`, with legacy behavior as the zero value.
  Modern frames bypass legacy initialization/replay; generation loss fails
  written or uncertain work once, retaining only definitely unwritten FIFO
  entries. Active RPCs/subscriptions prevent verified dormancy; a fresh modern
  wake sends no legacy initialize. Launcher and engine share pre-child wrapper
  option validation, stopping before upstream arguments.
- Documented Serena's fixed-project tools-interface configuration and smoke
  boundaries, distinguishing documented HTTP multi-client support from the
  stdio-through-mux inference without claiming memory-savings benchmarks.

## [0.31.0] - 2026-10-05

Published as [`v0.31.0`](https://github.com/thebtf/mcp-mux/releases/tag/v0.31.0)
and `muxcore/v0.31.0`. Native Engram handoff was not filed under an explicit
operator exception; production consumer upgrades are not claimed.

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
- Non-contention namespace-lock failure during exact-current pending retirement
  callback or current retirement timer now latches existing `maintenanceFailed`
  authority. Direct consumers without a reaper fail closed instead of silently
  losing publication intent. Contention and renewed-blocked retry remain unchanged;
  no new state, scheduler, clock reset or authority-deletion bypass is added.
- Automatic retirement callback/timer publication write failures and ordinary
  expiry write/non-contention lock failures now latch existing `maintenanceFailed`.
  Guarded expiry cannot delete a latched lease before an RPC's failure check;
  known current-lease renew/resume refusal preserves safe lease readback. Shared
  store/reconciliation stay unchanged:healthy explicit durable retry and owned
  contention/ticker retry remain,without another controller or failure bypass.
- Library `ApplyUpdateAndRestart`/`RestartWithSuccessor` and launcher engine-switch
  restart keep contacted graceful-restart transport uncertainty terminal with its
  original cause. Unaware or invalid endpoints cannot select shutdown fallback.
  An explicit ordinary rejection permits fallback only after fresh validated
  maintenance-aware/clear status for the same nonempty daemon generation; failed
  shutdown is terminal. Existing old-daemon read-only startup compatibility stays.
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
- Era-less snapshot/handoff restore no longer treats retained CWD/token observations
  as a complete historical environment inventory. Ordinary no-lease restore remains;
  hold acquisition for incomplete restored owners and active-lease restore fail
  closed with existing `maintenance_invalid`, without payload schema/version/API changes.
- First post-resume `OnInject` demand shares stdin admission/successor activation,
  outside lifecycle locks. Healthy one-pass admission, caller-buffer copying,
  closed/full sentinels, maintenance sequence, FIFO and suspend accounting remain;
  no new queue, goroutine, API, or replay is added.

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
  Earlier29 closure and26f enumeration31/29/2 are historical. Restore-environment
  completeness and first-fresh OnInject repairs are committed/focused-proven.
  Actual [restore reply4179041769](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4179041769)
  and [injection reply4179041932](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4179041932)
  have exact UTF8/in-reply-to readback and native resolutions. All31 known threads
  are root-resolved, not a fresh final-head/all-PR CLEAN verdict.
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
  retained without retry, not an owning gate. Restore947e and injection1e45 are now
  committed/frozen at `1e45a13f176e8322c2f6530ded433a1cbfa2df9e`. Each focused receipt
  proves immutable26f plus only its two overlays, not the full integrated1e source.
  Restore: actual unsafe HELD/tree retirement/forgotten-environment spawn RED→typed
  invalid/no new lease/same live owner GREEN and race,1 leaf each;9 complement leaves
  PASS under race. Loss precedes token consumption; TTL extension is SOURCE-only,
  with no executed expiry claim. Injection: connected both-era/parked RED4 named
  records→GREEN4; race12 top-level/18 named PASS records including parents, not18
  leaves,0 SKIP/races. Integrated1e source is pushed to the existing PR branch;
  Actual immutable1e Linux normal root/module tests, both vets and full-race suites
  each once exit0:27 tested packages/3 no-test;2339 positive race leaves/5 named
  SKIP,0 failed/races. Clean CGO0/trimpath1e owner0.31.0 binary gives Scenario11
  1191/1191; separate unstamped CGO1 R1 100+8 PASS reports1e45a13f, not0.31.0.
  [Exact1e CI37229280285](https://github.com/thebtf/mcp-mux/actions/runs/37229280285)
  FAILED: coverage/Ubuntu/BSD PASS; macOS daemon and Windows daemon component fail
  `TestMaintenanceSnapshotPendingCompatibleEnvironmentScopeFailsClosed` at line924,
  `actual snapshot unexpectedly preserved the optional admitted environment`.
  Windows job was canceled after its observed daemon failure, not a failed job verdict.
  Parent's SOURCE trace finds a four-assertion guard, not proof that the optional
  environment survived. Logs show AddCwd now2 roots: raw/canonical spelling differs
  on Darwin `/var`→`/private/var` and Windows case normalization, breaking the
  `CwdSet` cardinality assumption. Fixture-only `91dc293afb71059097fcdb65c6a07cf8feadb0b3`
  removes raw-cardinality/env-slot assumptions: original26 production/new fixture
  actual unsafe HELD/retirement/forgotten-spawn RED→current plain/race1 leaf PASS
  each+9 guard race leaves,0 SKIP/races. Production/proof11 inputs match1e,460
  excluded files unchanged; no build/public repeat/full91/Windows/macOS PASS.
  Callback fixture-order correction now has scoped native race8/8 PASS; c18 CI RED retained.
  Root's complete3-record grant recheck finds no mcp-mux project grant;
  only optional local Windows proof is held, not task approval. Bootstrap finds
  approved target absent: CREATE that exact non-reparse directory before
  `/writable-root add C:/Users/btf/AppData/Local/mcp-mux-verification-01a0fb9a`,
  then `/writable-root status` with exact root/origin/project/expiry/writer readback.
  That grant holds only the optional local route, not release globally. Required
  Windows+Unix Scenario11 proof is OS-native, not workstation-specific. Root has
  selected explicit maintenance-proof-v0.31.0 hosted Windows opt-in. Workflow-only
  `c18bfc7619f42d5fc9330ea5c0c5a31634abacaf` is committed/pushed after91; root label
  readback preceded push. actionlint/YAML/P7 AST both steps PASS, STATIC only.
  [CI37234225216](https://github.com/thebtf/mcp-mux/actions/runs/37234225216) boundc18
  FAILED: macOS daemon `TestMaintenanceSessionHandlerRetirementWaitsForActualCallbackReturn/0/short_drain`
  line231, `actual callback reservations were not counted` (0.02s leaf), retained verbatim.
  Old restore line924 failure is absent. Windows/Ubuntu daemon components PASS
  (127.325s/101.865s), but both jobs CANCELED by fail-fast before hosted steps;
  BSD/coverage PASS. Atc18 no Windows Scenario11 ran/artifact/PASS or aggregate
  GREEN was established. Static selection remained valid;344 supersedes that gap.
  SOURCE: opening reply flush precedes deferred Pending decrement; receiving it
  does not join cleanup. Two callbacks can coexist with that opening reservation.
  CI's !=2 guard did not log a value:3 is SOURCE-possible, not observed/no Mac cause.
  One-file a93c3b6 fixture freeze removes exact-two snapshots, asserts positive-grace
  hold waits for actual callback return after original-ID errors, retains drains/
  clocks/BLOCKED/Done/cancel-without-return/resume refusal/same-pipe successor.
  Actual one-invocation native race8/8 original subtests PASS,0 SKIP/errors/race
  warnings/stderr, on immutablec18+onlya93. Root committed/pushed test-only
  `34410430507ef771cf8d88598bb7ede7afcdfb5d`;110 selected production and broader
  production/proof11 inputs equal1e. Live excluded equality REFUSED4 concurrent
  docs drift, no WIP imported/staging claim; root must bind final doc hashes.
  Exact344 [CI37236880161](https://github.com/thebtf/mcp-mux/actions/runs/37236880161)
  completed SUCCESS, all5 jobs. Selected Windows job111537660496's opt-in,
  checkout, Scenario11 and evidence-retention steps all SUCCESS; artifact11315977986
  `maintenance-proof-windows-v0.31.0-37236880161-1` was retained/downloaded.
  Actual Windows JSON/build receipt PASS: source/pr-head/clone344 equal, clean
  embedded344 Go1.25.12 CGO0/trimpath binary SHA256
  `cf6ab1afa0c3cf41a59caf97ffc0620be7d76aac0bbebaf44d64350d2be23cde`;
  smoke exit0,1158/1158 checks,0 cleanup errors, owner0.31.0 in23 observations.
  This closes Windows premerge proof only;1e Linux full/artifact evidence retains
  its source scope, not a fresh344 Linux full run. Final review disposition,
  doc-hash admission/source-plus-docs acceptance and release remain root-owned.
  No prod/schema/helper-clock/deadline/OS skip or extra SubprocessTreeDone case added.
  Conditional second checkout uses exact event
  PR head, not synthetic merge; fresh full LOCALAPPDATA PRIMARY LF/no-hardlinks
  clone uses `.agent/tmp`,0.31.0 build/existing Scenario11. Only redacted windows.json/
  build-receipt.json upload; ordinary triggers/tests unchanged. Actual job/JSON
  hashes are recorded in release evidence; no local profile/ACL/bypass effects.
  Later bounded daemon proof on immutable315+only53cd/d504 establishes actual
  original71dc callback/timer lock-error RED2 leaves, then normal/race PASS each
  3 top-level/5 positive leaves,0 SKIP/race warnings/stderr,each invocation once.
  Actual OS path errors exercise native retirement and unchanged clocks/durable
  bytes/fences/stale-entry guards; root accepts scoped behavior, not new full CI,
  artifact or release proof. Receipt/full hashes are in release evidence.
  Historical exact344 enumeration artifact1349 had33 threads/31 resolved/2 pending.
  Root subsequently posted/read back replies4179716104→4179066572 and
  4179716099→4179436225 and resolved both:all33 then-known,not fresh all-PR CLEAN.
  Daemon ae21825 and six-file fallback74d5293 are committed; their original
  independent focused projections remain immutable315+only respective overlays.
  New tests with old344 production give4 causal engine RED leaves
  (both public helpers:old/uncertain endpoint,shutdown1/starts1) and cmd6 RED/
  1 known-aware-clear PASS. Final normal/race each prove engine38+cmd13 positive
  leaves,51 distinct/102 total,0 SKIP/data races/unsettled; each family runs once.
  Real same owner and same authenticated host IPC survive refusals. Retained
  engine110-byte socket setup failure is not causal RED; approved short private
  root max85-byte sockets supply actual proof. LF archive rejection is retained.
  Full six source hashes/receipt10b290 limits are in release evidence. Actual
  integrated74 Linux root2/module25 tested normal packages plus3 root no-test,
  both vets once PASS; fresh CGO0/trimpath clean embedded74/owner0.31.0 binary
  `07ef0ebd1c2597a4f032748685d265e3e87efe58939dc19acd6db475d53b3951`
  gives Scenario11 1191/1191+23 owner0.31.0 rows. Separate CGO1/non-trimpath
  unstamped74d5293c R1 binary gives100+8 PASS,not release-version parity.
  Unix selected Scenario8 six named/eight terminal events PASS0FAIL/SKIP;
  461Git blobs0mismatch/no WIP; no local full-race duplicate(CI owns).
  Ancillary stale-BaseDir stat failure retained,current-base provenance corrected,
  no gates repeated.0 matching processes/socket registrations/authority;3 inactive
  identity-bound sockets retained,not deleted. Full final74 receipt/hash in release evidence.
  [Exact74 CI37242471098](https://github.com/thebtf/mcp-mux/actions/runs/37242471098)
  RED: Mac new4 live-host fixture cases before helper,line1110,owner socket104 bytes
  exceeds Darwin103. Ubuntu/coverage/BSD PASS;Windows CANCELED before public steps.
  Test-only2b8ffa short namespace filepath.Base(existingmp*base) now has actual
  immutable74+ONLYfixture Linux race4/4 original branches PASS once,0SKIP/races/
  errors/stderr;460 other files including production/proofscripts byte-equal.
  Private53-byte TMP/prebound97/101 gives observed owner96/97/control100/101
  <=Darwin103. Root accepted fixture-only source/behavior,not nativeDarwin/nextCI.
  Receipt43ff6e49/full hash in release evidence;oldMac failure not rerun,no normal run.
  Production/proofscripts unchanged74. Selected workflowbfabb5e/5a8e5d7f has
  actionlint/YAML3AST STATIC PASS;canonical hosted critical runtime pending.
  Actual issue135 milestone5985521449 posted/readback. Final source+fixture+docs/
  next CI/critical proof,merge/tags/delivery/canary pending;native Engram issue
  capability unmounted,effect-only,no substitute/all-PR CLEAN/release completion.
  Later0119 [CI37244791265](https://github.com/thebtf/mcp-mux/actions/runs/37244791265)
  has allOS race/vet/module PASS and Windows Scenario11 step SUCCESS,but aggregate
  RED:critical build/lifecycle/time3 PASS,topology cleanup status1 FAIL,native5b
  not reached. Python selection correction worked,not a five-step critical PASS.
  Topology fixture later committed4aa7 has independent existing-producer Linux
  proof ca0a1eb0:five idle assertion booleans true,old/new daemon PIDs absent,
  listener ENOENT. Windows crosscompile only;native Windows five-step pending.
  New automatic-write-failure receipt84ac3ed1 binds immutable4aa7+ONLY3 daemon
  overlays:original53cd two families5 leaves gives4 causalRED/1healthy contention
  PASS;final9families34positive leaves normal/race each once,0FAIL/SKIP/races/setup.
  Real authority-leaf writes after PREPARE/native retirement and recovered path+
  pastTTL remain fenced,exact HoldID/clocks/bytes preserved;healthy explicit retry
  retained.217 actual authority files/71pairs retained despite0taskPIDs/sockets;
  empty configured authority directory is not absence proof,no deletion/release.
  Root accepts this one-class scoped repair,next atomic3source+2docs commit pending;
  sharedstore/reconcile unchanged,no exact errno or whole-phase fault claim.
  Remote0119 snapshot35total33resolved has2 new P2s:4179727154/PRRT_kwDORq0kOM6o3fCS
  and4179791527/PRRT_kwDORq0kOM6o3pHP. No remote resolution/futureclosing inferred.
  Linux74 artifact is prior production only;new integrated full/build/public/CI/
  nativeWindows critical proof and release remain parent-owned,pending.
  Historical8142 proofs/failures remain scoped. No all-PR CLEAN or delivery claim.

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

[Unreleased]: https://github.com/thebtf/mcp-mux/compare/v0.33.0...HEAD
[0.33.0]: https://github.com/thebtf/mcp-mux/compare/v0.32.0...v0.33.0
[0.32.0]: https://github.com/thebtf/mcp-mux/compare/v0.31.0...v0.32.0
[0.31.0]: https://github.com/thebtf/mcp-mux/compare/v0.30.0...v0.31.0
[0.30.0]: https://github.com/thebtf/mcp-mux/compare/v0.29.1...v0.30.0
[0.29.1]: https://github.com/thebtf/mcp-mux/compare/v0.29.0...v0.29.1
[0.29.0]: https://github.com/thebtf/mcp-mux/compare/v0.28.0...v0.29.0
[0.28.0]: https://github.com/thebtf/mcp-mux/compare/v0.27.2...v0.28.0
[0.27.2]: https://github.com/thebtf/mcp-mux/compare/v0.27.1...v0.27.2
[0.27.1]: https://github.com/thebtf/mcp-mux/compare/v0.27.0...v0.27.1
[0.27.0]: https://github.com/thebtf/mcp-mux/compare/v0.26.13...v0.27.0


## Historical migration and incident notes

Moved from the former eager AGENTS library catalog on 2026-10-10. These are
release-era records, including old consumer pins, test inventories, debug
observations, and a then-proposed API. They are not current runtime proof or
authority to execute old upgrade/recovery commands. For current integration
and supported update/maintenance behavior, use [muxcore/README.md](muxcore/README.md)
and the canonical [AGENTS.md](AGENTS.md) safety boundaries. The versioned
entries above own newer release history.

### v0.26.9 - transparent upstream respawn for live sessions

**No breaking changes.** v0.26.9 makes pipe-backed owners treat an upstream
process exit as a recoverable upstream-generation failure instead of owner or
session teardown when live sessions remain attached. Muxcore drains current
in-flight requests with explicit JSON-RPC `upstream process exited` errors by
original id, respawns the replacement upstream inside the same owner, refreshes
MCP discovery caches, and lets the next request on the same downstream session
hit the replacement upstream automatically.

Consumer impact: update to v0.26.9. Do not add product-local direct-stdio
smokes, binary-renaming schemes, retry loops, or manual reconnect hacks for
ordinary subprocess upstream crash/update recovery; muxcore owns the
same-session respawn protocol. Pure `SessionHandler` native consumers still use
the explicit muxcore live-update helpers for product binary replacement.

### v0.26.8 - snapshot tools/list cache refresh fix

**No breaking changes.** v0.26.8 keeps snapshot-restored `tools/list` cache
available while the replacement upstream refreshes in the background. This
prevents startup-time `tools/list` timeouts where a restored owner had cached
tools, cleared them before refresh, and then a new MCP host session waited on a
slow live upstream until the host startup budget expired.

Consumer impact: update to v0.26.8. Do not add product-local cold-start,
binary-renaming, or cache invalidation workarounds for this issue; muxcore owns
the snapshot-cache refresh protocol.

### v0.26.7 - consumer docs target correction

**No runtime changes from v0.26.6.** Use v0.26.7 as the current consumer target
so tagged source docs and agent-facing instructions point at the released
auto-managed namespace implementation instead of v0.26.5.

### v0.26.6 - auto-managed engine namespace

**No breaking changes for ordinary `engine.New` consumers.** `engine.Config.Name`
is now a human-readable/status label, and muxcore derives a collision-resistant
transport namespace automatically from the label plus product identity. Daemon
control sockets, owner sockets, daemon locks, stale-socket cleanup, snapshot
restore, and reconnect behavior use the resolved namespace.

| API | Semantics |
|-----|-----------|
| `engine.Config.Name` | Optional display label surfaced in status and registry descriptors. Empty derives from Go build metadata or executable name. |
| `engine.Config.Namespace` | Optional legacy/advanced transport namespace override. Empty is the normal value; muxcore derives the namespace automatically. Set only to preserve a previously shipped namespace. |
| `daemon.Config.Namespace` | Lower-level direct-daemon override. Empty defaults to `daemon.Config.Name` for compatibility; ordinary consumers should use `engine.New`. |

Consumer impact: new consumers should not hand-pick pipe names. Existing
consumers that must keep a shipped namespace for rolling compatibility can set
`Namespace` explicitly during migration.

### v0.26.5 - process-explosion owner fanout reduction

**No breaking changes.** v0.26.5 reduces two confirmed owner/process fanout
vectors after startup storms:

- forced-isolated retry counters are cleaned when the final owner in that retry
  family is removed;
- global/shared env bucketing ignores ordinary launch noise (`PATH`, temp dirs,
  shell/session metadata) while preserving credential/config/proxy/URL/host/port
  and cert identity boundaries.

Consumer impact: update to v0.26.5; no muxcore API change is required. This
release does not kill or clean already-running stale workstation processes. It
prevents new fanout from these source-side vectors after consumers deploy the
new muxcore/runtime.

Operational invariant: a Windows named pipe that is already bound is healthy
only if the control endpoint answers ping/status within the bounded readiness
window. Bound but unresponsive is an abnormal recovery state, not replacement
readiness and not permission to spawn a competing daemon.

### v0.26.4 - Windows occupied control-pipe startup guard

**No breaking changes.** v0.26.4 includes the v0.26.3 restart reconnect /
launcher upgrade protocol plus a Windows startup guard for occupied but
unresponsive daemon control pipes. When a control endpoint is already bound but
does not answer ping, `mcp-mux` now waits and fails clearly instead of spawning a
competing daemon against the same named pipe. A genuinely missing pipe remains a
normal startup miss.

Consumer impact: update to v0.26.4 or newer; no muxcore API change is required. This
does not recover an already-unresponsive daemon process by killing it. Recovery
still needs explicit operator or product policy.

### v0.26.3 - restart reconnect and launcher upgrade protocol

**No breaking changes.** v0.26.3 fixes restart reconnect behavior and stable
launcher upgrade protocol issues. It is superseded by v0.26.4 for Windows
control-pipe startup reliability.

### v0.26.2 - consumer docs target correction

**No runtime changes from v0.26.1.** Use v0.26.2 as the current consumer target
so tagged source docs no longer point agents at the older v0.25.3 install
snippet.

### v0.26.1 - opt-in daemon registry ownership hardening

**No breaking changes.** v0.26.1 includes the v0.25.3 native SessionHandler
hot-update helpers and the v0.26.x opt-in daemon registry. Consumers that do
not set `engine.Config.Registry` remain invisible to cross-engine discovery and
keep existing behavior.

| API | Semantics |
|-----|-----------|
| `engine.Config.Registry *registry.Config` | Optional daemon advertisement. Nil is the opt-out zero value. |
| `registry.Config{ProductName, MuxcoreVersion, Capabilities}` | Descriptor metadata for products that want central read-only visibility. |
| `registry.Capabilities{ListOwners: true}` | CR-001 read-only capability used by `mux_engines` and scoped `mux_list(engine_name)`. |
| `mux_engines` | mcp-mux operator tool that lists opted-in muxcore daemons as healthy/stale/invalid/duplicate after status verification. `duplicate` means multiple healthy descriptors share an engine name; stale leftovers stay stale. |
| `mux_prune_engines` | mcp-mux operator tool that dry-runs by default and removes only stale / invalid native registry descriptor files when called with `dry_run:false`. It never stops native muxcore daemons, owners, processes, or control sockets. |
| `mux_list(engine_name: "...")` | Explicit read-only owner listing for one registered engine. Default `mux_list` remains scoped to `mcp-mux`; `sessions` counts downstream MCP clients/shims, which can exceed visible terminal sessions. |

Descriptor verification rejects PID mismatches, and daemon shutdown removes a
descriptor only if it still belongs to the same process. CR-001 does not add
cross-engine stop/restart/update. Stale descriptors left by crashed or test
daemons can be garbage-collected through `mux_prune_engines` after verification.

### v0.25.3 - native SessionHandler live update helpers (#239)

**No breaking changes.** The live-update helpers are additive; existing
`upgrade.Swap`, control commands, and engine configuration keep their current
behavior. v0.25.3 superseded the v0.25.1/v0.25.2 release candidates and
included the active-control-socket wait fix needed for reliable fallback start
after graceful restart.

| API | Semantics |
|-----|-----------|
| `engine.UpdateAndRestartOptions` | Caller supplies `CurrentExe`, `StagedExe`, drain/restart/shutdown/ready timeouts, stale cleanup preference, and optional lock-failure behavior. |
| `engine.RestartWithSuccessorOptions` | Caller supplies the explicit successor executable for stable-launcher/versioned-engine topologies without renaming the launcher. |
| `engine.UpdateAndRestartResult` | Reports partial success: `OldPath`, daemon-running state, lock acquisition, graceful restart, shutdown fallback, replacement start/readiness, stale cleanup count, and warnings. |
| `(*engine.MuxEngine).ApplyUpdateAndRestart(ctx, opts)` | Runs the reusable provider sequence: `upgrade.Swap`, daemon namespace lock, `graceful-restart`, shutdown fallback, wait-for-exit, replacement daemon start, and ready wait. |
| `(*engine.MuxEngine).RestartWithSuccessor(ctx, opts)` | Runs the restart sequence with an explicit `SuccessorExe`, but does not rename files. Use after the consumer has updated its active engine pointer. |
| `control.Request.SuccessorExe` + `control.GracefulRestartOptionsHandler` | Additive control-plane extension so a caller can tell the old daemon which executable should become the successor during graceful restart. Older handlers fall back to the legacy `HandleGracefulRestart(int)` path. |

**Migration for aimux after muxcore release:**

Use this helper only for the fixed replaceable engine path topology. If aimux
or another consumer uses a stable launcher plus versioned engine store, update
the active engine pointer and invoke graceful restart with the intended
successor executable instead of passing the launcher path as `CurrentExe`:

```go
result, err := eng.RestartWithSuccessor(ctx, engine.RestartWithSuccessorOptions{
    SuccessorExe:    nextEngineExe,
    DrainTimeout:    30 * time.Second,
    RestartTimeout:  60 * time.Second,
    ShutdownTimeout: 10 * time.Second,
    ReadyTimeout:    30 * time.Second,
})
```

Use `ApplyUpdateAndRestart` only when `CurrentExe` is the replaceable engine
binary that should exist after the swap:

```go
result, err := eng.ApplyUpdateAndRestart(ctx, engine.UpdateAndRestartOptions{
    CurrentExe:      currentExe,
    StagedExe:       currentExe + "~",
    DrainTimeout:    30 * time.Second,
    RestartTimeout:  60 * time.Second,
    ShutdownTimeout: 10 * time.Second,
    ReadyTimeout:    10 * time.Second,
    CleanStale:      true,
})
```

Contract: surviving shims reconnect automatically to the replacement daemon
when possible. This is transport continuity, not request replay; in-flight
requests may receive explicit JSON-RPC errors by original ID.

For `SessionHandler` consumers, this is not heap-state migration. Muxcore
preserves shim transport, owner/snapshot metadata, cached MCP discovery
responses, classification, cwd/env metadata, reconnect-token history, and
reconnect behavior. Product-private handler state must be persisted by the
consumer or reconstructed by the successor daemon.

Healthy SessionHandler hot-update evidence: same open session reports the new
product executable/version, `daemon_generation` changes,
`handoff.restored_owner_count > 0`, `shim_reconnect_refreshed > 0`,
`shim_reconnect_fallback_spawned == 0`, and `shim_reconnect_gave_up == 0`.

**Tests landed in this release:**
- `TestRestartWithSuccessor_ValidationErrors`
- `TestRestartWithSuccessor_GracefulSuccessUsesExplicitSuccessor`
- `TestRestartWithSuccessor_GracefulErrorFallbackStartsSuccessor`
- `TestRestartWithSuccessor_PreparesControlSocketBeforeCleanFallbackStart`
- `TestRestartWithSuccessor_TreatsAlreadyBoundReplacementAsReady`
- `TestPrepareControlSocketForReplacement_WaitsOnActiveSocketFalseNegative`
- `TestPrepareControlSocketForReplacement_ReturnsAfterEndpointUnavailable`
- `TestApplyUpdateAndRestart_GracefulSuccessUsesSuccessorExe`
- `TestApplyUpdateAndRestart_DaemonNotRunningOnlySwaps`
- `TestApplyUpdateAndRestart_SwapFailureStopsBeforeDaemonCalls`
- `TestApplyUpdateAndRestart_GracefulErrorFallsBackToShutdownAndStart`
- `TestApplyUpdateAndRestart_LockFailureIsPhaseError`
- `TestApplyUpdateAndRestart_FallbackExitTimeoutIsPhaseError`
- `TestApplyUpdateAndRestart_StartFailureIsPhaseError`
- `TestApplyUpdateAndRestart_ReadyTimeoutIsPhaseError`
- `TestApplyUpdateAndRestart_SessionHandlerConsumerUsesEngineNamespace`
- `TestGracefulRestartPassesSuccessorExeToOptionsHandler`
- `TestSuccessorExecutableRequestOverrideWins`

---

### v0.24.0 — multi-tenant extensions: ConnInfo + SessionMeta + AuthorizeSession + OnFrameReceived (#109, #110, #111, #112)

**No breaking changes.** All four extensions are additive — nil-default
callbacks and optional interface upgrades preserve pre-v0.24 behavior
byte-identically for non-adopting consumers.

| API | Semantics |
|-----|-----------|
| `muxcore.ConnInfo` | New value type carrying OS-level peer identity (`PeerPid`, `PeerUid`, `Platform`). Populated once at accept time via `peerCreds(conn)` on Linux (SO_PEERCRED), Windows (`GetNamedPipeClientProcessId` via the `Fd()` exposed by `*go-winio.win32File`), and macOS (LOCAL_PEERPID + GetsockoptXucred). Zero-value semantics: `PeerPid==0`/`PeerUid==0` ≡ "unavailable". Stable platform identifier constants: `PlatformLinuxUnix`, `PlatformWindowsNamedPipe`, `PlatformDarwinUnix`, `PlatformUnknown`. |
| `muxcore.SessionMeta` | New value type combining `Conn ConnInfo` (OS facts) with `TenantID string` + `AuthorizedAt time.Time` (consumer-policy fields produced by AuthorizeSession). `IsAuthorized()` discriminator distinguishes "callback not configured" (zero `AuthorizedAt`) from "AuthAllow with empty TenantID" (legitimate per FR-3). |
| `muxcore.AuthDecision` + `muxcore.SessionAuth` | New types backing AuthorizeSession verdicts (`AuthAllow`, `AuthDeny`). |
| `muxcore.FrameAction` | New type backing OnFrameReceived verdicts (`FramePass=0`, `FrameDrop=1`, `FrameError=2`). Numeric ordering preserves fail-open semantics — `FramePass` is the zero value, returned on timeout / panic / nil callback. |
| `muxcore.NotificationHandlerWithSessionMeta` | Optional handler upgrade — receives `SessionMeta` on every notification. Owner type-asserts at dispatch; handlers satisfying both legacy and `*WithSessionMeta` see ONLY the WithSessionMeta path (EC-7). |
| `muxcore.SessionHandlerWithSessionMeta` | Optional handler upgrade — receives `SessionMeta` on every request. Same dispatch precedence as above. |
| `engine.Config.AuthorizeSession` | Single-shot per-session admission gate. Invoked AFTER IPC handshake / peer-creds extraction and BEFORE AddSession. AuthDeny closes the connection with JSON-RPC -32000 + reason and never spawns upstream. AuthAllow stamps SessionMeta.TenantID + AuthorizedAt. Panics recovered → AuthDeny{Reason:"authorize panic"}. |
| `engine.Config.OnFrameReceived` | Per-frame inbound admission hook. Synchronous on reader goroutine, 1ms budget (overrun → FramePass), panic-safe (panic → FramePass). FrameDrop = silent discard; FrameError = JSON-RPC -32004 with msg.ID preserved; FramePass = normal dispatch. Scope is INBOUND ONLY (CHK014) — outbound responses and synthetic notifications are not intercepted. |

**Migration:** Existing consumers (aimux, engram, mcp-launcher) require zero source change. All callbacks default to nil; both new handler interfaces are optional upgrades.

**Migration for aimux (multi-tenant adoption):**

```go
import muxcore "github.com/thebtf/mcp-mux/muxcore"

// Optional handler upgrade — consume meta.Conn / meta.TenantID inside
// every dispatch.
type aimuxHandler struct{ /* ... */ }

func (h *aimuxHandler) HandleRequest(ctx context.Context, p muxcore.ProjectContext, req []byte) ([]byte, error) {
    // legacy fallback — used only by sessions that bypassed the
    // *WithSessionMeta dispatch path
    return nil, fmt.Errorf("legacy dispatch not supported")
}

func (h *aimuxHandler) HandleRequestWithSessionMeta(
    ctx context.Context,
    p muxcore.ProjectContext,
    meta muxcore.SessionMeta,
    req []byte,
) ([]byte, error) {
    // meta.Conn.PeerPid / meta.Conn.PeerUid / meta.Conn.Platform
    // meta.TenantID — if AuthorizeSession was wired
    return doWork(ctx, p, meta, req)
}

eng, err := engine.New(engine.Config{
    Name:           "aimux",
    SessionHandler: &aimuxHandler{},

    // Pre-dispatch admission gate — runs once per session.
    AuthorizeSession: func(ctx context.Context, conn muxcore.ConnInfo, p muxcore.ProjectContext) muxcore.SessionAuth {
        tenant, ok := lookupTenant(conn.PeerPid)
        if !ok {
            return muxcore.SessionAuth{Decision: muxcore.AuthDeny, Reason: "tenant_not_enrolled"}
        }
        return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: tenant}
    },

    // Per-frame admission hook — runs on every inbound request/notification.
    // Sync on reader goroutine, 1ms budget. Heavy work belongs elsewhere.
    OnFrameReceived: func(sessionID string, frameSize int, method string) muxcore.FrameAction {
        if rateLimiter.Allow(sessionID) {
            return muxcore.FramePass
        }
        return muxcore.FrameError // -32004 to client; no dispatch
    },

    Logger: logger,
})
if err != nil {
    return err
}
```

**Cross-platform peer-credential extraction:**

| Platform | PeerPid source | PeerUid source |
|----------|----------------|----------------|
| Linux | `SO_PEERCRED` ucred (existing) | `SO_PEERCRED` ucred (shared `readPeerUcred` helper — one syscall returns Pid+Uid+Gid) |
| Windows (named pipe) | `GetNamedPipeClientProcessId(Fd())` via `interface{ Fd() uintptr }` assertion — winio's `*win32File` already exposes Fd publicly via embedding (no fork required) | always 0 (Windows has no UID concept comparable to Unix) |
| Darwin | `getsockopt(SOL_LOCAL, LOCAL_PEERPID)` | `GetsockoptXucred(SOL_LOCAL, LOCAL_PEERCRED)` — kernel xucred struct (Getpeereid is not exposed by `x/sys/unix`) |

**Tests landed in this release:**
- Phase 1 (`G001`): `TestPeerCreds_LoopbackPID_Linux`, `TestPeerCreds_LoopbackPID_Windows`, `TestPeerCreds_LoopbackPID_Darwin`, `TestDispatch_WithSessionMetaPreferredOverLegacy`, `TestDispatch_LegacyHandlerByteIdentical_v23`
- Phase 2 (`G002`): `TestAuthorize_DenyClosesConnection`, `TestAuthorize_AllowPopulatesSessionMeta`, `TestAuthorize_NilConfigUnchangedDispatch`, `TestAuthorize_PanicTreatedAsDeny`, `TestAuthorize_AllowEmptyTenantIdLegitimate`, `TestAuthorize_DenyNoUpstreamSpawn`
- Phase 3 (`G003`): `TestFrameHook_PassDispatchesNormally`, `TestFrameHook_DropSilentDiscard`, `TestFrameHook_ErrorRespondsWithJSONRPC`, `TestFrameHook_TimeoutGracefulDegradation`, `TestFrameHook_PanicTreatedAsPass`, `TestFrameHook_NilUnchangedDispatch`, `TestFrameHook_OutboundFramesNotIntercepted`, `TestFrameHook_OverheadP99`, `BenchmarkFrameHook_NoopOverhead` (804 ns/op — well under spec's 100µs budget)

**Cross-version coexistence:** v0.24.0 daemon coexists with pre-v0.24 consumers. nil-default for every new field preserves the v0.23 dispatch path byte-identically. v0.24 consumers can opt in incrementally — adopt `*WithSessionMeta` interfaces first, then `AuthorizeSession`, then `OnFrameReceived` independently as policy needs arise.

---

### v0.23.0 — engine.Config.OnInject for fire-and-forget IPC frame injection (#107)

**No breaking changes.** `OnInject == nil` is the zero value and preserves
pre-v0.23 behavior for non-adopting consumers.

| API | Semantics |
|-----|-----------|
| `engine.Config.OnInject` | Additive passthrough callback. Called once with `inject func([]byte) error` after the initial shim→owner handshake completes. |
| `owner.ResilientClientConfig.OnInject` | Real wiring point in the resilient shim. Injected frames enter the existing `msgFromCC` path and preserve FIFO ordering with normal CC traffic. |
| `owner.ErrInjectFull` | Sentinel returned when the `msgFromCC` buffer is saturated. Non-blocking backpressure signal; callers decide drop vs retry. |
| `owner.ErrInjectClosed` | Sentinel returned after the resilient proxy exits. Further inject attempts become lifecycle-aware no-ops. |

**Migration:** Existing consumers (aimux ≤v0.22.0, engram, mcp-launcher) require zero source change. `OnInject == nil` is the zero value.

**Migration for aimux:**

```go
eng, err := engine.New(engine.Config{
    Name: "aimux",
    Command: os.Args[0],
    Args: os.Args[1:],
    OnInject: func(inject func([]byte) error) {
        sink.SetSendFunc(func(frame []byte) error {
            // inject returns nil on success, owner.ErrInjectFull under
            // backpressure, or owner.ErrInjectClosed after the proxy exits.
            // Caller decides drop vs retry vs stop sending; we surface as-is.
            return inject(frame)
        })
    },
    Logger: logger,
})
if err != nil {
    return err
}
```

**Tests landed in this release:**
- `TestResilientClient_OnInject_DeliversFrames`
- `TestResilientClient_OnInject_BufferFull`

### v0.22.0 — multi-tenant FS isolation + Persistent propagation (#102, #103)

**BREAKING CHANGES.** Engine name now scopes the FS namespace for owner sockets.
Two muxcore consumers on one host (e.g. mcp-mux + aimux) no longer share the
literal `"mcp-mux-"` prefix and cannot interfere with each other.

Historical note: this section describes the v0.22 migration point. The current
consumer contract is the "v0.26.6 - auto-managed engine namespace" section above:
`engine.Config.Name` is a display label, and ordinary consumers should leave
`engine.Config.Namespace` empty.

| Change | Migration |
|--------|-----------|
| `serverid.IPCPath(id, baseDir)` → `serverid.IPCPath(baseDir, name, id)` | Pass engine name as the second argument. Format becomes `{baseDir}/{name}-{id}.sock`. |
| `serverid.ControlPath(id, baseDir)` → `serverid.ControlPath(baseDir, name, id)` | Same — argument-order change + new name parameter. |
| `serverid.LockPath(id, baseDir)` → `serverid.LockPath(baseDir, name, id)` | Same. |
| v0.22-only `engine.Config.Name` requirement | At that release point, `engine.New(Config{Name: "..."})` rejected empty Name. Current muxcore derives Name and Namespace automatically for `engine.New`. |
| `daemon.Config.Name string` (additive) | The engine layer now passes `cfg.Name` through to `daemon.Config.Name`. Internal callers are updated; library consumers that construct `daemon.Config` directly must set `Name`. |
| `daemon.Config.Persistent bool` (additive) | The engine layer now passes `cfg.Persistent` through. Closes #103: SessionHandler-topology owners now hydrate `OwnerEntry.Persistent` from this field. Subprocess topology continues to derive Persistent from the upstream's `x-mux.persistent` capability — both paths converge on `OwnerEntry.Persistent` before the reaper reads it. |
| `daemon.HandleListOwners` control RPC (new) | New `Cmd: "list_owners"` returns `ListOwnersResponse{Owners []OwnerInfo, Truncated bool}`. mcp-mux's `mux_list/mux_stop/mux_restart` now query this RPC instead of FS-scanning TEMP — closes #102. |
| `cleanStaleSockets` is now name-scoped | A daemon with `Name="aimux"` no longer touches `mcp-mux-*` sockets in TEMP. Foreign-prefix sockets are left alone; only own-prefix unreachable sockets are cleaned. |

**Migration for aimux:**

```go
eng, err := engine.New(engine.Config{
    Name:           "aimux",  // REQUIRED — empty returns error in v0.22.0
    Persistent:     true,     // now actually propagates to OwnerEntry (#103 fix)
    SessionHandler: srv.SessionHandler(),
    // Command/Args optional — SessionHandler topology runs in-process
})
```

After upgrade, `mcp-launcher persist` regression should report PASS where it
previously FAILed.

**Migration for engram:**

```go
eng, err := engine.New(engine.Config{
    Name: "engram",  // REQUIRED in v0.22.0
    // ...rest unchanged
})
```

**Cross-version coexistence:** v0.22 mcp-mux daemon coexists cleanly on the
same host with a v0.21 aimux. The v0.22 daemon's `cleanStaleSockets` only
matches own-prefix sockets, so the v0.21 aimux's `mcp-mux-*` files remain
intact. Operator-side, `mux_list` from the v0.22 mcp-mux no longer sees those
files — it asks the daemon, not the filesystem.

**Migration is non-incremental.** v0.21.x consumers cannot consume
muxcore/v0.22.0 without updating call sites for the three `serverid` functions
and adding `Name` to `engine.Config`. Pin via Go modules until ready.

**Tests landed in this release:**
- `TestNewRejectsEmptyName` (engine) — empty Name diagnostic
- `TestPersistentPropagatesToDaemonConfig` (engine) — propagation
- `TestSessionHandlerOwnerInheritsPersistent` (daemon) — R1 from #103
- `TestReaperRespectsConfigPersistent` (daemon) — R2 from #103
- `TestHandleListOwners` + `TestHandleListOwners_Truncated` (daemon) — RPC shape + 200-cap
- `TestCleanStaleSocketsNameScope` (daemon) — foreign-prefix preservation
- `TestCrossEngineIsolation` (mcpserver integration) — end-to-end FS partition proof
- `TestMuxStopRefusesForeignID` + `TestMuxRestartRefusesForeignID` (mcpserver) — operator footgun closed

**R3 mcp-launcher persist regression** is in-house tooling (`D:\Dev\mcp-launcher`)
— consumer-side verification step run during T019/T021 deployment phases.

---

### v0.21.10 — control flush before afterFn

v0.21.10 fixes conn flush before shutdown — explicit `conn.Close()` before
`afterFn()` ensures response reaches the caller. v0.21.9's `afterFn`
callback was necessary but insufficient: `defer conn.Close()` never ran
because Shutdown completed instantly post-handoff, killing the goroutine.

v0.21.9 introduced the `afterResponse func()` callback pattern.
**Breaking:** `DaemonHandler.HandleGracefulRestart` signature gains a
third return value `afterResponse func()`.

v0.21.8 fixes control socket conflict during graceful restart handoff.
`daemon.New()` bound the control socket before `loadSnapshot`/`tryHandoffReceive`
— predecessor still held it → successor failed → handoff never reached.
Fix: in handoff mode (env vars present), `loadSnapshot`/`tryHandoffReceive`
run first, then `retryControlBind` polls until predecessor releases the
socket. Non-handoff path unchanged. No breaking API changes.

v0.21.7 fixes `spawnSuccessor` hardcoding `"--daemon"` instead of using
`engine.Config.DaemonFlag` (default `"--muxcore-daemon"`). Both v0.21.7
and v0.21.8 are required for graceful restart to work end-to-end.

v0.20.4 is the multi-user hardening release on top of v0.20.3. Closes the
2 HIGH security findings from the 2026-04-18 PRC audit (S8-001 token
handshake enforcement, S5-001 Unix socket 0600 permissions). Ships FR-28
and FR-29 from the post-audit-remediation spec amendment. No breaking API
changes — two additive exports (`muxcore.ListenUnix`, `SessionManager.IsPreRegistered`).

For historical v0.19.3 concurrency fixes, see [muxcore/v0.19.3 release notes](https://github.com/thebtf/mcp-mux/releases/tag/muxcore%2Fv0.19.3).

| Fix | Where | Severity |
|-----|-------|----------|
| Owner Serve loop CPU spin after failed background spawn (BUG-001) | `owner/owner.go` | P1 |
| `ownerNotifier.Notify` held RLock across 30s write deadline (BUG-002) | `owner/owner.go` | P1 |
| JSON escape regression in `dispatchToSessionHandler` — invalid JSON on Windows paths/quotes (H1) | `owner/owner.go` | P1 |
| `daemon.cleanupDeadOwner` TOCTOU identity gap (BUG-003) | `daemon/daemon.go` | P1 |
| `control.Server handleConn` no read/write deadlines — silent-client DoS (BUG-004) | `control/server.go` | P1 |
| `daemon.Spawn` recursion on stuck placeholder + findSharedOwner argv collision | `daemon/daemon.go` | P2 |
| `drainOrphanedInflight` silent stdout write failures — CC hang with no log | `owner/resilient_client.go` | P2 |
| `findSharedOwner` lock-semantics doc ambiguity + stale-iterator hazard | `daemon/daemon.go` | P2 |

Every fix ships with at least one regression test. Full release notes:
https://github.com/thebtf/mcp-mux/releases/tag/muxcore/v0.19.3

v0.19.2 is a bug fix release on top of v0.19.1 — fixes a recursive goroutine
leak in `daemon.Spawn` when a concurrent placeholder wait times out. No API
changes, zero consumer code modifications required.

v0.19.1 was a refactor-only release on top of v0.19.0 — zero behaviour change.
Adds `engine.Config.SkipSnapshot` as an opt-in field (zero-value preserves the
prior hardcoded default). All v0.19.0 migration notes below still apply.

### Upgrade from v0.18.x

#### Breaking changes

| Change | Migration |
|--------|-----------|
| `DaemonControlPath(baseDir)` → `DaemonControlPath(baseDir, name)` | Add engine name as second arg. Empty string = "mcp-mux" (backward compat). engine.Config.Name handles this automatically. |
| `DaemonLockPath(baseDir)` → `DaemonLockPath(baseDir, name)` | Same as above. |

#### New: SessionHandler (replaces Handler for multi-session awareness)

```go
import muxcore "github.com/thebtf/mcp-mux/muxcore"

type MyHandler struct{}

func (h *MyHandler) HandleRequest(ctx context.Context, p muxcore.ProjectContext, req []byte) ([]byte, error) {
    // p.ID  — deterministic project ID (from worktree root hash)
    // p.Cwd — CC session working directory
    // p.Env — per-session env diff (API keys, config)
    return myServer.Handle(req)
}

// Optional: lifecycle hooks
func (h *MyHandler) OnProjectConnect(p muxcore.ProjectContext)    { /* new CC session */ }
func (h *MyHandler) OnProjectDisconnect(projectID string)         { /* CC session left */ }

// Optional: targeted notifications
func (h *MyHandler) SetNotifier(n muxcore.Notifier) { h.notifier = n }
// Then: h.notifier.Notify(projectID, notification) — to one session
//       h.notifier.Broadcast(notification)          — to all sessions

// Optional: client notification handling
func (h *MyHandler) HandleNotification(ctx context.Context, p muxcore.ProjectContext, data []byte) {
    // receives notifications/cancelled etc from CC
}

// Wire it:
e, _ := engine.New(engine.Config{
    Name:           "aimux",
    SessionHandler: &MyHandler{},
})
e.Run(ctx)
```

Legacy `Handler func(ctx, stdin, stdout)` still works. If both set, SessionHandler wins.

#### New: upgrade.Swap (binary update for engine consumers)

```go
import "github.com/thebtf/mcp-mux/muxcore/upgrade"

// Atomic binary swap: current → .old.{pid}, new → current
oldPath, err := upgrade.Swap(currentExe, newExe)

// Clean stale binaries (.old.*, .bak, ~~)
cleaned := upgrade.CleanStale(exePath)
```

For current consumers, prefer `engine.ApplyUpdateAndRestart` for the full live
update flow. `upgrade.Swap` remains the low-level atomic file rename primitive;
by itself it does not signal, wait for, or restart a daemon.

#### Bug fixes included in v0.18.0–v0.19.0

| Fix | Issue |
|-----|-------|
| Full env passthrough — remove diffEnv, pass complete session env | #50 |
| Stale daemon socket cleanup for engine consumers | #48 |
| Daemon CPU spin (60-80%) when owner shutdown with nil upstream | #46 |
| Crash circuit breaker (5 crashes/60s → spawn rejected) | #43 |
| CWD-aware dedup (no cross-project context leaks) | #42 |
| Per-engine daemon sockets (no collision between mcp-mux + aimux) | #43 |
| `_meta` injection for in-process handlers | #44 |
| SpawnUpstreamBackground HandlerFunc fix | #41 |

### For aimux

```bash
cd aimux && go get github.com/thebtf/mcp-mux/muxcore@v0.26.9
```

Key changes to adopt:
1. **SessionHandler** — replace `srv.StdioHandler()` with SessionHandler implementation to get per-CC-session ProjectContext
2. **Native update topology** — use `eng.RestartWithSuccessor(...)` when aimux keeps a stable launcher plus versioned engine store; use `eng.ApplyUpdateAndRestart(...)` only when `CurrentExe` is the replaceable engine path. Do not call `upgrade.Swap` as the whole update protocol.
3. **Native process management** — agents should use aimux's own `sessions`, `status`, and `upgrade` surfaces for aimux state. Default `mcp-mux mux_list` is scoped to the `mcp-mux` daemon namespace. When aimux consumes a muxcore version with opt-in daemon registry support, it may advertise via `engine.Config.Registry`; then `mux_engines` can show aimux and `mux_list(engine_name="aimux")` can list aimux owners read-only after status verification. This does not imply cross-engine stop/restart/update.
4. **Transparent subprocess respawn** — included automatically for pipe-backed owners. Do not duplicate it in aimux with direct stdio smokes, ad hoc binary names, or local retry loops; validate that the same open session survives an upstream crash/update and that the next call reaches the replacement upstream.
5. **v0.19.3 concurrency fixes** — included automatically, no code changes needed. The CPU-spin-on-failed-background-spawn (BUG-001) and ownerNotifier.Notify RLock hold (BUG-002) are both hidden behind existing aimux code paths and required no consumer-side changes.
6. **Circuit breaker** — included automatically, protects against upstream crash loops

### For engram

```bash
cd engram && go get github.com/thebtf/mcp-mux/muxcore@v0.26.9
```

Key changes:
1. **DaemonControlPath** — if you call it directly, add name parameter: `DaemonControlPath(baseDir, "engram")`
2. **v0.19.3 concurrency fixes** — included automatically
3. **SessionHandler** — optional. engram can stay on legacy `Handler` until multi-session support is needed
4. **Opt-in daemon registry** — optional read-only visibility. If engram adopts `engine.Config.Registry`, `mcp-mux serve` can show it through `mux_engines` and scoped `mux_list(engine_name="engram")`; default `mux_list` remains the `mcp-mux` namespace and engram's own management surface remains authoritative.
5. **Transparent subprocess respawn** — included automatically for pipe-backed owners. Remove or avoid product-local retry/reconnect workarounds for ordinary upstream process replacement; the provider acceptance is same-session crash error plus next-call generation replacement.

### v0.21.10 — Flush conn before afterFn (#99)

- v0.21.9 `afterFn` ran after `writeResponse` but `defer conn.Close()` never executed — post-handoff Shutdown completes instantly (0 owners), process exits, goroutine killed, kernel send buffer lost.
- **Fix:** explicit `conn.Close()` before `afterFn()` in `handleConn`. 1 line.
- **No breaking changes.** No API changes.

### v0.21.9 — Defer shutdown until after response write (#99)

- `HandleGracefulRestart` called `go d.Shutdown()` before the control handler wrote the response. On Windows AF_UNIX, data in the kernel send buffer is lost on process exit → caller sees `i/o timeout` instead of confirmation.
- **Fix:** `HandleGracefulRestart` returns `afterResponse func()` callback. Control server writes response first, then invokes callback.
- **Breaking:** `DaemonHandler.HandleGracefulRestart` signature: `(int) (string, func(), error)`. Consumers must update implementations and mocks.
- **Combined with v0.21.7 + v0.21.8:** all three fixes required for end-to-end graceful restart with confirmed response.

### v0.21.8 — Defer control socket binding in handoff mode (#99)

- `daemon.New()` bound the control socket **before** `loadSnapshot`/`tryHandoffReceive`. Predecessor still holds socket → successor fails → handoff never reached → 30s fallback.
- **Fix:** in handoff mode (env vars present), `loadSnapshot`/`tryHandoffReceive` run first, then `retryControlBind` polls (500ms × 60 = 30s max) until predecessor releases the socket via `Shutdown()`. Non-handoff path unchanged.
- **No breaking changes.** Internal constructor reordering only.
- **Combined with v0.21.7:** both fixes required for graceful restart to work end-to-end (v0.21.7 = DaemonFlag, v0.21.8 = socket ordering).

### v0.21.7 — Fix spawnSuccessor DaemonFlag hardcode (#99)

- `spawnSuccessor` hardcoded `"--daemon"` as the successor CLI flag. `engine.isDaemonMode()` checks `cfg.DaemonFlag` (default `"--muxcore-daemon"`). Mismatch caused successor to enter client mode — `tryHandoffReceive` never ran — graceful restart always fell back to kill-and-respawn after 30s.
- **Fix:** `daemon.Config` gains `DaemonFlag string` field. `engine.runDaemon` passes `e.cfg.DaemonFlag`. `spawnSuccessor` uses configured flag. Empty value defaults to `"--daemon"` for backward compat.
- **No breaking changes.** Additive field only.

### v0.21.1 — Shim reconnect token refresh (F2)

- **`HandleRefreshSessionToken(prevToken string) (newToken string, err error)`** added to `control.DaemonHandler` — lets a shim mint a fresh handshake token for the same owner after its original token is consumed, without triggering a full owner respawn.
- **`session.Manager` bound history** — `Bind` now records a 30-min TTL entry keyed by token; `RegisterReconnect(prev, ownerAlive)` looks up that history and returns a new pending token, or `ErrUnknownToken` / `ErrOwnerGone`.
- **`ResilientClientConfig.RefreshToken` + `MaxRefreshAttempts`** (default 3) — shim tries `HandleRefreshSessionToken` up to N times before falling back to the full `HandleSpawn` path. Fallback also fires immediately on `ErrOwnerGone`. Zero-value `RefreshToken` field preserves pre-F2 behaviour (skip refresh entirely).
- **Structured markers and counters** — `shim.reconnect.refresh_ok|refresh_fail|fallback_spawn` log markers; `shim_reconnect_refreshed`, `shim_reconnect_fallback_spawned`, `shim_reconnect_gave_up` counters exposed via `HandleStatus`. Note: `fallback_spawned` increments only when `HandleSpawn` is called with `ReconnectReason == "fallback_spawn"` (v0.21.x shims only); legacy shims that call bare `HandleSpawn` are invisible to all three counters.
- **Back-compat** — pre-v0.21.1 shims that have no knowledge of `refresh-token` are still rejected cleanly after their token is consumed; they recover via the existing `HandleSpawn` path, identical to a cold first-time spawn from the daemon's perspective.
- **Breaking change (internal API)** — `session.Manager.Bind` signature extended from `Bind(token string, session *Session)` to `Bind(token, ownerKey string, session *Session)`. Only call site is `owner.acceptLoop`; external consumers of muxcore do not call `Bind` directly.

### v0.21.2 — Engine accessors

- Adds read-only accessors to the `engine.Engine` type for observability and testing: `OwnerCount()`, `SessionCount()`, `HandleStatus()`, and `Entry(serverID)`. No breaking changes; all additive.

### v0.21.4 — Defensive ipc.Listen + upgrade --restart split-state fix

- **`ipc.Listen` now refuses to rebind an actively-served path.** Before removing the stale socket file and calling `sockperm.Listen`, `Listen` calls `IsAvailable`. If a live listener is detected, it returns `"ipc: listener already active at <path> (another process is serving)"`. Callers that previously relied on silent socket-steal semantics will now receive a loud error instead. This is a breaking behavioural change, but such reliance was always a bug — a second `Listen` on an active path would have silently disconnected all existing clients. The guard turns that into a diagnosable failure; the caller (snapshot restore, owner startup) can log and skip the conflicting owner rather than corrupting it.

- **`upgrade --restart` fallback-shutdown branch now waits for old daemon exit.** Before spawning the new daemon, `runUpgrade` mirrors the graceful-restart branch's 20×500 ms `isDaemonRunning` poll into both fallback sub-branches (`graceful-restart not available` and `graceful-restart failed`). Prevents the race where the new daemon rebinds per-owner sockets via `ipc.Listen` while the old daemon's Owner structs and `sessionMgr` are still live, causing shim handshake tokens registered in the new daemon to be routed to the old daemon's accept loop and rejected.

- **No API changes.** Both fixes are additive or strictly defensive. `ipc.Listen` signature is unchanged; callers that passed a stale-file path continue to work. The poll loop in `runUpgrade` is internal to the binary.

- **Bundled release:** muxcore/v0.21.4 + v0.21.4 binary.

- **Investigation:** `.agent/debug/upgrade-restart-split-state/investigation.md`

### v0.21.3 — OwnerConfig.UpstreamWriter (proposed in PR #93)

- `owner.OwnerConfig.UpstreamWriter io.Writer` — optional field that, when set, replaces the default subprocess stdout pipe with a caller-supplied writer. Enables in-process upstream implementations that do not want to go through a subprocess. PR #93 is open at time of writing; adopt after merge.


