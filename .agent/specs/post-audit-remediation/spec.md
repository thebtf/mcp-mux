# Feature: Post-Audit Remediation (v0.19.3 Fix Batch)

**Slug:** post-audit-remediation
**Created:** 2026-04-15
**Status:** Implemented
**Author:** AI Agent (reviewed by user)
**Last Amendment:** 2026-04-18 (PRC-2026-04-18: S8-001 token enforcement + S5-001 multi-socket 0600 expansion)
**Shipped:** 2026-04-18 — `muxcore/v0.20.4` + `mcp-mux/v0.9.10` (PR #71, commit `58502d5`)

> **Provenance:** Specified by claude-opus-4-6[1m] on 2026-04-15.
> Evidence from: `.agent/reports/2026-04-15-production-readiness.md` (the audit report this spec directly addresses), `.agent/reports/2026-04-15-bug-hunting-report.md`, `.agent/reports/2026-04-15-security-scan-report.md`, and manual source verification during audit Phase 4 (owner.go:896, daemon.go:435, owner.go:1395–1404, owner.go:1742–1797, owner.go:1824–1837 read directly).
> Confidence: **VERIFIED** for blockers FR-1..FR-5 (all file:line references verified in code this session). **INFERRED** for FR-6..FR-24 (accepted on agent authority during Phase 3 synthesis — spot-check before implementation).

## Overview

Remediate every real finding from the 2026-04-15 production readiness audit that surfaced 30 deduplicated items against mcp-mux v0.19.0 + muxcore/v0.19.2. Ship the fix batch as muxcore/v0.19.3 with regression tests for every bug and documentation updates for env vars. Verdict: NOT READY → READY.

## Context

The 2026-04-15 audit (5 parallel agents, Feynman reality check, deterministic collector) produced WTF-points=92 (threshold 60 overridden because Feynman returned MOSTLY_REAL and production is live with 17 owners). The audit identified:

- **2 P1 Critical** concurrency bugs (CPU spin on failed background spawn, mutex held across 30s-deadline IPC write)
- **1 regression** from Phase 4 SessionHandler (JSON escape in error responses) introduced since the 2026-03-27 READY audit
- **5 P2 High** concurrency / contract bugs (daemon TOCTOU, missing read deadline, recursive Spawn on an untouched parallel path, silent write-error drop, lock-drop mid-iteration)
- **2 High security** items (Unix socket permissions, snapshot tamper) that do not apply to the current Windows single-user deployment but should be fixed for portability
- **12 Medium** items (coverage gaps, missing cleanup, context propagation, a broken skip reference)
- **8 Low** items (dead socket scan latency, env var documentation, test justification comments)

None are currently causing visible damage — they are failure-mode bugs (latent until the right trigger). The top priority is to eliminate the P1 class before v0.19.3 tag, then the High class, then the rest in priority order. Fixes will be grouped into worktree+PR sub-batches to keep each PR reviewable and each commit individually revertable.

The audit verdict was **CONDITIONALLY READY** with the explicit contract: "ship-worthy for the current deployment pattern but with a clear v0.19.3 fix batch (items 1–5 under Blockers) before the next muxcore tag." This spec formalizes that batch and extends it to every warning the audit raised.

## Clarifications

### Session 2026-04-18 (amendment: FR-28 + FR-29)

Five clarifications resolved via `/nvmd-clarify --auto` against the 2026-04-18 amendment section. Recommendations accepted verbatim.

| # | Category | Question | Resolution |
|---|----------|----------|------------|
| C1 | Security | Should the FR-28 rejection log entry include the invalid token string? | **NO.** Log only the peer PID (if available via `SO_PEERCRED` on Unix, nil on Windows) and the socket path. Never log the token value. Rationale: tokens in log files = auth bypass via log-file read; the per-token value has zero diagnostic value (an unknown token is unknown regardless of its bytes). Log format: `accept: rejected connection from pid=%d (invalid/missing token)`. |
| C2 | Data Lifecycle | On FR-28 rejection, is the pre-registered token consumed or preserved? | **PRESERVED.** A rejection does not mutate the pre-registered set; only a successful `Bind` consumes the token. This allows transient-failure retry on the legitimate client path (e.g., client closes socket mid-handshake and reconnects) without forcing re-registration via the control socket. Single-use semantics are enforced by `Bind`, not by rejection. |
| C3 | Domain/API | Is `SessionManager.IsPreRegistered(token string) bool` exported as part of the muxcore public API, or internal-only? | **EXPORTED.** Promote to the muxcore public API for symmetry with existing `PreRegister` + `Bind` exports. Engine consumers (aimux, engram) may want to implement their own acceptance gate patterns and should be able to test pre-registration without calling `Bind` (which mutates state). Zero API cost — the function is trivial and side-effect-free. |
| C4 | UX / Observability | Rate-limit rejection log entries to prevent flooding during a probe storm? | **YES.** Cap at 10 rejection log entries per minute per owner, drop-oldest semantics using a simple time-windowed counter (no external dependency). Above the cap, emit a single "rate-limited: N rejections suppressed in last 60s" summary every minute. Rationale: a malicious local process can generate thousands of rejections per second; unbounded logging is itself a DoS vector on disk space and log-shipping pipelines. |
| C5 | Platform / Documentation | Does the FR-29 Windows no-op stub need an inline comment documenting AF_UNIX / named pipe default ACL behavior? | **YES.** Add a package-level doc comment in `socket_perms_windows.go` citing the verified behavior: "On Windows 10 1803+ AF_UNIX sockets inherit the creating process's default DACL, which grants access only to the owner SID and LocalSystem. Named pipes created via `net.Listen(\"unix\", path)` on Windows follow the same model. No `Umask`-equivalent API is needed." Zero code, only documentation — prevents future reviewer from incorrectly adding a Windows-side permission-tightening hack. |

## Functional Requirements

### Priority 1 (Blockers — must land in v0.19.3)

### FR-1: Eliminate Serve-loop CPU spin on failed background spawn
The `Owner.Serve()` loop must not return a restartable error when `SpawnUpstreamBackground` has failed and `upstream == nil && backgroundSpawnCh == nil && deadCh == closedChan`. The current priority-check pattern added for issue #46 does not engage because `o.done` is not closed. Remediation must guarantee: after one such cycle, either the owner transitions to a clean shutdown (`o.done` closed, next iteration exits via priority check) OR `Serve` returns `suture.ErrDoNotRestart` so the supervisor does not re-enter the loop. A regression test must reproduce a failed background spawn and assert that `Serve` does not return repeatedly within one second.

### FR-2: Release Owner lock before blocking IPC writes in `ownerNotifier.Notify`
`ownerNotifier.Notify` at `muxcore/owner/owner.go:1395–1404` currently holds `n.owner.mu.RLock()` across the full `s.WriteRaw(notification)` call, which can block up to 30 s on a slow IPC consumer. Every goroutine needing `o.mu.Lock()` (removeSession, cacheResponse, progress token cleanup, cwd updates) is stalled for that window. Remediation must copy the target session pointer under RLock, release the lock, then call `WriteRaw` on the local copy — the same pattern already used by the sibling `ownerNotifier.Broadcast`. A regression test must assert that a blocked `WriteRaw` does not prevent concurrent `addSession`/`removeSession` from acquiring `o.mu.Lock()`.

### FR-3: JSON-escape error messages in SessionHandler dispatch response
`muxcore/owner/owner.go:896` in `dispatchToSessionHandler` interpolates `err.Error()` as a bare `%s` into a raw JSON string literal. Error messages containing double quotes, backslashes, newlines, or control characters produce invalid JSON that the CC client cannot parse, causing dropped connections. Remediation must use `json.Marshal(err.Error())` or route through the existing `respondWithError` helper which already constructs its response via `json.Marshal`. A regression test must call `dispatchToSessionHandler` with a handler that returns an error containing every class of problematic character (`"`, `\`, `\n`, `\t`, `\u0000`, Windows path `C:\foo\bar`) and assert the output is valid JSON that round-trips through `json.Unmarshal`.

### FR-4: Add identity guard to `cleanupDeadOwner` delete
`muxcore/daemon/daemon.go` in `cleanupDeadOwner` (around line 291) releases and re-acquires `d.mu` across the cleanup sequence, then unconditionally calls `delete(d.owners, sid)`. A concurrent `Spawn` that replaces the map entry with a new live owner for the same `sid` will be evicted by the unconditional delete. Remediation must guard the delete with an identity check: only delete if the current entry is still the same pointer (or same placeholder) that `cleanupDeadOwner` observed at the start of its critical section. A regression test must simulate the race: inject a dead owner, interleave a fresh `Spawn` for the same `sid`, call `cleanupDeadOwner`, and assert the fresh entry survives.

### FR-5: Enforce read deadline on control server connections
`muxcore/control/server.go` `handleConn` (around line 62) accepts connections and reads requests without setting a read deadline. A client that connects and never sends data holds a goroutine until the connection is forcibly closed. The client side already enforces `clientDeadline = 5s`. Remediation must call `conn.SetReadDeadline(time.Now().Add(clientDeadline))` before the first read and either reset or extend the deadline between subsequent reads on the same connection. A regression test must open a control socket connection, write nothing for longer than `clientDeadline`, and assert the server-side goroutine exits with a deadline-exceeded error.

### Priority 2 (High — target v0.19.3 if time permits, otherwise v0.19.4)

### FR-6: Refactor post-placeholder-wait recovery into explicit retry loop
`muxcore/daemon/daemon.go:435` still contains `return d.Spawn(req)` on the path reached after `case <-creating:` resolves with `entry.Owner == nil || !entry.Owner.IsAccepting()`. Two audit agents (code-reviewer H2, bug-hunter BUG-005) independently flagged this. The recursion is bounded in practice, but the comment block at lines 416–422 explicitly says "Do NOT recurse" — comment and code disagree. Remediation must replace both recursive calls in Spawn (line 435 and line 458 isolated-mode branch) with a labeled retry loop and a `maxRetries=3` counter. On exceeding the retry budget, return a descriptive error. A regression test must exercise the post-timeout recovery path and assert the retry budget is respected.

### FR-7: Preserve error visibility in `drainOrphanedInflight`
`muxcore/owner/resilient_client.go` `drainOrphanedInflight` (around line 564) writes JSON-RPC error responses for in-flight requests to `rc.cfg.Stdout` using `fmt.Fprintf` with the return value discarded. If stdout is broken at reconnect time (the exact scenario this function exists to handle), the orphaned requests are dropped silently. Remediation must capture the `fmt.Fprintf` return and log write failures at warning level, including the number of orphaned requests that failed delivery. A regression test must supply a broken stdout and assert the failure is logged.

### FR-8: Fix `findSharedOwner` lock semantics
`muxcore/daemon/daemon.go` `findSharedOwner` (around line 741) is documented to be called with `d.mu` held but internally unlocks and re-acquires the lock when it encounters a creating placeholder, then continues iterating over the (now stale) `d.owners` snapshot. Remediation must use a two-phase pattern: copy the candidate entries under `d.mu`, release the lock, scan the copy outside the lock, and re-acquire if mutation is needed. Update the function's doc comment to reflect the new contract. A regression test must exercise concurrent map mutation during `findSharedOwner` iteration and assert no stale pointer dereference.

### FR-9: Tighten Unix control socket permissions
`muxcore/ipc/transport.go:25` uses `net.Listen("unix", path)` which creates the socket with mode `0755` under a typical `022` umask. On multi-user Unix hosts any local account can connect to the daemon control socket and issue spawn / shutdown / graceful-restart commands. Remediation must call `os.Chmod(path, 0600)` immediately after `net.Listen` succeeds, or use `syscall.Umask(0077)` around the `Listen` call. On Windows this is a no-op. A regression test (Unix-only, build tag `!windows`) must assert socket permissions are `0600` after Listen.

### FR-10: Harden daemon snapshot against tamper
`muxcore/snapshot/snapshot.go` + `muxcore/daemon/snapshot.go` write the snapshot to `os.TempDir()/mcp-muxd-snapshot.json`. The file stores `Command + Args` which at load time are passed directly to `exec.Command(command, args...)`. An attacker able to write to `/tmp` (multi-user Unix) can substitute arbitrary argv that executes on next daemon restart. Remediation must move the snapshot to `os.UserCacheDir()` with directory and file mode `0700/0600`, and add an HMAC signature using an in-memory runtime key so a tampered or replayed snapshot is rejected at load time. The key must be regenerated per daemon process (i.e., the snapshot does not survive process crashes — consistent with the intent that snapshots are for graceful restart only). A regression test must write a tampered snapshot and assert `loadSnapshot` refuses to use it.

### Priority 3 (Medium — target v0.19.4)

### FR-11: Propagate owner lifecycle to in-process handler goroutines
`muxcore/owner/owner.go` calls `upstream.NewProcessFromHandler(context.Background(), ...)` at lines 360 and 452, passing a non-cancellable context to in-process handlers. Handlers that select on `ctx.Done()` as a shutdown signal do not see the owner's `Shutdown()` call — they only see the pipe close (stdin EOF), which some handlers do not monitor. Remediation must derive the handler context from `o.done` via a small wrapper goroutine: `handlerCtx, handlerCancel := context.WithCancel(context.Background()); go func() { <-o.done; handlerCancel() }()`. A regression test must install a handler that blocks on `ctx.Done()` and assert it unblocks within 100 ms of `owner.Shutdown()`.

### FR-12: Propagate owner lifecycle to notification handler goroutines
`muxcore/owner/owner.go:719` spawns `go nh.HandleNotification(context.Background(), project, msg.Raw)` without any cancellation. A blocking `NotificationHandler` implementation leaks goroutines until the process exits. Remediation must use the same derived-context pattern as FR-11 so notification handlers see owner shutdown. A regression test must assert goroutine count returns to baseline after `owner.Shutdown()` even when a blocking NotificationHandler is installed.

### FR-13: Clean up progress tracker state on session removal
`muxcore/owner/owner.go` `removeSession` at line 1419 deletes entries from `progressOwners`, `progressTokenRequestID`, and `requestToTokens` but does not call `progressTracker.Cleanup()` for the removed tokens. The dedup state persists across session reuse, potentially suppressing synthetic progress for a future session that happens to pick up the same upstream. Remediation must either call `progressTracker.Cleanup(token)` for each removed token inline, or refactor `removeSession` to use the existing `clearProgressTokensForRequest` helper which already does the right thing. A regression test must assert that after `removeSession`, `progressTracker` returns no dedup hits for the removed tokens.

### FR-14: Log write errors in `ownerNotifier.Broadcast`
`muxcore/owner/owner.go:1406` `Broadcast` currently calls `s.WriteRaw(notification)` in a loop and discards every return value. Partial delivery failures are invisible to operators. Remediation must capture each error and log a debug-level line listing the session ID and error text. The function signature stays unchanged (still `func Broadcast([]byte)` — a public-API change to return an error slice is out of scope). A regression test must supply one session with a broken writer and one with a healthy writer, then assert the debug log contains exactly one warning.

### FR-15: Write a real unit test for `Owner.Serve` upstream-exit contract
`muxcore/owner/owner_serve_test.go:170` currently `t.Skip`s `TestOwnerServe_ReturnsErrorOnUpstreamExit` with a reference to `TestSupervisor_ExponentialBackoff`, which does not exist — the closest real test is `TestSupervisorExponentialBackoffOnFailureStorm` in a different package that tests suture mechanics, not the `Serve()` return contract. Remediation must write a real unit test that (a) starts a shared owner with a mock upstream, (b) kills the upstream, (c) asserts `Serve()` returns a non-nil error (not `nil`, not `suture.ErrDoNotRestart`) that triggers supervisor restart. The skip must be removed.

### FR-16: Test the atomic-swap rename-fail partial failure
`muxcore/upgrade/upgrade_test.go:98` skips the rename-back path on atomic-swap failure with a note that "OS-level injection not portable". Remediation must add a test that injects a failure between `rename(current, old)` and `rename(new, current)` using a test seam (e.g., an `osRename` function variable that tests can override) and asserts the rollback restores the original binary. Confidence: Medium — may be deferred if the test seam adds too much production complexity.

### FR-17: Async-dispatch `cleanStaleSockets` on daemon startup
`muxcore/daemon/daemon.go:298` scans `os.TempDir()` synchronously and sends a 5-second-timeout `control.Send` ping to every matching file before the daemon becomes functional. Ten stale sockets from repeated crashes = up to 55 seconds of startup latency. Remediation must either (a) fire the per-socket pings in parallel with a `sync.WaitGroup` and a shared cancellation channel, or (b) move the sweep to a goroutine that runs concurrently with daemon startup so it cannot block `daemon.New()` from returning. The pings should share a short timeout (`1 * time.Second`) since a stale socket typically refuses connection immediately.

### FR-18: Delete snapshot only after successful restoration
`muxcore/snapshot/snapshot.go:~148` calls `os.Remove(path)` immediately after `json.Unmarshal` succeeds but before the daemon has actually restored any owner state. An OOM kill or SIGKILL between the remove and the owner re-spawn loop produces a cold start on next boot — graceful restart silently becomes ungraceful. Remediation must defer the delete until all owners have been successfully re-registered, or move to a two-file scheme where the snapshot is renamed to `*.loaded` during restore and only deleted on successful completion.

### FR-19: Close the `upgrade.Swap` two-rename window
`muxcore/upgrade/upgrade.go:~35–44` renames `currentExe → oldPath` then `newExe → currentExe`. Between these two calls the binary is absent on disk — any concurrent `exec` of the mcp-mux path fails. Remediation must use `os.Link` + `os.Rename` (link the new binary into place, then atomically replace) on platforms that support hard-link-then-rename, or document the race as "accept risk on Windows; low likelihood because shims rarely exec during upgrade". The chosen approach must be documented in the function comment.

### FR-20: Make `generateToken` fail-fast on entropy exhaustion
`muxcore/daemon/daemon.go:338` `generateToken`'s fallback path `hex.EncodeToString([]byte(fmt.Sprintf("%016x", time.Now().UnixNano())))` double-encodes and produces a deterministic, guessable value if `crypto/rand.Read` fails. Session binding is defeated. Remediation must replace the fallback with a `panic("crypto/rand unavailable")` or a fatal log — entropy failure is a fatal condition, not a recoverable one. A test must verify the fatal path via monkey-patching `rand.Reader`.

### FR-21: Raise engine and session test coverage on critical orchestration paths
Specifically add unit tests for: `engine.runProxy` SessionHandler-only-no-Handler error path (currently 0%, lines 297–300 in engine.go), `engine.waitForDaemon` timeout path (currently 0%, lines 352–367), `session.WriteRaw` write-deadline branch (0% at package scope — `conn != nil` path), `session.SendNotification` drop-oldest buffer-full path (0% at package scope, lines 174–180). The target is to lift `muxcore/engine` from 18.2% to at least 40% and `muxcore/session` from 40.7% (session_manager-only) to cover the Session struct methods as well.

### Priority 4 (Low — target v0.19.4 or v0.19.5)

### FR-22: Tighten snapshot file permissions explicitly
Regardless of whether FR-10 (move to UserCacheDir + HMAC) lands in the same release, the snapshot file creation path must call `os.OpenFile` with explicit mode `0600` instead of relying on `os.CreateTemp`'s platform-dependent default. This is a defense-in-depth measure.

### FR-23: Set suture supervisor tuning explicitly
`muxcore/daemon/daemon.go:180` constructs `suture.New("mcp-mux-daemon", suture.Spec{...})` with only `EventHook` set. The comment above documents intended tuning (`FailureThreshold=5`, `FailureDecay=30s`, `FailureBackoff=15s`) but the code passes zero values, which fall back to suture v4 library defaults (`FailureThreshold=10`). Remediation must set all three fields explicitly to match the documented intent.

### FR-24: Document missing environment variables in README
README.md's env var table currently omits `MCP_MUX_OWNER_IDLE` (supersedes the documented `MCP_MUX_GRACE`), `MCP_MUX_SHIM_LOG` (debug-level shim logging to file), and `MCP_MUX_DAEMON` (sets `--daemon` flag via env). Remediation must add table rows for all three with default values and purpose. `MCP_MUX_SESSION_ID` is intentionally not documented — it is set internally by muxcore to mark proxy mode and is not user-facing.

### FR-25: Dedupe `collectEnv` helper
`collectEnv` is duplicated verbatim across `cmd/mcp-mux/main.go:560` and `muxcore/engine/engine.go:398`. Remediation must extract a single definition into `muxcore/serverid` (or a new `muxcore/envutil` package) and re-export from both call sites. Pure mechanical refactor.

### FR-26: Clear justification comments for `//nolint` directives
Three `//nolint:errcheck` directives in test code lack a justification comment: `muxcore/control/control_test.go:464`, `muxcore/owner/dispatch_test.go:230, 234`. Remediation must add a one-line reason comment above each directive.

### FR-27: Surface or suppress the `runUpgrade` fallback shutdown error
`cmd/mcp-mux/main.go:511` calls `control.Send(ctlPath, control.Request{Cmd: "shutdown"})` and discards both return values without `_ =`. This is a fallback shutdown invoked when graceful-restart failed. Remediation must capture the return and log at warning level so operators can see whether the fallback landed.

## Non-Functional Requirements

### NFR-1: Test coverage regression
Every new fix must ship with a regression test that fails on the pre-fix code and passes on the post-fix code. No fix lands without a corresponding test. The test must be runnable under `go test -count=1 ./...` without external dependencies (no network, no Docker, no subprocess beyond what existing tests already use).

### NFR-2: Build + vet cleanliness
After every commit, `go build ./...` and `go vet ./...` must remain clean on both the root module and the `muxcore/` submodule. Any commit that breaks either is reverted before the next commit.

### NFR-3: Behavioral preservation
No fix may change observable behavior outside of what its FR explicitly describes. Specifically: no changes to the MCP wire protocol, no changes to the control-plane command set, no changes to the IPC path layout, no changes to env var names or semantics (only new additions allowed).

### NFR-4: PR reviewability
Each PR must contain at most 3 FRs. PRs must be reviewable in under 15 minutes by a human. Fixes that require extensive refactoring (FR-6 retry loop, FR-8 lock semantics, FR-10 snapshot hardening, FR-21 coverage lifts) get their own PRs.

### NFR-5: Backwards compatibility of muxcore API
Engine consumers (aimux, engram) must be able to upgrade from muxcore/v0.19.2 to v0.19.3 via `go get` with zero source code changes. No breaking changes to `engine.Config`, `daemon.Config`, `owner.OwnerConfig`, or `muxcore.SessionHandler`/`Notifier`/`ProjectLifecycle` interfaces. New fields may be added as optional (zero value = prior behavior).

### NFR-6: Verification cascade
Every FR must pass `/confidence-check` verification before it is marked complete in tasks.md. Facts claimed in commit messages (e.g., "fixed in muxcore/owner/owner.go:1395") must be verified by reading the file post-edit, not just by trusting the Edit tool's "file state current" notification.

### NFR-7: Deploy and verify each fix
After each PR merges into master, muxcore does NOT need an intermediate tag — only the final v0.19.3 tag. However, after the final tag, `mcp-mux upgrade --restart` must be run locally and `mcp-mux status` must confirm all live owners reconnect cleanly (as was done for v0.19.2 today).

## User Stories

### US1: Maintainer ships v0.19.3 without CPU spikes (P1)
**As a** maintainer of mcp-mux, **I want** `Owner.Serve()` to exit cleanly when `SpawnUpstreamBackground` fails, **so that** a single upstream spawn failure does not burn 5–10 CPU cycles per crashed owner before suture's failure threshold engages.

**Acceptance Criteria:**
- [ ] Regression test reproduces the failed-background-spawn scenario and asserts `Serve` does not return more than once within 1 s
- [ ] `mcp-mux status` shows 0 CPU spike in process monitor when a flaky upstream is spawned
- [ ] suture FailureThreshold/Backoff explicitly set (FR-23) so the cap on restart cycles is known

### US2: Operator sees no stalled sessions from slow IPC consumers (P1)
**As an** operator running mcp-mux with a slow IDE consumer, **I want** `ownerNotifier.Notify` to release the owner lock before the IPC write, **so that** one slow writer does not stall every other session on the same owner for up to 30 s.

**Acceptance Criteria:**
- [ ] `Notify` uses the same copy-release-write pattern as `Broadcast`
- [ ] Regression test installs a writer that blocks for 5 s, starts a concurrent `addSession`, and asserts `addSession` completes within 100 ms
- [ ] No deadlock under `go test -race`

### US3: CC never drops connection because of a quoted error message (P1)
**As a** user of mcp-mux with SessionHandler-based consumers (aimux, engram), **I want** error responses from `dispatchToSessionHandler` to always be valid JSON, **so that** a handler returning an error containing a Windows path or a quoted string does not crash the MCP wire.

**Acceptance Criteria:**
- [ ] Regression test with error messages containing `"`, `\`, `\n`, `\t`, `\u0000`, and `C:\foo\bar` all round-trip through `json.Unmarshal`
- [ ] Fix uses `json.Marshal` or routes through `respondWithError`
- [ ] No other interpolation sites in `owner.go` have the same pattern (audit)

### US4: Concurrent Spawn does not lose a fresh owner to eviction (P2)
**As a** maintainer, **I want** `cleanupDeadOwner` to only evict the owner it was called for, **so that** a fresh concurrent Spawn for the same `sid` is not accidentally deleted.

**Acceptance Criteria:**
- [ ] Identity-guarded delete lands
- [ ] Regression test interleaves fresh Spawn with cleanup and asserts the fresh entry survives

### US5: Control socket does not leak goroutines on silent clients (P2)
**As a** maintainer, **I want** every control-socket connection to have a read deadline, **so that** a client that connects and sends nothing cannot hold a daemon goroutine indefinitely.

**Acceptance Criteria:**
- [ ] `handleConn` sets `SetReadDeadline` before every read
- [ ] Regression test connects without writing, asserts server goroutine exits within `clientDeadline + 1s`

### US6: Multi-user Unix deployment is portable (P3)
**As a** user deploying mcp-mux on a shared Linux host, **I want** the control socket and snapshot file to be user-private, **so that** another local account cannot hijack my daemon.

**Acceptance Criteria:**
- [ ] Control socket is `0600` on Unix
- [ ] Snapshot lives in `os.UserCacheDir()` with `0700`/`0600`
- [ ] Snapshot is HMAC-signed with an in-memory runtime key; tampered snapshots are rejected
- [ ] Windows build continues to work (no-op chmod)

### US7: README documents every user-facing env var (P4)
**As a** new user reading the README, **I want** every `MCP_MUX_*` environment variable to appear in the env var table, **so that** I can configure the daemon without grepping the source.

**Acceptance Criteria:**
- [ ] `MCP_MUX_OWNER_IDLE`, `MCP_MUX_SHIM_LOG`, `MCP_MUX_DAEMON` added to README table
- [ ] `MCP_MUX_SESSION_ID` intentionally omitted, with a code comment explaining why
- [ ] AGENTS.md cross-references README for env var reference

## Edge Cases

- **Concurrent SpawnUpstreamBackground failures:** if two owners spawn in parallel and both fail, FR-1 fix must handle both cleanly (each owner independently transitions to clean shutdown; no cross-owner interference).
- **HMAC key loss between snapshot write and read:** if the daemon crashes after writing a snapshot but before restart, the in-memory HMAC key is lost. On next restart a new key is generated and the old snapshot is rejected. This is acceptable (matches the intent that snapshots are only for graceful restart, not crash recovery). Document the behavior explicitly.
- **Windows vs Unix test split:** FR-9 (socket permissions) and FR-22 (snapshot file mode) use build tags `!windows` so tests compile and run on all platforms without false failures.
- **Goroutine count regression tests (FR-11, FR-12):** must use `runtime.NumGoroutine()` before and after with a small tolerance (±2) to avoid flakiness from the Go runtime's own worker pool scaling.
- **FR-16 atomic-swap test seam:** adding a test seam to production code (`var osRename = os.Rename`) is a small concession to testability. If the team prefers zero production complexity, FR-16 can be deferred entirely — the skip is a documented gap, not a blocker.
- **FR-17 async sweep race:** if the async sweep identifies a stale socket and removes it while a second daemon instance is legitimately trying to bind that same path, the second bind may race against the sweep. Mitigation: the sweep must `stat` and `control.Send ping` before removing, same as the current sync path.
- **FR-10 snapshot HMAC:** if a consumer starts the engine with `SkipSnapshot=true` (added in PR #51), the HMAC logic is never exercised — tests must cover both the snapshot-enabled and snapshot-disabled paths.

## Out of Scope

- **Feynman reality check PARTIAL coverage** on SessionHandler live-consumer exercise. Engaging a real SessionHandler consumer (aimux or engram) in the live daemon is consumer work, not mcp-mux work. Tracked separately.
- **Dependency CVE audit** (`govulncheck ./...`). Deferred to a separate `/nvmd-specify --quick` round; not part of the fix batch.
- **Larger refactor of owner.go into smaller files.** The audit accepted the 2100-LOC file as a justified cohesive state machine. File splitting is explicitly out of scope.
- **Observability improvements** (structured logging via slog, Prometheus metrics, OpenTelemetry tracing). Tracked as separate future work.
- **Suture v5 migration or alternative supervisor selection.** Current suture v4.0.6+ is adequate; no framework churn in this batch.
- **Linux/macOS CI matrix.** Current CI runs Windows (as the primary target); adding Unix CI is a separate initiative. FR-9, FR-10, FR-22 will be tested on Unix via local WSL or CI expansion, but expanding CI is not in this spec.

## Dependencies

- `github.com/thejerf/suture/v4` at v4.0.6 or later (already pinned in go.sum)
- `crypto/hmac` + `crypto/sha256` (Go stdlib) for FR-10 snapshot signing
- `golang.org/x/sys/unix` for `os.Chmod` on the socket — already indirectly available via `os.Chmod`
- No new third-party dependencies
- Assumes `go.sum` for the muxcore submodule is fresh and verified

## Success Criteria

- [ ] All 27 FRs implemented with passing regression tests
- [ ] `go test -count=1 -race ./...` green on root module and muxcore submodule
- [ ] `go vet ./...` clean
- [ ] muxcore coverage: engine ≥ 40%, session ≥ 60%, overall ≥ 78%
- [ ] `mcp-mux upgrade --restart` from v0.19.2 to v0.19.3 reconnects all live owners cleanly, 3× consecutive runs
- [ ] Re-run `/nvmd-platform:production-ready-check` post-v0.19.3: WTF-points drop from 92 to ≤ 20, verdict upgrades from CONDITIONALLY READY to READY
- [ ] Every PR reviewed by `nvmd-platform:pr-reviewer` background sonnet agent with zero unresolved threads before merge
- [ ] CONTINUITY.md Known Follow-ups section is empty after v0.19.3 ships

## Open Questions

- **[NEEDS CLARIFICATION]** FR-10 HMAC key lifetime: should the key regenerate on every daemon boot (current proposal) or persist to a separate OS-keyring location (more complex, but survives crash)? Recommendation: per-boot regeneration; document the crash-recovery limitation explicitly.
- **[NEEDS CLARIFICATION]** FR-16 production test seam: accept the small production complexity of `var osRename = os.Rename` to enable the rollback test, or defer FR-16 entirely with a documented gap? Recommendation: defer (low ROI, low risk — the rename-back path is simple code that is hard to break accidentally).
- **[NEEDS CLARIFICATION]** Priority 2 batching: bundle FR-6..FR-10 into v0.19.3 alongside the Priority 1 blockers, or hold them for v0.19.4 to keep the v0.19.3 PR batch small and fast? Recommendation: land FR-6, FR-7, FR-8 in v0.19.3 (same files as blockers, reviewable together); defer FR-9, FR-10 to v0.19.4 (Unix portability is a bigger scope and deserves its own PR train).

---

## Amendment: 2026-04-18 — PRC-2026-04-18 (multi-user hardening)

> **Provenance:** Amended by claude-opus-4-7[1m] on 2026-04-18.
> Evidence from: `.agent/reports/2026-04-18-production-readiness.md` (CONDITIONALLY READY verdict), `.agent/reports/2026-04-18-prc-security-scan.md` (2 HIGH findings), `.agent/reports/2026-04-18-prc-code-review.md`. All file:line references read directly.
> Confidence: **VERIFIED** for FR-28 (code trace through `owner.go:acceptLoop` in security review). **VERIFIED** for FR-29 (file paths `serverid/serverid.go:188,195` + `cmd/mcp-mux/daemon.go:67` confirmed via Read this session).
> Reason for amendment: v0.9.9 shipped 5 PRC hardening fixes (ownerNotifier sync→async, removeSession tracker cleanup, daemon log defer Close, generateToken 128-bit, findSharedOwnerLocked rename). Two HIGH security findings remained open (S8-001, S5-001) and are explicitly part of the same "post-audit remediation" arc. Per autopilot rule (Scope Expansion = Additional Pipeline Iteration, NON-NEGOTIABLE): amend the active spec rather than creating a new one. FR-9 (scoped to `muxcore/ipc/transport.go:25` for control socket) is extended in scope here to cover every IPC socket call site; FR-28 is genuinely new.
> Scope relationship to prior FRs: FR-28 has NO prior FR. FR-29 overlaps FR-9 — FR-9 is retained as-is (it was deferred and never implemented); FR-29 expands the scope to `serverid/serverid.go` IPC data socket + `cmd/mcp-mux/daemon.go` control socket and specifies a single implementation approach (umask around `net.Listen`). Upon FR-29 implementation, FR-9 is considered absorbed — mark FR-9 as superseded by FR-29 in plan.md and tasks.md.

### FR-28: Enforce token handshake in Owner.acceptLoop

**Severity:** HIGH (S8-001) · **File:** `muxcore/owner/owner.go:1607-1621` (`acceptLoop`) · **Source:** PRC-2026-04-18 security scan

The current `acceptLoop` reads the token when `o.tokenHandshake == true` but does not reject the connection on missing or unregistered token. A session is constructed and added to `o.sessions` regardless of `sessionMgr.Bind` outcome. Any local process that can reach the IPC data socket can inject arbitrary JSON-RPC messages into the owner's upstream pipe.

Remediation must:
1. Add `SessionManager.IsPreRegistered(token string) bool` as an **exported** muxcore API (per C3) — returns true iff the token exists in the pre-registered set, without consuming it. Side-effect-free.
2. In `acceptLoop`, after `readToken(conn)` succeeds: if `token == ""` OR `!o.sessionMgr.IsPreRegistered(token)` → log rejection (per C1/C4 below) → `conn.Close()` → `continue`.
3. Preserve existing single-use semantics: `Bind` still consumes the token on success. Per C2, **rejection does NOT consume** the pre-registered token — only successful `Bind` does — to allow transient-failure retry on the legitimate client path.
4. Rejection log format (per C1): `accept: rejected connection from pid=%d (invalid/missing token)`. The peer PID is read via `SO_PEERCRED` on Unix (`syscall.Ucred`) and left as `-1` on Windows. **Never log the token value itself** — tokens in logs = auth bypass via log-file read.
5. Rate-limit rejections (per C4): track rejections per owner in a 60 s sliding window; cap log emissions at 10 per minute. When the cap is hit, suppress further per-rejection lines and emit a single `accept: rate-limited: N rejections suppressed in last 60s` summary every 60 s until the window quiets. Implementation: simple `time.Time` ring buffer of size 10, no external dependency. Rejection itself is NEVER rate-limited — only the log emission.

**Regression test MUST:**
- Exercise `acceptLoop` via a real in-process listener (not mock `net.Conn`).
- Construct three cases: (a) empty token handshake → rejected, (b) random unregistered token → rejected, (c) pre-registered token via `sessionMgr.PreRegister` → accepted and session added.
- Assert rejected connections leave `o.sessions` unchanged and close the socket within 100 ms.
- Run under `-race` with concurrent valid + invalid connects.

**Backward compatibility:** FR-28 only activates when `o.tokenHandshake == true`. Engine consumers that run with `tokenHandshake == false` (legacy mode) are unaffected. mcp-mux daemon-mode always enables `tokenHandshake` — this is the production path.

### FR-29: Restrict all IPC/control socket permissions to 0600 on Unix

**Severity:** HIGH (S5-001) · **Files:** `muxcore/serverid/serverid.go:188, 195`, `cmd/mcp-mux/daemon.go:67`, `muxcore/ipc/transport.go:25` · **Source:** PRC-2026-04-18 security scan

Every `net.Listen("unix", path)` call in the project creates the socket under the current process umask (typically `022` → `0755`). On Unix systems with a shared `/tmp` (default on Linux/macOS), any local account can `connect()` to the socket and impersonate a session (combined with FR-28 this is defense-in-depth; without FR-28 it is a direct RCE vector).

Remediation must: wrap every `net.Listen("unix", path)` call with a `syscall.Umask(0177)` set-and-restore pair so the socket is created with mode `0600`. Because `syscall.Umask` is process-global and not thread-safe, the implementation MUST serialize calls through a shared `sync.Mutex` (package-level) to prevent races where one goroutine's listen concurrently clobbers another's mask.

Call sites to fix (all must be covered):
1. `muxcore/serverid/serverid.go:188` (IPC data socket)
2. `muxcore/serverid/serverid.go:195` (IPC data socket, secondary)
3. `cmd/mcp-mux/daemon.go:67` (daemon control socket)
4. `muxcore/ipc/transport.go:25` (engine-consumer IPC socket — absorbs FR-9)

Windows: `syscall.Umask` is not available. Implementation MUST use build tags (`sockperm_unix.go` + `sockperm_windows.go`) so Windows continues to rely on ACLs. Unix file: active. Windows file: no-op wrapper that delegates to `net.Listen` without `umask` changes. Per C5, `sockperm_windows.go` MUST include a package-level doc comment citing verified behavior: *"On Windows 10 1803+ AF_UNIX sockets inherit the creating process's default DACL, granting access only to the owner SID and LocalSystem. Named pipes created via `net.Listen(\"unix\", path)` on Windows follow the same model. No `Umask`-equivalent API is needed."* This prevents a future reviewer from mistakenly adding a Windows-side permission-tightening hack.

**Regression test MUST (build tag `!windows`):**
- Unit test per call site: create the listener via `sockperm.Listen`, `os.Stat(path)`, assert `mode & 0777 == 0600`.
- Race test: 50 concurrent `sockperm.Listen` calls across unique socket paths — assert all 50 sockets have `mode == 0600` (proves the mutex is correctly serializing).
- Cleanup: `os.Remove(path)` after each test to prevent bleed.

**Backward compatibility:** On Unix the permission is tightened (0755 → 0600). Existing daemons running with 0755 sockets will continue to work after upgrade — only NEW sockets created post-upgrade are 0600. Migration note in release notes: "after v0.9.10 upgrade, restart the daemon to apply 0600 permissions to the control socket." On Windows: zero effect.

### NFR-8: Platform-specific build tag discipline

Every platform-divergent code path introduced by FR-28 or FR-29 MUST use Go build tags (`//go:build unix` + `//go:build windows`) rather than runtime `runtime.GOOS` checks. Runtime checks leave dead code in every binary and obscure platform-specific behavior. Build-tag files MUST be named with the `_unix.go` / `_windows.go` suffix so `go vet` and reviewers can locate them by convention.

### NFR-9: Defense-in-depth layering

FR-28 (application-layer authentication) and FR-29 (OS-layer permissions) are complementary, not redundant. Both MUST ship — FR-28 alone does not protect against a process that has already circumvented token bootstrap (e.g., reads the token from a log file); FR-29 alone does not protect against a legitimate user running a malicious binary in their own session. Remediation is incomplete if either is deferred.

### NFR-10: No regression on v0.9.9 behavior

FR-28 and FR-29 MUST NOT regress any behavior validated in PRC-2026-04-18 (supervisor-loop elimination, ownerNotifier async delivery, progressTracker cleanup, token entropy). Regression matrix:
1. `go test -count=1 -race ./muxcore/...` green pre- and post-implementation.
2. `mux_list` + `mux_restart` round-trip on a live multi-session daemon works identically before and after.
3. The daemon-log storm counter stays at 0 for a 30-minute window post-deploy (same measurement method used for v0.9.8 and v0.9.9 verification).

### Priority 1b (Amendment-level Blockers — must land in muxcore/v0.20.4 + mcp-mux/v0.9.10)

These are blockers for the "multi-user / shared-machine deployment" extended use case only. For the primary single-user local desktop use case, the verdict remains READY without them. Prioritization: ship both in one combined PR train to avoid a half-landed state where FR-28 is enforced but FR-29 is not (that combination surfaces false-positive "rejected token" logs from any local probe).

### US8: Operator deploys mcp-mux on a shared workstation without impersonation risk (P1)

**As an** operator on a Linux workstation shared with other accounts,
**I want** `mcp-mux` to reject unauthorized IPC connections at both the OS and application layer,
**so that** another local user cannot inject JSON-RPC into my upstream MCP servers (e.g., file editors, shell tools).

**Acceptance Criteria:**
- [ ] A probe connection from `nc -U /tmp/mcp-mux-*.sock` is rejected at the OS layer (`permission denied`) on Unix.
- [ ] A probe connection from the same user account with an invalid token is rejected at the application layer within 100 ms with a log entry at owner-level.
- [ ] A probe connection with a valid pre-registered token is accepted and the session proceeds normally.
- [ ] The daemon log contains no false-positive "rejected" entries during a 1-hour normal-use window.

### US9: Windows user sees zero behavior change after v0.9.10 (P1)

**As a** Windows user on the primary deployment platform,
**I want** v0.9.10 to be a drop-in upgrade with no new permission prompts, no new error dialogs, and no perceptible difference from v0.9.9,
**so that** the hardening does not disrupt the primary use case.

**Acceptance Criteria:**
- [ ] `mcp-mux upgrade --restart` from v0.9.9 to v0.9.10 completes without user interaction on Windows 11.
- [ ] `mux_list` shows the same session count before and after upgrade.
- [ ] No new log warnings, no new entries in the Windows Event Viewer.

## Success Criteria (Amendment)

- [ ] FR-28 implemented with `SessionManager.IsPreRegistered(token) bool` API and corresponding regression tests.
- [ ] FR-29 implemented across all 4 call sites with build-tag separation (`_unix.go` / `_windows.go`).
- [ ] muxcore v0.20.4 tagged and released with both FRs.
- [ ] mcp-mux v0.9.10 bundles muxcore v0.20.4 and deploys via `mcp-mux upgrade --restart`.
- [ ] Post-deploy security re-scan: S8-001 and S5-001 both close.
- [ ] `nvmd-platform:pr-reviewer` sonnet background review green with zero unresolved threads before merge.
- [ ] `.agent/reports/` contains a new follow-up PRC run demonstrating verdict upgrade from CONDITIONALLY READY to READY for the multi-user deployment scenario.
