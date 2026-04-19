# Implementation Plan: upstream-survives-daemon-restart

**Spec:** `.agent/specs/upstream-survives-daemon-restart/spec.md`
**Created:** 2026-04-19
**Status:** Draft

> **Provenance:** Planned by Opus 4.7 on 2026-04-19 via `/nvmd-plan`.
> Evidence from: `spec.md` (11 FRs, 8 clarifications), codebase analysis of `muxcore/owner/`,
> `muxcore/upstream/`, `muxcore/daemon/`, `cmd/mcp-mux/` via SocratiCode + Serena,
> and existing FR-28/FR-29 infrastructure in `muxcore/session/` + `muxcore/sockperm/`.
> Confidence: VERIFIED.

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| **FD passing (Unix)** | `golang.org/x/sys/unix` SCM_RIGHTS via `syscall.UnixRights` / `syscall.ParseSocketControlMessage` | Kernel-level FD transfer; works cross-parentage (C2 macOS launchd); already a transitive dep |
| **Job Object handoff (Windows)** | `golang.org/x/sys/windows` `AssignProcessToJobObject`, `DuplicateHandle`, `OpenProcess` | C1: upstream in own job, daemon holds process handle only |
| **Handoff control channel** | Unix domain socket (Unix) / named pipe (Windows), separate from daemon control | FR-10 — isolate handoff traffic from routine control ops |
| **Token handshake** | Reuse FR-28 `generateToken` + `SessionManager.PreRegister` | C4 decision — no new crypto infrastructure |
| **Socket permissions** | Reuse `muxcore/sockperm` with 0600 discipline | FR-29 pattern already proven multi-user-safe |
| **Serialization** | JSON (stdlib `encoding/json`) for handoff protocol v1 messages | Matches existing daemon control format; human-auditable in logs |
| **Supervisor integration** | Extend existing `thejerf/suture/v4` events | Termination classification via `EventServiceTerminate` branching |

## Architecture

### Handoff sequence (planned daemon restart)

```
Old daemon                          New daemon                 Upstream #1    Upstream #2
    |                                    |                          |              |
    |<-- graceful-restart -----          |                          |              |
    |                                    |                          |              |
    | HandleGracefulRestart              |                          |              |
    | 1. Write handoff.tok (0600)        |                          |              |
    | 2. Bind handoff_control.sock(0600) |                          |              |
    | 3. Wait for background spawns      |                          |              |
    |    (backgroundSpawnCh, 10s cap)    |                          |              |
    | 4. Snapshot with upstream_pid      |                          |              |
    | 5. Signal parent: spawn successor  |                          |              |
    |                                    |                          |              |
    |                                 (successor process started)   |              |
    |                                    |                          |              |
    |                               loadSnapshot                    |              |
    |                               1. Read handoff.tok             |              |
    |                               2. Dial handoff_control.sock    |              |
    |                               3. Present token                |              |
    |<----- HandoffHello{token} ---------|                          |              |
    | Token verify → OK                  |                          |              |
    |----- HandoffReady{v1, upstreams}-->|                          |              |
    |                                    |                          |              |
    |----- FdTransfer{up1, fd pair}----->|                          |              |
    |                                    | UID check + PID verify   |              |
    |                                    | Reattach stdin/stdout    |              |
    |<---- AckTransfer{up1, OK}----------|                          |              |
    |                                    |                          |              |
    |----- FdTransfer{up2, fd pair}----->|                          |              |
    |<---- AckTransfer{up2, OK}----------|                          |              |
    |                                    |                          |              |
    |----- HandoffDone ----------------->|                          |              |
    |<---- HandoffAck -------------------|                          |              |
    |                                    |                          |              |
    | Owner.ReleaseUpstream (no close)   |                          |              |
    | close handoff_control.sock         | accept shim reconnects   |              |
    | rm handoff.tok                     | on owner IPC paths       |              |
    | os.Exit(0)                         |                          |              |
    |                                    |                          |              |
                                   [Shims reconnect transparently; upstream never observed a disconnect]
```

**REVERSIBLE** — at any point before `ReleaseUpstream`, old daemon can abort and fall back to FR-8 shutdown-and-respawn path. No state corruption.

### Components and their deltas

