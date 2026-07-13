# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Fixed request-loss ambiguity during reconnect: already-sent requests receive
  an explicit JSON-RPC error with their original ID, while only the cached
  `initialize` handshake is replayed.
- Fixed partial handoff adoption and handle-cleanup paths so uncommitted process
  authorities are aborted and closed.

### Documentation

- Documented lifecycle defaults and environment overrides, persistence policy,
  v1-to-v2 compatibility, rollback behavior, forbidden local workarounds, and
  the distinction between Serena dashboard configuration and process cleanup.

[Unreleased]: https://github.com/thebtf/mcp-mux/compare/v0.27.0...HEAD
[0.27.0]: https://github.com/thebtf/mcp-mux/compare/v0.26.13...v0.27.0
