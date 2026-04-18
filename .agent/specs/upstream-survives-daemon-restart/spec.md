---
feature_id: ~
title: "Upstream survives daemon restart — FD passing / process reparenting"
slug: upstream-survives-daemon-restart
status: Draft
created: 2026-04-19
modified: 2026-04-19
parent: ~
children: []
supersedes: ~
superseded_by: ~
split_from: ~
merges: []
aliases: [upstream-restart-retry-semantics]
open_crs: [CR-001]
---

# Feature: Upstream survives daemon restart — FD passing / process reparenting

**Slug:** upstream-survives-daemon-restart
**Created:** 2026-04-19
**Status:** Draft
**Author:** AI Agent (reviewed by user)

> **Provenance:** Specified by Opus 4.7 on 2026-04-19 via `/nvmd-specify --quick`.
> Evidence from: semantic trace (socraticode + serena) of `muxcore/owner/owner.go:1817-1851`,
> `muxcore/upstream/process.go:320-354`, `cmd/mcp-mux/main.go:504-577`,
> `muxcore/daemon/snapshot.go:175`; CC stdio client source at
> `D:\Dev\_EXTRAS_\claude-code\src\services\mcp\client.ts:1249-1363`; engram issue #109 root cause comment.
> Confidence: VERIFIED.

## Overview

Every daemon-level lifecycle event (`mcp-mux upgrade --restart`, `mux_restart`, idle-timeout
reaper, daemon crash, `mcp-mux stop`) currently kills every upstream MCP process via
SIGTERM+SIGKILL and respawns a fresh one. This violates constitution principles #1 (Transparent
Proxy) and #9 (Atomic Upgrades): a server running through mcp-mux does NOT behave identically to
one running without it — without us the upstream never restarts mid-session. This spec makes
upstream processes persist across planned daemon lifecycle events so inflight requests never
die in a murdered child.

## Context

### What exists today

`Owner.Shutdown()` at `muxcore/owner/owner.go:1817-1851` calls `upstream.Close()`, which at
`muxcore/upstream/process.go:320-354` does: close stdin → 5s drain wait → `GracefulKill`
(SIGTERM → 3s wait → SIGKILL via process-group tree-kill on Unix / TerminateJobObject on
Windows). This runs for every path into `Owner.Shutdown()`:

| Trigger | Entry point |
|---------|-------------|
| `mcp-mux upgrade --restart` | `HandleGracefulRestart` → `d.Shutdown()` → per-owner `Shutdown` |
| `mux_restart <sid>` | `daemon.Remove(sid)` → `Owner.Shutdown()` |
| Idle reaper (10-min default) | `daemon.Remove(sid)` → `Owner.Shutdown()` |
| `mcp-mux stop` | `daemon.Shutdown()` → per-owner `Shutdown` |
| Daemon crash | Child processes orphan — reap via SIGHUP or kernel cleanup |

After shutdown, `muxcore/daemon/snapshot.go:175` (`SpawnUpstreamBackground`) starts a **fresh
upstream process**. Cached `initialize`/`tools`/`prompts`/`resources` responses are restored
from the snapshot for fast shim replay, but every inflight JSON-RPC request sent into the
dead upstream is physically lost. `drainOrphanedInflight` then writes JSON-RPC `-32603
"upstream restarted, request lost during reconnect"` to CC stdout per orphaned request.

### Why this is architectural, not a retry problem

Without mcp-mux, CC's stdio MCP client (`client.ts:1266-1371`) has **no retry logic** for
stdio transports — an `onerror` or rejected pending-callTool promise is surfaced to the user
unchanged. But without mcp-mux, upstream restart never happens: upstream is CC's own child
process, lifetime-bound to the CC session. No daemon, no reaper, no `upgrade --restart`, no
shared-owner churn. The "problem" is entirely introduced by our daemon lifecycle.

Issue #109 asked for structured error + idempotent retry. That is a defense-in-depth
band-aid over a self-inflicted wound. The correct fix is to stop murdering upstream in the
first place.

### Supporting evidence

- `muxcore/owner/owner.go:1817-1851` — `Owner.Shutdown()` unconditionally `up.Close()`s upstream.
- `muxcore/upstream/process.go:320-354` — `Process.Close()` escalates to SIGKILL on timeout.
- `muxcore/daemon/snapshot.go:174-175` — new daemon invokes `SpawnUpstreamBackground()` for every
  restored owner (fresh process, not reattachment).
