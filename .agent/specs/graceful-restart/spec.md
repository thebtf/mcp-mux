# Feature: Graceful Daemon Restart with State Snapshot Transfer

**Slug:** graceful-restart
**Created:** 2026-04-07
**Status:** Draft
**Author:** AI Agent (reviewed by user)

> **Provenance:** Specified by Claude Opus 4.6 on 2026-04-07.
> Evidence from: investigation report (graceful-daemon-restart), tableflip research,
> codebase analysis (daemon.go, owner.go, resilient_client.go), constitution.md principle 9.
> Confidence: VERIFIED (platform constraints), INFERRED (snapshot format design).

## Overview

When upgrading the mcp-mux binary, the old daemon serializes its runtime state (owner
configurations, cached MCP responses, classification results) to a JSON snapshot file
before shutdown. The new daemon loads this snapshot on startup, creates owners with
pre-populated caches, and re-spawns upstream processes in background. Shims experience
a ~1 second reconnect blip with instant cached replay instead of 5-15 second cold start.

## Context

Current `upgrade --restart` stops the daemon and kills all upstream processes. Shims
auto-reconnect and spawn a new daemon, but every owner starts from scratch: upstream
startup (1-15s), proactive init, tools/list, classification. With 15+ servers, total
recovery takes 30-60 seconds. Users see intermittent "failed" status and lose in-flight
requests.

Constitution principle 9 requires: "Running servers MUST NOT crash during upgrade."
The current approach violates this — all sessions are disrupted.

Platform constraint: FD-passing (tableflip, grace) is Unix-only. Windows (primary
platform) cannot transfer pipe file descriptors between processes. Upstream child
processes must be re-spawned. But cached responses CAN be transferred via file.

## Functional Requirements

### FR-1: State Snapshot Serialization
The daemon must serialize its runtime state to a JSON file when receiving a
graceful-restart command. State includes: owner configurations (command, args, env,
cwd, serverID), cached responses (initialize, tools/list, prompts/list, resources/list,
resources/templates/list), classification results (mode, source, reason), and session
metadata (muxSessionID, cwd, env for each active session).

### FR-2: Snapshot Loading on Daemon Startup
When a new daemon starts and a valid snapshot file exists at the well-known path
(`os.TempDir()/mcp-muxd-snapshot.json`, same directory as daemon control socket),
it must load the snapshot and create owners with pre-populated caches. Owners created
from snapshot must NOT spawn upstream processes immediately — they serve cached
responses while upstream spawns in background.

### FR-3: Graceful Restart Control Command
A new `graceful-restart` command must be added to the daemon control protocol. When
received, the daemon: (a) writes state snapshot, (b) shuts down gracefully (drain
in-flight requests, close IPC listeners). The upgrade CLI must use this command
instead of plain shutdown when `--restart` is specified.

### FR-4: Cache-First Owner Mode
Owners created from snapshot must operate in "cache-first" mode: serve cached responses
to connecting shims immediately, while spawning the upstream process and running proactive
init in background. Once the fresh upstream responds, the cache is updated (in case the
upstream version changed).

### FR-5: Snapshot Cleanup
After successful load, the snapshot file must be deleted (consumed). Stale snapshots
older than 5 minutes must be ignored and deleted (daemon may have crashed between
snapshot write and restart).

### FR-6: Fallback to Cold Start
If no snapshot exists, or the snapshot is invalid/stale, the daemon must fall back to
the current cold-start behavior (proactive init, upstream startup wait). Graceful restart
is an optimization, not a requirement.

## Non-Functional Requirements

### NFR-1: Reconnect Latency (Performance)
Shim reconnect with cached replay must complete in < 2 seconds (from IPC EOF to first
response on stdout). Current cold start: 5-15 seconds.

### NFR-2: Cross-Platform (Portability)
Snapshot mechanism must work identically on Windows, macOS, and Linux. No platform-
specific code beyond existing build tags.

### NFR-3: Snapshot Size (Resource)
Snapshot file must be < 1 MB for typical deployments (15 servers, 60 sessions).
JSON encoding with base64 for binary cached responses.