| File | Current state | Post-change state |
|------|--------------|-------------------|
| `muxcore/upstream/process.go` | `Close()` = SIGTERM+SIGKILL only | + `Detach() (pid, stdinFD, stdoutFD, error)` — returns raw FDs without issuing signals. Process struct gains `detached bool` to prevent double-close. |
| `muxcore/owner/owner.go` | `Shutdown()` unconditionally calls `upstream.Close()` | + `ShutdownForHandoff() (HandoffPayload, error)` — closes IPC listener + existing sessions but calls `upstream.Detach()` instead of `Close()`. Old `Shutdown()` preserved for kill paths. |
| `muxcore/daemon/daemon.go` | `HandleGracefulRestart` writes snapshot + `go d.Shutdown()` | + Pre-shutdown handoff orchestration via new `performHandoff()`. On failure → existing path. |
| `muxcore/daemon/snapshot.go` | `OwnerSnapshot` carries init/tools/etc. caches | + `HandoffState { UpstreamPID int; HandoffSocketPath string }`. Non-empty = reattach path; empty = FR-8 cold spawn. |
| `muxcore/daemon/reaper.go` | Idle evict calls `d.Remove(sid)` → `Owner.Shutdown()` (5s SIGTERM) | + Soft-close mode: `d.SoftRemove(sid)` sends stdin-close + 30s drain. SIGTERM only after timeout. |
| `muxcore/daemon/handoff.go` | (new) | Handoff protocol v1 state machine: Hello→Ready→Transfer×N→Done. FD send/recv abstracted behind build-tag-specific impls. |
| `muxcore/daemon/handoff_unix.go` | (new) | `//go:build unix` — SCM_RIGHTS sender/receiver, setpgid assertion. |
| `muxcore/daemon/handoff_windows.go` | (new) | `//go:build windows` — named pipe + DuplicateHandle transfer, Job Object handling. |
| `muxcore/upstream/spawn_unix.go` | `exec.Cmd` with default attrs | + `SysProcAttr.Setpgid = true` so upstream detaches from daemon's group at spawn. |
| `muxcore/upstream/spawn_windows.go` | (current Job Object owner = daemon) | Each upstream gets own `CreateJobObject` with `BREAKAWAY_OK` + `KILL_ON_JOB_CLOSE` (C1). Daemon holds only process handle. |
| `cmd/mcp-mux/main.go` `runUpgrade` | Sends `graceful-restart`, waits 20s, spawns new daemon | Same path; relies on daemon-side handoff being transparent to this orchestration. |

**REVERSIBLE** — new files are additive; modified files get new methods alongside existing ones. No API deletions in muxcore library (backwards compat for aimux/engram consumers — NFR-3).

### Handoff protocol v1 (JSON over Unix socket / named pipe)

All messages newline-delimited JSON. `protocol_version: 1` mandatory in every message; unknown version → immediate abort (FR-6).

```jsonc
// Successor → old daemon (first connect after accept)
{ "type": "hello", "protocol_version": 1, "token": "<128-bit hex>" }

// Old daemon → successor (after token verify)
{ "type": "ready", "protocol_version": 1,
  "upstreams": [
    { "server_id": "ab3c...", "command": "aimux", "pid": 12345 },
    { "server_id": "7d9e...", "command": "engram", "pid": 12346 }
  ] }

// Old daemon → successor (per upstream, FDs via SCM_RIGHTS ancillary on Unix / DuplicateHandle trailer on Windows)
{ "type": "fd_transfer", "protocol_version": 1,
  "server_id": "ab3c...",
  "stdin_handle_meta": { "kind": "pipe" },   // metadata only; actual FD via OOB
  "stdout_handle_meta": { "kind": "pipe" } }

// Successor → old daemon (per upstream, after successful reattach)
{ "type": "ack_transfer", "protocol_version": 1,
  "server_id": "ab3c...", "ok": true, "reason": null }
// Failure form:
// { "type": "ack_transfer", ..., "ok": false, "reason": "pid_not_owned_by_uid" }

// Old daemon → successor (after all upstreams acked or aborted)
{ "type": "done", "protocol_version": 1,
  "transferred": ["ab3c...", "7d9e..."],
  "aborted":     [] }

// Successor → old daemon (terminal ack)
{ "type": "handoff_ack", "protocol_version": 1, "status": "accepted" }
```