- `muxcore/owner/resilient_client.go:542-595` — `drainOrphanedInflight` emits
  `-32603 "upstream restarted, request lost during reconnect"` to every pending CC request.
- Constitution #1 (Transparent Proxy), #9 (Atomic Upgrades), #5 (Graceful Degradation).
- Engram #109 with root cause comment authored 2026-04-19.

## Functional Requirements

### FR-1: Upstream processes MUST survive planned daemon restart (Unix + Windows)

When a daemon exits due to `upgrade --restart`, graceful `shutdown`, or a controlled
replacement (new binary spawned by an operator), upstream MCP server processes MUST remain
running. Their stdin/stdout file descriptors MUST be transferred to the successor daemon so
JSON-RPC traffic continues without the upstream observing a disconnect.

**Scope:** ALL upstreams currently registered in the daemon. No subset, no opt-out by default.
Constitution #2 forbids adding configuration for basic operation.

### FR-2: Unix implementation — SCM_RIGHTS FD passing over control socket

On Unix (Linux, macOS, any POSIX), the old daemon MUST transfer upstream file descriptors to
the new daemon via SCM_RIGHTS ancillary messages on a Unix domain socket before exiting. The
successor daemon reattaches to the existing upstream process and resumes IPC proxy without a
fresh `initialize` handshake.

**Pattern reference:** nginx binary upgrade (master process passes listening sockets to new
master via Unix socket).

**Detachment precondition:** upstream child processes MUST be placed in a separate process
group (`setpgid`) at spawn time so SIGHUP cascades and parent-death signals do not kill them
when the daemon exits. Child processes MUST ignore or not be signaled by daemon-shutdown
SIGTERM propagation.

### FR-3: Windows implementation — Job Object BREAKAWAY_OK + DuplicateHandle

On Windows, upstream child processes MUST be spawned with `CREATE_BREAKAWAY_FROM_JOB` or
assigned to a Job Object with `JOB_OBJECT_LIMIT_BREAKAWAY_OK` so they do not terminate when
the daemon's job object is destroyed. Handle transfer MUST use `DuplicateHandle` across the
old/new daemon boundary via a named pipe control channel.

**Existing state:** `muxcore/upstream/process.go` currently uses a Job Object with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` for tree-kill on crash — this must become conditional
on crash semantics (see FR-5).

### FR-4: Reaper and supervisor MUST distinguish planned restart from upstream crash

The daemon's suture supervisor and idle reaper MUST classify upstream termination events:

- **Planned daemon restart** (FR-1 handoff completed) → preserve upstream; new daemon adopts.
- **Upstream crash / OOM / panic** (child exits without handoff) → existing respawn path applies.
- **Operator-requested `mux_stop <sid> --force`** → existing kill path applies.
- **Idle reaper eviction** → NEW behavior: reaper MUST attempt graceful stdin close and give
  upstream a larger drain window (30s default), not a 5s SIGTERM escalation.

The supervisor's `EventServiceTerminate` branch MUST differentiate "parent is exiting for
replacement" from "child died in flight" before recording crash metrics or activating the
circuit breaker.

### FR-5: Crash vs planned restart — observable state machine

Termination causes MUST be logged with explicit class so operators reading daemon logs can
distinguish: `planned_handoff`, `upstream_crash`, `operator_stop`, `idle_eviction`,
`daemon_panic`. Metric counters per class exposed via `mux_list`/`HandleStatus`.

### FR-6: Handoff protocol MUST be versioned

The control message carrying FD references and upstream metadata MUST include a
`handoff_protocol_version` field (integer). The successor daemon MUST reject unknown versions
with a clear error (falls back to FR-8 degraded mode). This protects against
binary-skew during rolling upgrades where old/new mcp-mux binaries disagree on serialization.

### FR-7: Handoff MUST be atomic per upstream

If any single upstream FD transfer fails, that upstream's handoff is ABORTED and falls back
to legacy shutdown-and-respawn (FR-8). Other upstreams' handoffs proceed independently. A
single broken upstream MUST NOT block the entire fleet's transition.

### FR-8: Degraded fallback — legacy shutdown-and-respawn

If FR-2/FR-3 cannot be executed (platform unsupported, FD transfer rejected, control channel
unavailable, successor daemon missing), the old daemon MUST fall back to the current
shutdown → snapshot → respawn path. Inflight requests lose their `-32603` as today. This
degraded mode is a SAFETY NET, not a normal operating mode; an info-log line MUST clearly
state the fallback reason.

**Constitution #5:** Graceful degradation — never fail hard when a degraded mode exists.

### FR-9: Daemon crash (unplanned exit) — upstream survival is best-effort

If the daemon panics or is SIGKILL'd externally, upstream processes MUST remain alive (FR-2
detachment precondition guarantees this on Unix). The NEW daemon started on next shim
reconnect MUST reattach to living upstreams by PID (stored in snapshot + `/proc/{pid}/fd`
inspection on Linux, platform equivalent on macOS, `OpenProcess` on Windows). If reattachment
cannot be safely verified, the daemon falls back to FR-8 for that upstream.

### FR-10: Control socket between old/new daemon MUST be session-independent

The handoff channel (Unix socket or Windows named pipe) MUST be a dedicated lifecycle-control
path, NOT the existing `daemon_control.sock` used by `mcp-mux status` / `spawn` requests. A
crashing handoff MUST NOT corrupt routine control operations.

### FR-11: Successor daemon MUST accept handoff from current daemon only

Handshake on the handoff channel MUST include a cryptographic token the old daemon writes to
a well-known path. Successor daemon presents the token; old daemon verifies before
transferring FDs. Prevents an attacker spawning a rogue daemon mid-upgrade to steal FDs.

## Non-Functional Requirements

### NFR-1: Performance — handoff completes in < 2 seconds for 15 upstreams

Measured: time from old daemon receives `graceful-restart` → last FD delivered to successor
→ successor accepting shim reconnects. 99th-percentile target on reference hardware
(4-core, 16 GB): 2 seconds. Per-upstream overhead MUST scale linearly, not quadratically.

### NFR-2: Platform coverage — Linux + macOS + Windows parity

Feature MUST work on Linux, macOS, and Windows. Platform-specific code isolated behind build
tags (`//go:build linux`, `//go:build darwin`, `//go:build windows`). Constitution #6.

