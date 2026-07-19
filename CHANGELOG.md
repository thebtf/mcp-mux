# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/thebtf/mcp-mux/compare/v0.29.0...HEAD
[0.29.0]: https://github.com/thebtf/mcp-mux/compare/v0.28.0...v0.29.0
[0.28.0]: https://github.com/thebtf/mcp-mux/compare/v0.27.2...v0.28.0
[0.27.2]: https://github.com/thebtf/mcp-mux/compare/v0.27.1...v0.27.2
[0.27.1]: https://github.com/thebtf/mcp-mux/compare/v0.27.0...v0.27.1
[0.27.0]: https://github.com/thebtf/mcp-mux/compare/v0.26.13...v0.27.0