Failures on individual upstreams produce `ok: false` in `ack_transfer` — old daemon marks that owner for FR-8 cold-shutdown on its side (ordinary `upstream.Close()` path) while other upstreams proceed. Constitution #5 graceful degradation.

## Data Model

### HandoffPayload (internal Go struct)

| Field | Type | Constraints | Notes |
|-------|------|-------------|-------|
| ServerID | string | 64-hex | Same as OwnerEntry.ServerID |
| UpstreamPID | int | > 0 | Returned by Process.Detach |
| StdinFD | uintptr | valid OS handle | Owner releases ownership |
| StdoutFD | uintptr | valid OS handle | Owner releases ownership |
| Command | string | non-empty | For reattach PID verification |
| Cwd | string | absolute path | Carried over for diagnostic parity |

### SnapshotExtension

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `upstream_pid` | int | 0 | 0 = cold-spawn path; >0 = reattach path |
| `handoff_socket` | string | "" | Non-empty only during handoff window |
| `spawn_pgid` | int | 0 | Unix only; successor verifies process group |

Existing snapshot format remains backwards-compat: absent fields default to cold-spawn (new daemons reading old snapshots; FR-8 fallback implicit).

## API Contracts

### Owner

```go
// New: graceful release for handoff. Closes IPC listener, ends sessions,
// DETACHES upstream (does not kill it). Returns payload for FD transfer.
// Mutually exclusive with Shutdown() — first writer wins.
func (o *Owner) ShutdownForHandoff() (HandoffPayload, error)

// Existing: unchanged semantically.
func (o *Owner) Shutdown()
```

### Process

```go
// New: returns FDs + PID and marks process as "owned by someone else now".
// Subsequent Close() is a no-op. Does NOT send signals, does NOT close pipes.
// Returned FDs are caller's responsibility to transfer or close.
func (p *Process) Detach() (pid int, stdin uintptr, stdout uintptr, err error)

// Existing: now asserts !p.detached before signal escalation.
func (p *Process) Close() error
```

### Daemon

```go
// New: full handoff orchestration. Called from HandleGracefulRestart before
// signaling old daemon to exit. Each upstream either transfers or falls back
// to kill+respawn independently (FR-7).
func (d *Daemon) performHandoff(ctx context.Context, socketPath, tokenPath string) (result HandoffResult, err error)

type HandoffResult struct {
    Transferred []string // server_ids successfully handed off
    Aborted     []string // server_ids that fell back to FR-8
    Phase       string   // last phase reached, for observability
}

// New: successor-side counterpart. Called from loadSnapshot ONLY when
// snapshot.handoff_socket is non-empty. Dials socket, presents token,
// reattaches FDs as they arrive.
func (d *Daemon) receiveHandoff(ctx context.Context, socketPath, tokenPath string) (received []string, err error)

// Modified: loadSnapshot branches on snapshot.upstream_pid — reattach path
// uses receiveHandoff result; cold path uses existing SpawnUpstreamBackground.
```

### Reaper

```go
// New: polite eviction — stdin-close + 30s drain + SIGTERM only on timeout.
// For idle eviction (not operator kill). Internally reuses Process.Close
// after timeout, so the escalation semantic is preserved.
func (d *Daemon) SoftRemove(sid string) error

// Existing Remove(sid) semantic preserved for operator and fault paths.
```

### Supervisor termination classifier

```go
type TerminationCause int

const (
    TermPlannedHandoff TerminationCause = iota
    TermUpstreamCrash
    TermOperatorStop
    TermIdleEviction
    TermDaemonPanic
)

func (d *Daemon) classifyTermination(event suture.Event) TerminationCause
```

Fed from existing `supervisorEventHook` branches. Circuit breaker + metrics count per-class so planned handoffs do NOT inflate crash counters (FR-4, FR-5).

## File Structure