### NFR-3: Backwards compatibility — old shim reconnect path remains functional

Shims running mcp-mux/v0.9.x (pre-v0.10.0) MUST continue to reconnect after upgrade. They
will see the pre-existing `-32603` orphan path (old daemon can't participate in handoff),
but basic connectivity MUST survive. No flag-day upgrade.

### NFR-4: Observability — handoff phase timeline exported via logs + metrics

Each handoff event emits structured log lines with phases:
`handoff_begin`, `fd_serialized`, `fd_transferred`, `upstream_reattached`, `handoff_complete`.
Per-phase duration + total count exposed via `mux_list --verbose` and `HandleStatus`. Needed
for debugging handoff timeouts in production.

### NFR-5: Security — FD receiver validates process identity before accepting

Successor daemon MUST verify the PID received in the control message matches a process
owned by the same UID (Unix) or same logon session (Windows) as itself before issuing
`DuplicateHandle` / fd attachment. Cross-user FD theft via race is prevented.

### NFR-6: Testability — handoff must be reproducible in CI without privileged access

Integration tests MUST simulate old-daemon → new-daemon handoff on Linux runners without
root, via ephemeral user-owned sockets. Windows CI uses localonly named pipes.

### NFR-7: Resource budget — no upstream FD leak if successor handshake fails

If the successor daemon crashes during handoff, the old daemon MUST detect the dead peer
within 5 seconds and fall back to FR-8 shutdown. Leaked FDs on old daemon process are
acceptable since that process is exiting anyway; successor-side leaks MUST NOT occur.

## User Stories

### US1: Upgrade mcp-mux without killing agent work (P0)

**As a** developer running `mcp-mux upgrade --restart` mid-autopilot session,
**I want** the upstream MCP servers to keep running across the daemon restart,
**so that** every inflight tool call completes normally — zero `-32603` errors.

**Acceptance Criteria:**
- [ ] US1-AC1: With 3 parallel CC sessions and 8 upstream servers loaded, running
  `mcp-mux upgrade --restart` produces zero `-32603` errors in any CC session's journal.
- [ ] US1-AC2: The upstream process PID (sampled via `mux_list`) is identical before and
  after `upgrade --restart` for each registered server.
- [ ] US1-AC3: Inflight `tools/call` requests in flight at the moment of `upgrade --restart`
  receive their normal `result` or `error` response from the upstream — no mux-synthesized
  `-32603 upstream restarted`.
- [ ] US1-AC4: A fresh `initialize` handshake is NOT observed in upstream stderr/logs after
  the restart.

### US2: `mux_restart <sid>` is still a hard-restart of that server (P1)

**As an** operator who wants to reset a hung upstream via `mux_restart`,
**I want** `mux_restart` to actually kill and respawn the upstream (current behavior),
**so that** a frozen server can be recovered without mystery.

**Acceptance Criteria:**
- [ ] US2-AC1: `mux_restart <sid>` without flags kills the upstream (PID changes) and
  spawns a fresh one.
- [ ] US2-AC2: FR-1 handoff machinery is NOT invoked for `mux_restart` — it is a deliberate
  kill, not a lifecycle transition.
- [ ] US2-AC3: `mux_restart --graceful <sid>` (new flag) triggers FR-1 handoff within the
  same daemon instance — upstream stays alive, owner replaced in place. **OPEN QUESTION:**
  is `--graceful` useful? See Open Questions.

### US3: Idle reaper becomes a soft-close (P1)

**As a** developer running 2+ CC sessions,
**I want** an upstream whose last session disconnected to stay alive for 10+ minutes
(current grace) AND when reaper finally evicts it, have the upstream receive a polite
stdin-close rather than SIGTERM → SIGKILL,
**so that** upstreams get to flush caches, close files, and exit with code 0.

**Acceptance Criteria:**
- [ ] US3-AC1: Idle reaper eviction closes upstream stdin, waits up to 30s for voluntary
  exit, escalates to SIGTERM only after timeout.
- [ ] US3-AC2: Upstream processes exiting via this path exit with their own chosen exit
  code (not SIGKILL 137).

### US4: Daemon crash does not take down upstreams (P0)

**As an** operator during a daemon panic or external SIGKILL,
**I want** upstream MCP processes to survive and be reattachable by the next daemon,
**so that** a single daemon bug does not invalidate hours of upstream state (e.g. aimux
session registries, cached embeddings).

**Acceptance Criteria:**
- [ ] US4-AC1: `kill -9 <daemon-pid>` leaves all upstream PIDs running.
- [ ] US4-AC2: Next shim reconnect spawns a new daemon which reattaches to living upstreams
  by PID without respawning them.
- [ ] US4-AC3: If reattachment verification (NFR-5) fails for an upstream, that single
  upstream is respawned; others continue normally.

## Edge Cases

- **Rolling upgrade with version skew.** Old binary supports handoff protocol v1, new binary
  requires v2. FR-6: successor rejects unknown version, falls back to FR-8. Logged clearly.
- **Successor daemon crashes mid-handoff.** Old daemon detects dead peer (socket EOF) within
  5s, aborts pending FD transfers, falls back to FR-8 per affected upstream. NFR-7.
- **Upstream process has exited just before handoff begins.** Not transferred. Next daemon
  cold-spawns from snapshot. No error surfaced to CC (no inflight request was affected
  because upstream was already dead).
- **Concurrent shim reconnect during handoff window.** Shim finds old daemon's control
  socket already closed; dials new daemon's control socket; new daemon has not yet
  reattached upstream → shim sees transient connection-refused + retries per existing
  resilient-client backoff. MUST succeed within 3s.
- **Handoff control socket permission denied.** `sockperm.Listen` sets 0600 — same user,
  same permissions. If current process cannot bind (e.g., path collision with a rogue
  daemon) — FR-8 degraded fallback.
- **Upstream holds file descriptor to a file on a removed network mount mid-handoff.** FD
  passed to successor — successor's operations on that FD fail at syscall level as they
  would have on the old daemon. Out of scope for this spec.
- **Upstream was spawned with a now-deleted working directory.** Cwd captured in owner
  snapshot. If successor-daemon environment lacks that cwd (unlikely but possible),
  reattachment succeeds (cwd is already set in the running child — successor does NOT
  re-cd). Subsequent child respawn after a real crash would fail; out of scope.
- **Reaper evicts upstream during handoff window.** Handoff takes ownership — reaper
  observes owner is in `handoff_in_progress` state and defers eviction.
- **Token handshake file inaccessible.** FR-11: successor fails handshake, falls back to
  FR-8. Alarm-level log.

## Out of Scope

- **Inflight request retry semantics** (original issue #109 scope). Retry is a defense-in-depth
  band-aid needed only when handoff fails (FR-8 path). The retry subfeature is planned as a
  follow-on spec (`inflight-idempotent-retry`), NOT this spec. Target muxcore/v0.21.1.
- **Cross-host upstream migration.** Handoff is intra-host only. A new daemon on a different
  machine cannot adopt an upstream via the socket mechanism defined here.
- **Hot-reloading upstream binary.** If the upstream MCP server itself ships a new binary,
  it still requires its own restart (upstream decides). mcp-mux does not orchestrate upstream
  binary upgrades.
- **Serializable mid-flight JSON-RPC request state.** We preserve the process, not a
  request-level transaction log. A SIGKILL'd daemon with in-flight requests written to the
  upstream's socket buffer but not yet read — those bytes stay in the kernel buffer on the
  pipe and are read by the next daemon on reattach. No additional buffering required.
- **Retro-fit onto daemon versions older than v0.21.0.** Old shims still get the FR-8 path.
- **`MCP_MUX_NO_DAEMON=1` legacy mode.** That path never had a daemon to restart; FR-1
  is N/A.

## Dependencies

### Internal

- `muxcore/owner/owner.go` — `Owner.Shutdown()` gains a "handoff mode" that releases
  upstream without closing it.
- `muxcore/upstream/process.go` — new `Detach()` method that returns the underlying FDs + PID
  without issuing SIGTERM.
- `muxcore/daemon/daemon.go` — new handoff phases in `HandleGracefulRestart` and new
  reattachment phase in `loadSnapshot`.
- `muxcore/daemon/snapshot.go` — snapshot format gains `upstream_pid`, `stdin_fd_seq`,
  `stdout_fd_seq` slots (or equivalent named-handle refs on Windows).
- `muxcore/ipc/transport.go` — new `handoff_control.sock` / named pipe path separate from
  existing daemon control.
- `cmd/mcp-mux/main.go` — `upgrade --restart` orchestrates the two-daemon handshake.

### External

- `golang.org/x/sys/unix` — SCM_RIGHTS APIs (already a dependency).
- `golang.org/x/sys/windows` — Job Object and DuplicateHandle APIs.
- Go stdlib `syscall.UnixRights`, `syscall.ParseSocketControlMessage`.

### Existing work to preserve

- `sockperm.Listen` 0600 discipline carries over to handoff socket.
- Token handshake infrastructure (FR-28 in post-audit-remediation spec) reused for FR-11.
- `muxcore/daemon/snapshot.go` format extends (backwards-compat read of older snapshots).

## Success Criteria

- [ ] SC-1: Zero `-32603 upstream restarted` errors observed during normal `mcp-mux upgrade
  --restart` against a live 3-session / 8-server workload across Linux, macOS, Windows.
- [ ] SC-2: Upstream PID constant across `upgrade --restart`, verified by `mux_list` sampled
  before and after.
- [ ] SC-3: Reaper idle eviction no longer produces SIGKILL exit code 137 for polite
  upstreams; most upstreams exit 0.
- [ ] SC-4: Daemon-crash recovery test: `kill -9 <daemon-pid>`, next shim reconnect,
  upstream PIDs unchanged, `tools/call` against reattached upstream succeeds.
- [ ] SC-5: Handoff protocol v1 explicitly rejected by a v2-only successor with a clear log
  line; FR-8 fallback activates without crash.
- [ ] SC-6: Cross-platform integration tests green in CI (Linux amd64/arm64, macOS
  arm64, Windows amd64).
- [ ] SC-7: No regression in existing resilient-client reconnect semantics: existing
  drainOrphanedInflight behavior is preserved for the FR-8 fallback path.

## Open Questions

- **Q1:** [NEEDS CLARIFICATION] Should `mux_restart <sid>` grow a `--graceful` flag
  (US2-AC3) that re-spawns owner while keeping upstream alive, or is graceful restart only
  triggered via daemon-level `upgrade --restart`? **Recommendation:** defer `--graceful` to
  a follow-up; `mux_restart` stays a hard-restart to preserve operator intent clarity.
- **Q2:** [NEEDS CLARIFICATION] Windows Job Object handoff — Windows provides
  `AssignProcessToJobObject` only for processes not already in a terminal job. If the old
  daemon's job object has `KILL_ON_JOB_CLOSE`, children die regardless of `BREAKAWAY_OK`
  on the spawning flag. Must we spawn upstreams in their OWN Job Object from birth (with
  the daemon holding a process-handle reference rather than a job-object reference)?
  **Recommendation:** yes — upstream Job Object model diverges from daemon Job Object.
- **Q3:** [NEEDS CLARIFICATION] FD passing on macOS — launchd interactions. If daemon is
  started as a launchd-managed LaunchAgent, the new-daemon process may be launchd-spawned
  rather than fork-ed from the old daemon. Does SCM_RIGHTS still reach it through a well-known
  control socket? **Recommendation:** yes (tested pattern on nginx+launchd), but CI must cover.

## Rule of Three Override

Not applicable. This is a CREATE, not an extraction.
