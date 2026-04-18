# Implementation Plan: Post-Audit Remediation

**Feature:** post-audit-remediation
**Spec:** `.agent/specs/post-audit-remediation/spec.md`
**Clarifications:** `.agent/specs/post-audit-remediation/clarifications/2026-04-15-auto.md`
**Target release:** muxcore/v0.19.3 (+ v0.19.4 for Priority 2 portability)

## Scope (after clarifications)

**v0.19.3 release (this plan):** FR-1..FR-8 = 5 P1 blockers + 3 adjacent P2 Highs = **8 FRs across 4 PRs**.

**v0.19.4 release (follow-up plan):** FR-9..FR-21 — out of this plan's scope.

**v0.19.5 release (optional):** FR-22..FR-27 — out of this plan's scope.

## PR Batching Strategy

Per NFR-4 (≤3 FRs per PR for reviewability) and file-proximity grouping:

| PR | Branch | FRs | Files | Scope |
|----|--------|-----|-------|-------|
| **PR-A** | `fix/owner-concurrency-bundle` | FR-1, FR-2, FR-3 | `muxcore/owner/owner.go` | 3 P1 fixes all in owner.go — CPU spin, lock release, JSON escape |
| **PR-B** | `fix/daemon-spawn-concurrency` | FR-4, FR-6, FR-8 | `muxcore/daemon/daemon.go` | 3 daemon.go concurrency fixes — TOCTOU guard, retry loop, findSharedOwner lock |
| **PR-C** | `fix/control-read-deadline` | FR-5 | `muxcore/control/server.go` | 1 fix — control server read deadline |
| **PR-D** | `fix/resilient-client-error-visibility` | FR-7 | `muxcore/owner/resilient_client.go` | 1 fix — drainOrphanedInflight error logging |

**Why this batching:**
- PR-A keeps the 3 owner.go fixes together (they share the file, similar test infrastructure, one review round). 3 FRs = NFR-4 upper limit.
- PR-B keeps 3 daemon.go concurrency fixes together for the same reason.
- PR-C is small and isolated (new dependency: `clientDeadline` constant must be accessible from server).
- PR-D is small and isolated (resilient_client.go has its own test file).

**PR-B depends on PR-A?** No — different files. Can run in parallel.
**PR-C depends on anything?** No — control package has no touch from other PRs.
**PR-D depends on anything?** No — resilient_client is isolated.

So all 4 PRs can potentially be prepared in parallel, but merged sequentially to keep master clean. Sequential merge order: **A → B → C → D** (based on priority, not dependency).

## Per-PR Architecture

### PR-A: `fix/owner-concurrency-bundle` (FR-1, FR-2, FR-3)