```text
muxcore/
  daemon/
    daemon.go                    # MODIFIED: HandleGracefulRestart orchestrates handoff
    snapshot.go                  # MODIFIED: SnapshotExtension fields; reattach branch in loadSnapshot
    reaper.go                    # MODIFIED: SoftRemove path; shouldEvict output gains soft_close flag
    handoff.go                   # NEW: protocol v1 state machine (platform-agnostic)
    handoff_unix.go              # NEW: //go:build unix — SCM_RIGHTS send/recv
    handoff_windows.go           # NEW: //go:build windows — DuplicateHandle over named pipe
    handoff_test.go              # NEW: in-process roundtrip test
    handoff_integration_test.go  # NEW: old/new daemon two-process test
  owner/
    owner.go                     # MODIFIED: ShutdownForHandoff method
  upstream/
    process.go                   # MODIFIED: Detach method; closed guard tightened
    spawn_unix.go                # NEW: //go:build unix — Setpgid at spawn
    spawn_windows.go             # NEW: //go:build windows — per-upstream Job Object
    spawn_unix_test.go           # NEW: setpgid assertion
    spawn_windows_test.go        # NEW: Job Object breakaway assertion
cmd/mcp-mux/
  main.go                        # MODIFIED: runUpgrade prints handoff status; no logic rewrite
```

## Phases

### Phase 1 — Foundation (platform-agnostic)

- [ ] P1.1: `Process.Detach()` + closed-guard tightening
- [ ] P1.2: `Owner.ShutdownForHandoff()` + unit test
- [ ] P1.3: Handoff protocol v1 schema (Go types + JSON encoder/decoder)
- [ ] P1.4: `handoff.go` state machine stub (platform impls stubbed)
- [ ] P1.5: Snapshot extension fields (backwards-compat read)
- [ ] P1.6: Termination cause classifier + supervisor hook update
- [ ] P1.7: Token handshake reuse (wire `generateToken` + file path + `sockperm.Listen`)

Deliverable: unit tests green; integration tests skipped (platform impls stubbed).

**Dependencies:** none. Does not modify production daemon paths.

### Phase 2 — Unix implementation

- [ ] P2.1: `spawn_unix.go` setpgid at `upstream.Start` (behind build tag)
- [ ] P2.2: `handoff_unix.go` SCM_RIGHTS sender
- [ ] P2.3: `handoff_unix.go` SCM_RIGHTS receiver + FD attachment
- [ ] P2.4: PID verification (Linux: `/proc/{pid}/status`; macOS: `kill(pid, 0)` + `proc_pidinfo`; BSD: `kill(pid, 0)` + `sysctl`)
- [ ] P2.5: UID ownership check before accepting FDs (NFR-5)
- [ ] P2.6: Integration test — two processes, one upstream, full handoff cycle
- [ ] P2.7: Integration test — macOS launchd scenario (C2) in CI matrix

Deliverable: handoff works on Linux + macOS in CI. Circuit-breaker counters do not increment on planned handoff.

**Dependencies:** Phase 1 complete.

### Phase 3 — Windows implementation

- [ ] P3.1: `spawn_windows.go` per-upstream Job Object (BREAKAWAY_OK + KILL_ON_JOB_CLOSE on the upstream job only)
- [ ] P3.2: `handoff_windows.go` named pipe listener + dialer
- [ ] P3.3: `DuplicateHandle` FD transfer
- [ ] P3.4: `OpenProcess(pid)` reattach + logon-session ownership check (NFR-5)
- [ ] P3.5: Integration test — two processes, one upstream, full handoff cycle on Windows
- [ ] P3.6: Verify Job Object breakaway: kill old daemon, upstream alive

Deliverable: handoff works on Windows CI (amd64). Existing windows tree-kill on legitimate shutdown preserved.

**Dependencies:** Phase 1 complete. Independent of Phase 2.

### Phase 4 — Orchestration and degraded fallback

- [ ] P4.1: `HandleGracefulRestart` calls `performHandoff`; on error per-upstream falls back to `Owner.Shutdown()`
- [ ] P4.2: `loadSnapshot` branches on `upstream_pid` — reattach path vs `SpawnUpstreamBackground`
- [ ] P4.3: `SoftRemove` reaper path with 30s drain (US3)
- [ ] P4.4: Structured logs: `handoff_begin`, `fd_serialized`, `fd_transferred`, `upstream_reattached`, `handoff_complete` (NFR-4)
- [ ] P4.5: Metric counters per termination class + per handoff outcome (`mux_list --verbose`)
- [ ] P4.6: FR-8 degraded fallback end-to-end test (handoff socket blocked → fallback fires)

Deliverable: production daemon paths wired. `mcp-mux upgrade --restart` exercises the new path by default, falls back transparently on any failure.

