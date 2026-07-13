# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Private dormant frames now require a PID-bound direct-launcher capability plus
  active-engine proof. Old launchers stay fail-closed; verified active children
  may bootstrap the stable launcher for future invocations.
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

[Unreleased]: https://github.com/thebtf/mcp-mux/compare/v0.27.1...HEAD
[0.27.1]: https://github.com/thebtf/mcp-mux/compare/v0.27.0...v0.27.1
[0.27.0]: https://github.com/thebtf/mcp-mux/compare/v0.26.13...v0.27.0