**Files:**
- `muxcore/owner/owner.go` — 3 edits
- `muxcore/owner/dispatch_test.go` — new test for FR-3 JSON escape (if file exists; otherwise new file)
- `muxcore/owner/owner_serve_test.go` — new test for FR-1 CPU spin (the file exists, has the Skip#4 that we'll leave for FR-15 in v0.19.4)
- `muxcore/owner/notifier_test.go` — new file for FR-2 Notify lock release test

**FR-1 fix approach:**
- In `SpawnUpstreamBackground` failure path (where the goroutine completes with a spawn error), call `o.Shutdown()` before returning. This closes `o.done`, and the next `Serve` iteration exits cleanly via the priority check at line 1754 (`case <-o.done: return nil`).
- Alternative considered: return `suture.ErrDoNotRestart` from `upstreamDeathResult` when `up == nil && bgCh == nil && deadCh == closedChan`. Rejected because it is a wider change that affects the cold-start path too.
- The chosen approach is minimal: one `o.Shutdown()` call in the background-spawn goroutine's error handler.

**FR-2 fix approach:**
- Copy the target session pointer under RLock, release the lock, then call `WriteRaw` on the local copy. Exactly mirrors `Broadcast` at lines 1406–1416.
- Before:
  ```go
  func (n *ownerNotifier) Notify(projectID string, notification []byte) error {
      n.owner.mu.RLock()
      defer n.owner.mu.RUnlock()
      for _, s := range n.owner.sessions {
          if muxcore.ProjectContextID(s.Cwd) == projectID {
              return s.WriteRaw(notification)
          }
      }
      return fmt.Errorf("no session found for project %s", projectID)
  }
  ```
- After:
  ```go
  func (n *ownerNotifier) Notify(projectID string, notification []byte) error {
      n.owner.mu.RLock()
      var target *Session
      for _, s := range n.owner.sessions {
          if muxcore.ProjectContextID(s.Cwd) == projectID {
              target = s
              break
          }
      }
      n.owner.mu.RUnlock()
      if target == nil {
          return fmt.Errorf("no session found for project %s", projectID)
      }
      return target.WriteRaw(notification)
  }
  ```

**FR-3 fix approach:**
- Replace the two `fmt.Sprintf` JSON literals at owner.go:894 and owner.go:896 with a struct + `json.Marshal`:
  ```go
  type jsonrpcError struct {
      JSONRPC string         `json:"jsonrpc"`
      ID      json.RawMessage `json:"id"`
      Error   struct {
          Code    int    `json:"code"`
          Message string `json:"message"`
      } `json:"error"`
  }
  ```
- Use `json.Marshal` on a populated struct; fall back to a hardcoded valid-JSON bytes on marshal failure (marshal of simple strings cannot fail, but defense-in-depth).
- Alternative considered: route through `respondWithError`. Rejected because the helper writes directly to the session, while `dispatchToSessionHandler` builds the response bytes for later write under a different code path (the `resp != nil → s.WriteRaw(resp)` at line 900).

**Regression tests:**
- `TestDispatchToSessionHandler_ErrorMessageIsValidJSON` — table-driven with cases `"simple"`, `"with \"quote\""`, `"backslash: \\"`, `"newline\nembedded"`, `"tab\tembedded"`, `"null\u0000byte"`, `"C:\\Users\\path"`. For each: call dispatcher with a handler that returns `errors.New(msg)`, capture the response, `json.Unmarshal` it, assert the decoded error message equals the input.
- `TestOwnerServe_FailedBackgroundSpawnDoesNotSpin` — install a HandlerFunc that sleeps briefly then errors, enable `FromSnapshot` path so the owner has `backgroundSpawnCh`, call `Serve` once via supervisor, assert `Serve` returns ≤1 time within 500ms.
- `TestOwnerNotifier_NotifyReleasesLock` — install a session with a writer that blocks for 500ms, call `Notify` in a goroutine, concurrently call `removeSession` and assert it completes within 100ms (not 500ms).

### PR-B: `fix/daemon-spawn-concurrency` (FR-4, FR-6, FR-8)

**Files:**
- `muxcore/daemon/daemon.go` — 3 edits
- `muxcore/daemon/daemon_test.go` — 3 new regression tests

**FR-4 fix approach:**
- Locate `cleanupDeadOwner` (around line 291). Before the `delete(d.owners, sid)` call, add an identity check:
  ```go
  if current, ok := d.owners[sid]; ok && current == observed {
      delete(d.owners, sid)
  }
  ```
  where `observed` is the entry pointer captured at the function's entry.

**FR-6 fix approach:**
- Replace the recursive calls at daemon.go:435 and daemon.go:458 with a labeled retry loop wrapping the whole Spawn body:
  ```go
  func (d *Daemon) Spawn(req control.Request) (string, string, string, error) {
      const maxRetries = 3
      for retry := 0; retry < maxRetries; retry++ {
          result, err, retrySignal := d.spawnOnce(req)
          if !retrySignal {
              return result.ipcPath, result.sid, result.token, err
          }
      }
      return "", "", "", fmt.Errorf("spawn %s: exhausted retry budget", req.Command)
  }
  ```
- `spawnOnce` is the existing Spawn body split out. On paths that previously did `return d.Spawn(req)`, set `retrySignal = true` and return zero values.
- The current L458 path that mutates `req.Mode = "isolated"` is preserved by mutating the shared `req` reference before the retry — each iteration sees the mutated mode.

**FR-8 fix approach:**
- Refactor `findSharedOwner` to a 2-phase pattern: under `d.mu.RLock()`, build a slice of candidate entries (name + pointer). Release the lock. Scan the slice, skipping entries whose state has diverged.
- The caller (`Spawn`) currently holds `d.mu.Lock()` when calling `findSharedOwner`. Change caller to release the lock before the call and re-acquire if a match is found. Document the new contract: "findSharedOwner acquires its own read lock; the caller must not hold d.mu".
- Alternative considered: pass a pre-snapshotted map. Rejected because the snapshot still dereferences pointers, so staleness doesn't disappear.

**Regression tests:**
- `TestCleanupDeadOwner_IdentityGuard` — inject an observed dead owner, interleave a fresh Spawn for the same sid, assert the fresh entry is NOT deleted.
- `TestSpawn_RetryBudgetExhausted` — force the creating-placeholder timeout path to trigger retries, cap `maxRetries=3`, assert error is returned on the 4th would-be call.
- `TestFindSharedOwner_ConcurrentMutation` — run `findSharedOwner` in one goroutine and concurrent `Spawn`/`Remove` in another, assert no panic, no stale pointer dereference (runs under `-race`).

### PR-C: `fix/control-read-deadline` (FR-5)

**Files:**
- `muxcore/control/server.go` — 1 edit (add `SetReadDeadline` in `handleConn`)
- `muxcore/control/control_test.go` — 1 new test

**FR-5 fix approach:**
- In `handleConn`, before the first `bufio.NewReader(conn).ReadBytes('\n')`, call `conn.SetReadDeadline(time.Now().Add(clientDeadline))`.
- Optionally extend the deadline after each successful read to support persistent connections. For mcp-mux's usage (one-shot commands) a single deadline is sufficient.
- `clientDeadline` is already defined in `muxcore/control/client.go:10` as `5 * time.Second`. Make it package-visible (already is).

**Regression test:**
- `TestControlServer_ReadDeadlineFiresOnSilentClient` — start a control server, connect via raw `net.Dial`, send nothing, measure how long before the connection is closed from the server side. Assert ≤ `clientDeadline + 1 * time.Second`.

### PR-D: `fix/resilient-client-error-visibility` (FR-7)

**Files:**
- `muxcore/owner/resilient_client.go` — 1 edit
- `muxcore/owner/resilient_client_test.go` — 1 new test

**FR-7 fix approach:**
- Capture the `fmt.Fprintf` return in `drainOrphanedInflight`. On error: log `rc.cfg.Logger.Printf("drain orphaned inflight: write failed for %d requests: %v", count, err)`.
- Track count of failed writes vs total inflight.

**Regression test:**
- `TestDrainOrphanedInflight_LogsWriteFailures` — supply a broken `io.Writer` as stdout, preload the inflight tracker with 3 fake requests, call `drainOrphanedInflight`, assert the logger captured a warning containing "write failed" and "3".

## Release Flow

After all 4 PRs merge:

1. `git pull --ff-only origin master` on primary worktree
2. Verify master HEAD = merged state
3. Run full test suite: `go test -count=1 -race ./...` on root module and `muxcore/` submodule
4. `git tag -a muxcore/v0.19.3 -m <release notes>` — release notes template below
5. `git push origin muxcore/v0.19.3`
6. `gh release create muxcore/v0.19.3 --title ... --notes ...`
7. Docs bump PR for AGENTS.md → bump v0.19.2 → v0.19.3 with release highlights
8. Merge docs PR (via PR review workflow)
9. Pull master, rebuild `mcp-mux.exe~`, run `mcp-mux upgrade --restart`
10. Verify `mcp-mux status` shows all owners reconnected cleanly

## Release Notes Template (v0.19.3)

```
muxcore v0.19.3 — concurrency correctness

Bug fix release. Closes all P1 + P2-High findings from the 2026-04-15 production
readiness audit (.agent/reports/2026-04-15-production-readiness.md).

### Critical fixes
- Owner.Serve no longer CPU-spins when SpawnUpstreamBackground fails (BUG-001, PR-A)
- ownerNotifier.Notify releases the owner lock before IPC writes, matching
  Broadcast semantics (BUG-002, PR-A)
- dispatchToSessionHandler error responses use json.Marshal for the error
  message, fixing invalid JSON on Windows paths, quoted errors, newlines,
  and other special characters. Regression introduced in Phase 4 SessionHandler
  API (PR #47). (H1, PR-A)

### High-severity concurrency fixes
- cleanupDeadOwner now guards its delete with an identity check to prevent
  eviction of a fresh concurrent Spawn for the same serverID. (BUG-003, PR-B)
- control server handleConn sets a read deadline to prevent goroutine leaks
  from silent clients. (BUG-004, PR-C)
- daemon.Spawn replaces the remaining two recursive calls with a bounded
  retry loop (maxRetries=3). Previous PR #52 fix addressed only the timeout
  path; this release closes the post-channel-close recovery paths.
  (BUG-005 / H2, PR-B)
- drainOrphanedInflight logs write failures instead of silently dropping them,
  giving operators visibility when CC stdout is broken at reconnect.
  (BUG-006, PR-D)

---

## Amendment: 2026-04-18 — PR-E (Multi-user hardening plan)

**Spec amendment:** `spec.md` §"Amendment: 2026-04-18 — PRC-2026-04-18 (multi-user hardening)"
**Clarifications:** see `spec.md` §"Clarifications" Session 2026-04-18 (C1–C5)
**Target releases:** `muxcore/v0.20.4` + `mcp-mux/v0.9.10`
**Tasks:** `tasks.md` §"Amendment: 2026-04-18 — PR-E" (T6.1–T6.11) + §"Amendment Release" (T7.1–T7.4)
**Branch:** `feat/multi-user-hardening` (single PR-E bundle — both FRs land together per NFR-9 defense-in-depth rule)

### Scope (amendment)

Two new FRs + three new NFRs land in one PR:

| FR | What | Severity |
|----|------|----------|
| **FR-28** | Enforce token handshake in `Owner.acceptLoop`: reject empty or unregistered tokens with rate-limited, token-value-free log (C1+C4). Pre-registered tokens are preserved on rejection (C2). | HIGH (S8-001) |
| **FR-29** | All four `net.Listen("unix", …)` call sites go through a new `muxcore/internal/sockperm` package that applies `0600` permissions via `syscall.Umask`+mutex on Unix and is a no-op on Windows (FR-9 absorbed; C5 inline doc). | HIGH (S5-001) |
| **NFR-8** | Build-tag discipline (`_unix.go` / `_windows.go`) — no `runtime.GOOS` dispatch. | Quality |
| **NFR-9** | FR-28 + FR-29 ship together (defense-in-depth — neither alone is sufficient). | Release gate |
| **NFR-10** | Zero regression on v0.9.9 behavior — storm counter stays 0 for 30 min post-deploy. | Verification |

FR-9 (original spec, deferred and never implemented) is **superseded** by FR-29 — the new FR absorbs its scope (control socket 0600) and expands to three additional call sites. tasks.md marks this explicitly in `## Out of Tasks`.

### Files to Create

| Path | Purpose |
|------|---------|
| `muxcore/sockperm/sockperm.go` | Public `Listen(network, addr string) (net.Listener, error)` — delegates to platform-specific `listenWithMode`. Doc comment cross-references FR-29 + C5. |
| `muxcore/sockperm/sockperm_unix.go` | `//go:build unix` — package-level `sync.Mutex` serializes `syscall.Umask(0177)` + `net.Listen` + restore-umask. Comment documents the umask race that motivates the mutex. |
| `muxcore/sockperm/sockperm_windows.go` | `//go:build windows` — no-op wrapper. **MANDATORY** package-level doc comment (per C5) citing Windows 10 1803+ AF_UNIX default DACL behavior verbatim. |
| `muxcore/sockperm/sockperm_unix_test.go` | `//go:build unix` — 3 test cases per T6.8 (single, 50-concurrent race, umask-restored). |
| `muxcore/owner/peer_pid_linux.go` | `//go:build linux` — `readPeerPID(conn net.Conn) int` via `SO_PEERCRED` (`syscall.Ucred`). Linux-only — `Ucred` type and `SO_PEERCRED` sockopt are Linux-specific. |
| `muxcore/owner/peer_pid_darwin.go` | `//go:build darwin` — stub returning `-1`. macOS uses `LOCAL_PEERCRED` (different API via `getsockopt` with `xucred` struct) which is out of scope for this amendment; rejection log reads `pid=-1` on macOS. |
| `muxcore/owner/peer_pid_other_unix.go` | `//go:build unix && !linux && !darwin` — stub returning `-1` for FreeBSD/OpenBSD/etc. Same rationale as darwin. |
| `muxcore/owner/peer_pid_windows.go` | `//go:build windows` — stub returning `-1`. No `SO_PEERCRED` equivalent. |
| `muxcore/owner/rejection_logger.go` | `rejectionLogger` struct — 10-entry ring buffer + mutex + 60s summary ticker (C4). |
| `muxcore/owner/accept_loop_test.go` | 4 table cases + concurrent race case + rate-limit case (T6.5). |

### Files to Modify

| Path | Change |
|------|--------|
| `muxcore/owner/session_manager.go` (or equivalent) | Add exported `(sm *SessionManager) IsPreRegistered(token string) bool` — `RLock`-gated, side-effect-free (T6.2). |
| `muxcore/owner/owner.go` (lines 1607–1621) | Insert rejection gate in `acceptLoop` between `readToken` and `AddSession` (T6.4). Owner struct gains `rejectionLogger *rejectionLogger` field wired in `New`. |
| `muxcore/serverid/serverid.go:188, 195` | Replace `net.Listen("unix", …)` → `sockperm.Listen("unix", …)`. |
| `cmd/mcp-mux/daemon.go:67` | Same replacement — daemon control socket. |
| `muxcore/ipc/transport.go:25` | Same replacement — engine-consumer IPC (FR-9 absorbed). |
| `AGENTS.md` | Bump muxcore version reference to `v0.20.4` + note FR-28/FR-29 in release summary (T7.2). |

### Library Decisions

| Component | Library | Rationale |
|-----------|---------|-----------|
| Peer PID on Unix | `syscall.Ucred` via `syscall.GetsockoptUcred` (stdlib) | No external dep; supported since Go 1.x. Windows stub returns `-1` — no cross-platform abstraction dep needed. |
| Umask serialization | `sync.Mutex` (stdlib) | Simple, zero-dep. `syscall.Umask` is process-global, not goroutine-safe — mutex is required. |
| Rejection rate-limit | Ring buffer via `[10]time.Time` + `sync.Mutex` (stdlib) | No external deps. 10 is sufficient per C4; 60s window is a single ticker. |
| Tests | `testing` + `testing/quick` (stdlib) + `net` (stdlib) | Real `net.Listen` via `t.TempDir()` — no mocks for FR-29 perm check (ground truth via `os.Stat`). |

### Approach Decision: single PR-E vs split PR-E1 + PR-E2

**Chosen:** single PR-E bundling FR-28 + FR-29.

**Rationale:** NFR-9 (defense-in-depth) requires both to ship together. A split where FR-28 lands first would create a false-positive "rejected token" log storm from any local probe that previously just got a 500 — and FR-29 landing first without FR-28 surfaces no user-visible issue but leaves the application-layer hole open. Bundling eliminates the intermediate broken state. The PR is still within NFR-4 (≤3 FRs): 2 FRs + NFR housekeeping.

**Rejected alternative:** Split PR-E1 (FR-28) + PR-E2 (FR-29). Rejected because (a) intermediate-state log spam, (b) double-review overhead for interdependent work, (c) tasks.md already structures internal phasing (T6.2-T6.5 for FR-28, T6.6-T6.8 for FR-29) so reviewer can still inspect each FR independently within one diff.

### Test Strategy (amendment)

- **Unit** — every new function has a table-driven test. Build-tag separation for Unix-only tests.
- **Race** — T6.5 case D (concurrent valid+invalid connects) and T6.8 case B (50 concurrent `sockperm.Listen`) MUST pass under `-race`.
- **Integration** — T7.3 post-deploy: `mux_list` round-trip + 30-min storm-counter window. Real mcp-mux instance, not mocks.
- **Security re-scan** — T7.3 explicitly re-runs the 2026-04-18 security scan; S8-001 and S5-001 MUST close.

### Risks & Mitigations (amendment)

| Risk | Mitigation |
|------|-----------|
| `syscall.Umask` mutex misuse under concurrent `sockperm.Listen` calls from the same process → socket created with wrong mode | Test case T6.8-B (50 concurrent goroutines) asserts all sockets are `0600` — any race in the mutex would surface as a failure. Run under `-race`. |
| Windows behavior regression (e.g., named pipe default DACL differs between Win10 editions) | C5 doc comment forces future reviewer to re-verify before touching; `sockperm_windows.go` is a pure no-op by design (zero code = zero regression surface). |
| Pre-registered token set memory growth if clients never call `Bind` | Existing `SessionManager` already has registration timeout; FR-28 does not add new persistence. Rejection path never inserts, only reads. |
| FR-28 rejection rate-limit falsely suppresses legitimate operational debugging | Summary line every 60s with count preserves visibility; cap is per-owner, not global. Ring buffer size 10 is intentionally low — high-rate rejections ARE the anomaly worth alerting on, not suppressing silently. |
| `SO_PEERCRED` unavailable on macOS (only Linux has `Ucred`) | `readPeerPID` returns `-1` on macOS; log reads `pid=-1` instead of `pid=<N>`. Diagnostic value slightly degraded on macOS but not blocking. Document in `peer_pid_unix.go` comment. |

### Success Criteria (amendment)

- [ ] `muxcore/v0.20.4` tagged with FR-28 + FR-29 merged.
- [ ] `mcp-mux/v0.9.10` tagged + GitHub release published with amendment notes.
- [ ] Post-deploy: storm counter = 0 for 30 min, `mux_list` delta = 0 sessions.
- [ ] Re-run `/nvmd-platform:production-ready-check --quick`: verdict READY for multi-user deployment (prior was CONDITIONALLY READY).
- [ ] PR-E merged with all AI-review threads resolved and CI green on 3-OS matrix.
- [ ] Security re-scan: S8-001 and S5-001 both closed.

## Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| FR-1 `o.Shutdown()` in SpawnUpstreamBackground goroutine causes deadlock (Shutdown waits for Serve loop to exit, but the caller IS Serve loop on the next iteration) | Verify the order: the background goroutine is separate from Serve. Shutdown closes `o.done`; Serve's next priority check catches it and returns. No circular wait. |
| FR-6 retry loop changes error returns observable to shim | Shim already retries on error from spawn RPC. Adding a "retry budget exhausted" error is a new error text but the retry semantics are unchanged from shim's perspective. |
| FR-8 findSharedOwner signature change breaks callers | Only one caller (Spawn). Update in same commit. |
| PR-A JSON marshaling adds allocation on every error path | Error paths are rare; allocation is negligible. No perf test required. |
| PR-B retry loop refactor introduces bugs | Each FR in PR-B has its own regression test. go test -race runs on every commit. |
| FR-2 test flakiness from `runtime.NumGoroutine()` assumptions | Use direct lock contention measurement (elapsed time on `addSession` while `Notify` blocks) instead of goroutine count. |
| Release tag happens before all PRs merge | Release script (manual in this repo) requires explicit `git tag` command after merge. No automation triggers early. |

## Verification Checklist per PR

Each PR merges only after:
- [ ] `go build ./...` clean
- [ ] `go vet ./...` clean
- [ ] `go test -count=1 -race <affected packages>` green
- [ ] Regression test for each FR in the PR exercises the pre-fix failure mode and passes post-fix
- [ ] PR review via `nvmd-platform:pr-reviewer` subagent returns zero unresolved threads
- [ ] All reviewer suggestions confidence-checked: apply if correct, reply-resolve if wrong
- [ ] Commit message references the FR IDs and the audit report

## Out of Plan

- FR-9 (Unix socket perms), FR-10 (snapshot HMAC) — v0.19.4 plan
- FR-11..FR-21 (Medium) — v0.19.4 plan
- FR-22..FR-27 (Low) — v0.19.5 plan or one-off docs PR
- govulncheck integration — separate spec
- Observability (slog, metrics, tracing) — separate spec