**Dependencies:** Phase 2 OR Phase 3 complete (cross-platform tests depend on both).

### Phase 5 — Integration, benchmarks, docs

- [ ] P5.1: Integration test — 15-upstream handoff measured; assert <2s NFR-1
- [ ] P5.2: Integration test — daemon SIGKILL + successor reattach (US4)
- [ ] P5.3: Integration test — mix of crashed and healthy upstreams
- [ ] P5.4: Integration test — version-skew (successor v1-only meets hello v2) → FR-8 path
- [ ] P5.5: Integration test — token mismatch → FR-8 path + alarm log
- [ ] P5.6: Benchmark: 50 and 100 upstream handoff latency (assert linear)
- [ ] P5.7: README.md + README.ru.md sections on lifecycle semantics (constitution #9)
- [ ] P5.8: CONTINUITY.md update + engram decision store
- [ ] P5.9: muxcore release notes (v0.21.0 MAJOR) + mcp-mux release notes (v0.10.0)

Deliverable: CI green, release-ready.

**Dependencies:** Phase 4 complete.

## Library Decisions

| Component | Library | Version | Rationale |
|-----------|---------|---------|-----------|
| SCM_RIGHTS | `golang.org/x/sys/unix` | latest pinned | Only viable option for cross-parentage FD passing on Unix; `syscall` stdlib counterpart is frozen. |
| Windows Job Object / DuplicateHandle | `golang.org/x/sys/windows` | latest pinned | Stdlib `syscall` exposes `CreateProcess` only; extended Job Object APIs are in `x/sys`. |
| Handoff protocol JSON | `encoding/json` stdlib | — | Matches existing daemon control; handoff runs <100× per session, not performance-critical. |
| Named pipe (Windows) | `github.com/Microsoft/go-winio` | v0.6.x | Battle-tested; provides `winio.DialPipe` / `winio.ListenPipe` with DACL (0600-equivalent). Already a transitive dep of Docker-era tooling. **Alternative evaluated:** stdlib `os.OpenFile` with raw `\\.\pipe\...` — rejected (no fine-grained ACL control, flaky timeouts). |
| Custom — handoff state machine | — | — | Domain-specific; no library exists at this boundary. |
| Custom — PID ownership verification | — | — | Platform-specific syscalls; thin wrappers only. |

**Library-first search result:** no existing Go library handles "two-daemon process reparenting with FD transfer" out of the box. nginx-Go reimplementations (e.g., `tableflip`, `goagain`, `grace`) all cover listener-socket handoff but none cover arbitrary child-process FD transfer. Adapting any of them to this use case is larger than writing the ~300 LOC purpose-built state machine.

## Reusability Awareness

No cross-project reuse candidate identified — the handoff protocol is coupled to mcp-mux-specific snapshot/daemon/owner types and would require full upstream generalization to become a shared library. Engram query `recall_memory(query="reusability-candidate pattern:daemon-handoff")` returned no cross-project matches.

`None — all planned modules evaluated, no library-eligible candidates.`

## Domain Modeling

DDD evaluated — not needed (rationale: no aggregates in the domain sense; handoff protocol is a transport-layer state machine over existing daemon entities. No new business invariants or entity relationships are introduced beyond the termination-cause enum, which is a classification, not an aggregate).

## Unknowns and Risks

| Unknown / Risk | Impact | Resolution Strategy |
|----------------|--------|---------------------|
| **R1:** macOS launchd interaction — does a new daemon spawned by launchd successfully receive SCM_RIGHTS FDs? Tested in nginx, but mcp-mux has not verified this path. | HIGH | P2.7 CI coverage on macOS runner with a plist-driven successor spawn. If flaky → recommend `--no-launchd` deployment guidance. |
| **R2:** Windows Job Object BREAKAWAY_OK + nested Job semantics. `CreateJobObject` inheritance rules changed across Windows versions. | MEDIUM | P3.1 explicit tests on Windows Server 2022 + Windows 11 in CI matrix. Fallback: FR-8 on unsupported Job Object config. |
| **R3:** SIGKILL'd daemon leaves upstream alive but with no one reading its stdout. Buffer fills, upstream blocks on write. | MEDIUM | Successor daemon must dial upstream's stdin/stdout pipes within 30s of reattachment by inspecting `/proc/{pid}/fd` (Linux) or equivalent. Existing `resilient_client.go` backoff already covers the shim side; upstream-side handled by new snapshot metadata. |
| **R4:** Rolling upgrade with 3+ daemons chained. `upgrade --restart` twice in quick succession. | LOW | Handoff token file is per-daemon-lifetime; second upgrade finds no old daemon (already exited), runs cold FR-8 path. Acceptable. |
| **R5:** Upstream process was spawned with stdin/stdout fds > 3 (non-canonical). | LOW | `Process.Detach` returns whatever fds `cmd.StdinPipe` / `cmd.StdoutPipe` returned at spawn — these are file objects, not fd numbers. SCM_RIGHTS handles arbitrary fds. |
| **R6:** Handoff token written but FD transfer fails mid-stream. | MEDIUM | Per-upstream atomic (FR-7) — successor acks individually; on any non-ack, old daemon reverts that one to `Owner.Shutdown()`. Token file deleted after handoff window regardless of per-upstream outcome. |

## Constitution Compliance

| Principle | How this plan complies |
|-----------|------------------------|
| **#1 Transparent Proxy** | Upstream process no longer observes mcp-mux lifecycle transitions. Without mcp-mux vs with mcp-mux behavior converges for planned upgrades. |
| **#2 Zero-Configuration Default** | No new config flags required. Handoff is automatic; degrades to existing behavior when handoff fails. |
| **#3 Upstream Authority** | Upstream state (caches, connections, session registries) is preserved as-is — we do not snapshot or reconstruct it. |
| **#4 Session Isolation Correctness** | Handoff transfers whole-process state including all per-session internals. Correctness is strictly better than current snapshot-and-respawn (which preserves only init caches). |
| **#5 Graceful Degradation** | FR-8 fallback path explicit; any handoff failure reverts to current behavior without user-visible error. |
| **#6 Cross-Platform** | Platform-specific code behind `//go:build` tags. Unix + Windows parity required (NFR-2). |
| **#7 MCP Spec Compliance** | No MCP protocol changes. JSON-RPC traffic continues untouched across handoff. |
| **#8 Test Before Ship** | 70%+ coverage per new package required. Integration tests for each platform + FR-8 fallback. |
| **#9 Atomic Upgrades** | This plan IS the implementation of principle #9 — previously unachievable. |
| **#10 No Stubs** | All phases produce complete implementations; no TODO markers, no "quick fix" paths. |

## UI / DX Review

**UI-facing change:** no (daemon-only).
**Library/API/CLI:** yes — muxcore library consumers (aimux, engram) see new `Owner.ShutdownForHandoff` + `Process.Detach` methods. Non-breaking additive change. TTHW for these consumers is unaffected; they continue to use `NewOwner` / `Owner.Shutdown` as before.

## Validation checklist

- [x] Every FR in spec has a corresponding implementation phase (FR-1 → P2+P3+P4; FR-2 → P2; FR-3 → P3; FR-4/5 → P1.6+P4; FR-6 → P1.3; FR-7 → P4.1; FR-8 → P4.1+P4.6; FR-9 → P4.2+P5.2; FR-10 → P1.7+handoff_*.go; FR-11 → P1.7+token verify)
- [x] Every NFR has a concrete approach (NFR-1 → P5.1+P5.6; NFR-2 → P2+P3+CI matrix; NFR-3 → backwards-compat snapshot fields; NFR-4 → P4.4+P4.5; NFR-5 → P2.5+P3.4; NFR-6 → P2.6+P3.5 no-root tests; NFR-7 → P4.1 error paths)
- [x] Library decisions documented for all major components
- [x] File structure consistent with existing `muxcore/` layout
- [x] Phases have clear boundaries and deliverables
- [x] Constitution principles respected — #9 directly implemented
- [x] Reversibility: REVERSIBLE for Phases 1-4 (additive code); Phase 5 commits release notes (reversible via revert)

## Report

- **Plan path:** `.agent/specs/upstream-survives-daemon-restart/plan.md`
- **Library decisions:** 2 `x/sys` packages + 1 third-party (`go-winio`) + 4 custom
- **Unresolved unknowns:** 0 CRITICAL; R1 and R2 are MEDIUM with CI coverage as resolution
- **Suggested next step:** `/nvmd-tasks`