### NFR-4: Atomic Write (Reliability)
Snapshot must be written atomically (write to temp file, rename) to prevent corruption
if daemon is killed during write.

### NFR-5: Backward Compatibility
New daemon must handle absence of snapshot gracefully (cold start). Old shims must work
with new daemon (no protocol changes to shim-daemon IPC).

## User Stories

### US1: Developer Upgrades mcp-mux (P1)
**As a** developer, **I want** to upgrade mcp-mux without losing MCP server connections,
**so that** my CC sessions continue working with minimal interruption.

**Acceptance Criteria:**
- [ ] `go build -o mcp-mux.exe~ && mcp-mux upgrade --restart` completes in < 5 seconds
- [ ] All MCP servers show "connected" after upgrade without manual `/mcp` reconnect
- [ ] No "failed" status for any server during the upgrade window
- [ ] In-flight tool calls that were pending at upgrade time get error responses (not silent drop)

### US2: Daemon Crash Recovery (P2)
**As a** developer, **I want** the daemon to start normally after a crash (no snapshot),
**so that** graceful restart doesn't introduce new failure modes.

**Acceptance Criteria:**
- [ ] Daemon without snapshot file starts in < 3 seconds (same as current)
- [ ] Stale snapshot (> 5 minutes old) is ignored and deleted
- [ ] Corrupt snapshot (invalid JSON) is ignored with warning log

### US3: Multiple Parallel Sessions (P1)
**As a** developer with 4+ CC sessions, **I want** all sessions to reconnect to the
same shared/session-aware owners after restart, **so that** dedup is preserved.

**Acceptance Criteria:**
- [ ] Shared owners (tavily, socraticode) serve all reconnecting sessions from one upstream
- [ ] Session-aware owners (aimux, pr-review) maintain session-keyed state after reconnect
- [ ] Isolated owners (serena, netcoredbg) get separate instances per session (as before)

## Edge Cases

- Snapshot written but daemon killed before shutdown completes — snapshot consumed by new daemon, old daemon's orphaned upstreams exit naturally (stdin pipe closed)
- Two daemons start simultaneously (race) — daemon lock file prevents double-start; second waits
- Snapshot from incompatible version — include version field, reject mismatched versions
- Owner in snapshot references command that no longer exists — spawn fails gracefully, owner removed
- Session reconnects before upstream is ready — cache-first mode serves snapshot cache, upstream spawns in background

## Out of Scope

- Transferring upstream process file descriptors (impossible on Windows)
- Preserving in-flight request state (requests in progress at restart time are lost)
- Hot reload without any connection interruption (shims will experience IPC EOF)
- Persisting snapshot across reboots (snapshot is ephemeral, restart-only)
- Transferring session IPC connections (shims must reconnect)

## Dependencies

- Existing: proactive init (v0.7.5), resilient_client reconnect, daemon control protocol
- Existing: `upgrade --restart` command (v0.7.6)
- No new external dependencies

## Success Criteria

- [ ] `mcp-mux upgrade --restart` with 15 servers: all reconnect in < 2 seconds
- [ ] Zero "failed" server status in CC after upgrade (verified via mux_list)
- [ ] Cold start (no snapshot) behavior unchanged
- [ ] All existing tests pass + new tests for snapshot serialization/loading
- [ ] Snapshot file < 1 MB for 15-server deployment

## Clarifications

### Session 2026-04-07

| # | Category | Question | Resolution | Date |
|---|----------|----------|------------|------|
| C1 | Data Lifecycle | Should 5-minute staleness threshold be configurable? | Hardcoded constant — rarely used, 5min is generous | 2026-04-07 |
| C2 | Reliability | Should invalid snapshot log a warning or silently fall back? | Log warning — silent failures prohibited by constitution | 2026-04-07 |
| C3 | Terminology | Should "cache-first mode" be formally defined in glossary? | Inline description in FR-4 sufficient — single-use term | 2026-04-07 |

## Open Questions

- None (investigation report + clarifications resolved all questions)
