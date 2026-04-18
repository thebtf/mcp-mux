# Tasks: Post-Audit Remediation (v0.19.3)

Generated from plan.md. Each task is atomic, testable, and committable independently.

**Execution order:** T1.x (PR-A setup) → T2.x (PR-B setup) → T3.x (PR-C) → T4.x (PR-D) → T5.x (release)

**Parallelism:** PRs A/B/C/D touch different files and can be implemented concurrently in separate worktrees. Tasks within a PR are sequential.

**Owner:** main context (Opus). Implementation is done inline via Edit; no sub-agent delegation for fixes <30 LOC.

---

## PR-A: `fix/owner-concurrency-bundle` (FR-1, FR-2, FR-3)

### T1.1 Create worktree
- [x] `git worktree add ../mcp-mux-wt/owner-concurrency -b fix/owner-concurrency-bundle`

### T1.2 FR-3 fix (easiest, lowest risk — start here for momentum)
- [x] Read `muxcore/owner/owner.go` lines 860–910 to confirm exact current structure
- [x] Introduce local helper `buildErrorResponse(id json.RawMessage, code int, message string) []byte` that uses `json.Marshal` on a struct
- [x] Replace the two `fmt.Sprintf` error response literals at lines 894 and 896 with the helper
- [x] Verify with `go build ./muxcore/owner/...`

### T1.3 FR-3 regression test
- [x] Add `TestDispatchToSessionHandler_ErrorMessageIsValidJSON` to `muxcore/owner/dispatch_test.go` (or create new file)
- [x] Table-driven: `"plain"`, `quoted`, `backslash`, `newline`, `tab`, `null-byte`, `windows-path`
- [x] For each: install a handler that returns `errors.New(input)`, call `dispatchToSessionHandler` via a minimal fixture, capture response bytes, `json.Unmarshal` into a decoder struct, assert round-trip equality
- [x] Run: `go test -count=1 -run TestDispatchToSessionHandler_ErrorMessageIsValidJSON ./muxcore/owner/...`

### T1.4 FR-2 fix
- [x] Read `muxcore/owner/owner.go:1395-1404` (ownerNotifier.Notify current implementation)
- [x] Rewrite to copy-release-write pattern mirroring Broadcast at 1406-1416
- [x] Verify with `go vet ./muxcore/owner/...`

### T1.5 FR-2 regression test
- [x] Add `TestOwnerNotifier_NotifyReleasesLock` to `muxcore/owner/notifier_test.go` (new file)
- [x] Install an owner with 2 sessions. Session A uses a writer that blocks for 500ms. Session B is a normal buffer writer.
- [x] Call `Notify(sessionA_projectID, payload)` in a goroutine.
- [x] Concurrently call `owner.addSession(newSession)` — it requires `o.mu.Lock()`.
- [x] Assert `addSession` completes within 100ms (NOT 500ms — proving the lock was released).
- [x] Clean up the slow writer in test cleanup.

### T1.6 FR-1 fix
- [x] Read `muxcore/owner/owner.go` to find the `SpawnUpstreamBackground` failure path (grep for `SpawnUpstreamBackground` + `logger.Printf.*background.*fail` or similar)
- [x] In the goroutine's error branch, after closing + clearing `backgroundSpawnCh`, call `o.Shutdown()`
- [x] Verify `Shutdown` is idempotent so a subsequent explicit Shutdown does not panic

### T1.7 FR-1 regression test
- [x] Add `TestOwnerServe_FailedBackgroundSpawnDoesNotSpin` to `muxcore/owner/owner_serve_test.go`
- [x] Construct an owner from a template snapshot whose upstream spawn is wired to return an error immediately
- [x] Start `Serve` via a supervisor or directly on a goroutine
- [x] Assert: within 500ms, `Serve` has returned at most once (not 5+ times)
- [x] Assert: CPU-spin-equivalent metric — elapsed time ≥ 400ms with ≤1 iteration signal
- [x] Use `runtime.Gosched()` carefully to avoid false stability

### T1.8 PR-A commit and build
- [x] `go test -count=1 -race ./muxcore/owner/...`
- [x] `go build ./...`
- [x] `go vet ./...`
- [x] Commit: `fix(owner): concurrency bundle — CPU spin, lock release, JSON escape (FR-1, FR-2, FR-3)`

### T1.9 PR-A push and review
- [x] `git push -u origin fix/owner-concurrency-bundle`
- [x] `gh pr create` with body referencing FR-1, FR-2, FR-3 from spec
- [x] Dispatch `nvmd-platform:pr-reviewer` agent in background
- [x] Wait for completion notification

### T1.10 PR-A merge
- [x] Address all reviewer findings (confidence-check each)
- [x] `gh pr merge --squash`
- [x] `git worktree remove ../mcp-mux-wt/owner-concurrency`
- [x] Delete branch (local + remote)
- [x] `git pull --ff-only origin master`

---

## PR-B: `fix/daemon-spawn-concurrency` (FR-4, FR-6, FR-8)

### T2.1 Create worktree
- [x] `git worktree add ../mcp-mux-wt/daemon-spawn -b fix/daemon-spawn-concurrency`

### T2.2 FR-4 fix
- [x] Read `muxcore/daemon/daemon.go` around line 291 — locate `cleanupDeadOwner`
- [x] Capture the expected entry pointer at function entry; add identity guard before `delete(d.owners, sid)`
- [x] Document the new invariant with a comment

### T2.3 FR-4 regression test
- [x] Add `TestCleanupDeadOwner_IdentityGuard` to `muxcore/daemon/daemon_test.go`
- [x] Inject a dead placeholder; capture pointer; concurrently replace with a new live owner; call cleanupDeadOwner with the original pointer; assert the new one survives

### T2.4 FR-6 fix (larger scope — retry loop refactor)
- [x] Extract current Spawn body into `spawnOnce` returning `(string, string, string, error, retrySignal bool)`
- [x] Replace the 2 recursive calls (daemon.go:435 and daemon.go:458) with `retrySignal = true`
- [x] Wrap `spawnOnce` in a retry loop with `maxRetries = 3`
- [x] On exhaustion: return a descriptive error
- [x] Verify all existing tests still pass (no behavior change on happy path)

### T2.5 FR-6 regression test
- [x] Add `TestSpawn_RetryBudgetExhausted` to `muxcore/daemon/daemon_test.go`
- [x] Force a scenario where spawnOnce consistently signals retry (e.g., inject a mock that always returns "owner not accepting")
- [x] Assert error is returned on 4th would-be call

### T2.6 FR-8 fix (larger scope — lock semantics)
- [x] Read `muxcore/daemon/daemon.go` around line 741 — locate `findSharedOwner`
- [x] Refactor to 2-phase: snapshot candidates under `d.mu.RLock()`, scan outside lock
- [x] Update function doc comment to reflect new contract
- [x] Update caller in `Spawn` to not hold `d.mu` when calling `findSharedOwner`, re-acquire after match

### T2.7 FR-8 regression test
- [x] Add `TestFindSharedOwner_ConcurrentMutation` to `muxcore/daemon/daemon_test.go`
- [x] Run 100× iterations of `findSharedOwner` in one goroutine and concurrent `Spawn`/`Remove` in another
- [x] Must pass under `go test -race`

### T2.8 PR-B commit and build
- [x] `go test -count=1 -race ./muxcore/daemon/...`
- [x] `go build ./...`, `go vet ./...`
- [x] Commit: `fix(daemon): spawn concurrency bundle — TOCTOU guard, retry loop, findSharedOwner lock (FR-4, FR-6, FR-8)`

### T2.9 PR-B push and review
- [x] `git push -u origin fix/daemon-spawn-concurrency`
- [x] `gh pr create`
- [x] Dispatch `nvmd-platform:pr-reviewer` agent in background

### T2.10 PR-B merge
- [x] Address reviewer findings
- [x] `gh pr merge --squash`
- [x] Cleanup worktree and branches
- [x] `git pull --ff-only`

---

## PR-C: `fix/control-read-deadline` (FR-5)

### T3.1 Create worktree and implement
- [x] `git worktree add ../mcp-mux-wt/control-deadline -b fix/control-read-deadline`
- [x] Read `muxcore/control/server.go` around line 62
- [x] Add `conn.SetReadDeadline(time.Now().Add(clientDeadline))` before first read
- [x] Refresh deadline between reads if connection is persistent (optional)

### T3.2 FR-5 regression test
- [x] Add `TestControlServer_ReadDeadlineFiresOnSilentClient` to `muxcore/control/control_test.go`
- [x] Start control server, connect via `net.Dial("unix", path)`, send nothing
- [x] Assert server-side goroutine exits within `clientDeadline + 1 * time.Second`

### T3.3 PR-C commit, push, review, merge
- [x] Build, vet, test
- [x] Commit: `fix(control): set read deadline on handleConn (FR-5)`
- [x] Push, gh pr create, review via subagent, merge
- [x] Cleanup, pull master

---

## PR-D: `fix/resilient-client-error-visibility` (FR-7)

### T4.1 Create worktree and implement
- [x] `git worktree add ../mcp-mux-wt/resilient-client -b fix/resilient-client-error-visibility`
- [x] Read `muxcore/owner/resilient_client.go` around line 564 — locate `drainOrphanedInflight`
- [x] Capture `fmt.Fprintf` return; log failures at warning level with count

### T4.2 FR-7 regression test
- [x] Add `TestDrainOrphanedInflight_LogsWriteFailures` to `muxcore/owner/resilient_client_test.go`
- [x] Supply a broken `io.Writer`; preload inflight tracker with 3 requests; call drain
- [x] Capture log via a test logger; assert warning contains "write failed" and "3"

### T4.3 PR-D commit, push, review, merge
- [x] Build, vet, test
- [x] Commit: `fix(owner): log drainOrphanedInflight write failures (FR-7)`
- [x] Push, PR, review, merge, cleanup, pull

---

## Release: muxcore/v0.19.3

### T5.1 Verify master state
- [x] `git pull --ff-only origin master`
- [x] `git log --oneline -10` — confirm all 4 PRs merged in order
- [x] `go test -count=1 -race ./...` on root + muxcore
- [x] `go build ./...`
- [x] `go vet ./...`

### T5.2 Tag release
- [x] `git tag -a muxcore/v0.19.3 -m <release notes from plan.md>` pointing at master HEAD
- [x] `git push origin muxcore/v0.19.3`

### T5.3 GitHub release
- [x] `gh release create muxcore/v0.19.3 --title "muxcore v0.19.3 — concurrency correctness" --notes <full notes>`

### T5.4 Docs bump PR
- [x] `git worktree add ../mcp-mux-wt/docs-v0.19.3 -b docs/bump-v0.19.3`
- [x] Edit AGENTS.md: bump muxcore v0.19.2 → v0.19.3 with summary paragraph
- [x] Commit, push, gh pr create, gh pr merge --squash
- [x] Cleanup worktree and branches

### T5.5 Deploy
- [x] `go build -o D:\Dev\mcp-mux\mcp-mux.exe~ ./cmd/mcp-mux`
- [x] `./mcp-mux.exe upgrade --restart`
- [x] Verify: `./mcp-mux.exe status` shows all previous owners reconnected

### T5.6 Post-release verification
- [x] `/nvmd-platform:production-ready-check` — expect WTF-points drop from 92 → ≤20
- [x] Verify Known Follow-ups section in CONTINUITY.md is empty (except any v0.19.4/v0.19.5 deferred items)

### T5.7 Update CONTINUITY.md
- [x] Move v0.19.3 from Known Follow-ups to Releases table
- [x] Update "If You Remember 3 Things" to reflect post-v0.19.3 state
- [x] Add v0.19.4 Unix portability scope as new Known Follow-up

---

## Validation Gate

Before any task is marked complete, verify:
- [x] All code changes compile (`go build`)
- [x] All code changes vet-clean (`go vet`)
- [x] Regression test for the FR fails without the fix and passes with it
- [x] No regressions in full test suite
- [x] Commit message references the FR IDs from spec.md
- [x] File re-read post-Edit to confirm the change applied (NFR-6)

## Out of Tasks

- FR-10..FR-27 — deferred to v0.19.4 / v0.19.5 plans (not in this tasks.md)
- **FR-9 superseded by FR-29 (see Amendment below).** FR-9 was deferred and never implemented; FR-29 absorbs its scope and expands it to 4 socket call sites with build-tag separation.

---

## Amendment: 2026-04-18 — PR-E (Multi-user hardening FR-28 + FR-29)

**Spec:** `.agent/specs/post-audit-remediation/spec.md` (Amendment section)
**Target release:** `muxcore/v0.20.4` + `mcp-mux/v0.9.10`
**Worktree:** `../mcp-mux-wt/multi-user-hardening` (branch: `feat/multi-user-hardening`)
**Reviewer:** `nvmd-platform:pr-reviewer` background sonnet agent

### T6.1 Create worktree (Setup)
- [ ] `git worktree add ../mcp-mux-wt/multi-user-hardening -b feat/multi-user-hardening`
  AC: worktree exists at path · branch `feat/multi-user-hardening` checked out · master unmodified · `git status` clean in both locations · swap body→return null ⇒ N/A (shell command)
  [EXECUTOR: MAIN]

### T6.2 FR-28: SessionManager.IsPreRegistered (exported muxcore API)
- [ ] Read `muxcore/owner/session_manager.go` (or equivalent) to locate `PreRegister` + `Bind` impl
- [ ] Add exported function `(sm *SessionManager) IsPreRegistered(token string) bool` — returns `true` iff token exists in the pre-registered map, without mutating it
- [ ] Method MUST hold `sm.mu.RLock()` / `RUnlock()` (read-only) for safe concurrent access
- [ ] Add method doc comment explaining side-effect-free semantics and the FR-28 use case
- [ ] Verify `go build ./muxcore/...`
  AC: `grep -q "func.*IsPreRegistered" muxcore/owner/session_manager.go` · method exported (leading capital I) · holds RLock not Lock · godoc describes "without consuming" semantic · swap body→return `false` ⇒ FR-28 tests MUST fail
  [EXECUTOR: sonnet]

### T6.3 FR-28: Rejection log with rate-limit (per C1 + C4)
- [ ] Implement package-level rate-limiter in `muxcore/owner/` — struct `rejectionLogger` with `timestamps [10]time.Time` ring buffer + `mu sync.Mutex`
- [ ] Method `(rl *rejectionLogger) Log(logger *log.Logger, pid int)` — checks whether ≥10 entries within last 60s; if under cap: emit `accept: rejected connection from pid=%d (invalid/missing token)`; if at cap: no per-event log, increment suppressed counter
- [ ] Background ticker (60s) emits summary `accept: rate-limited: %d rejections suppressed in last 60s` when suppressed > 0, then zeroes counter
- [ ] Unit test: 15 rapid rejections → expect exactly 10 per-event logs + 1 summary line
  AC: ring buffer size 10 exactly · Lock held for all mutations · timestamps correctly shifted · summary only when suppressed > 0 · swap body→no-op ⇒ unit test MUST fail
  [EXECUTOR: sonnet]

### T6.4 FR-28: acceptLoop rejection gate
- [ ] Read `muxcore/owner/owner.go:1607-1621` (current `acceptLoop` token-handling block)
- [ ] After `token, reader = readToken(conn)` succeeds, insert check:
  ```go
  if o.tokenHandshake {
      if token == "" || !o.sessionMgr.IsPreRegistered(token) {
          peerPID := readPeerPID(conn) // platform-specific, returns -1 on Windows
          o.rejectionLogger.Log(o.logger, peerPID)
          conn.Close()
          continue
      }
  }
  ```
- [ ] Implement `readPeerPID(conn net.Conn) int` — Unix via `SO_PEERCRED`, Windows stub returns `-1`. Use build tags (`peer_pid_unix.go` + `peer_pid_windows.go`)
- [ ] FR-28 does NOT activate when `o.tokenHandshake == false` (legacy engine-consumer mode preserved — NFR-5)
- [ ] Verify `go build ./muxcore/owner/...` and `go vet ./muxcore/owner/...`
  AC: rejection path does NOT call `sessionMgr.Bind` (token preserved per C2) · rejection path does NOT call `o.AddSession` (session state unchanged) · `conn.Close` always called on rejection · `continue` always follows · swap rejection body → empty ⇒ FR-28 acceptance tests MUST fail
  [EXECUTOR: sonnet]

### T6.5 FR-28: Regression tests
- [ ] Create `muxcore/owner/accept_loop_test.go` — table-driven test exercising a real `net.Listen("unix", tempPath)` + goroutine `acceptLoop`
- [ ] Case A: empty token handshake → `conn.Close` within 100ms, `len(o.sessions) == 0` unchanged
- [ ] Case B: random 32-byte hex unregistered token → same result as A
- [ ] Case C: pre-registered token via `sessionMgr.PreRegister` → connection accepted, session added, token consumed (second Bind attempt fails)
- [ ] Case D: concurrent 10 valid + 10 invalid connects under `-race` → exactly 10 accepts, exactly 10 rejects, no panic
- [ ] Rate-limit test: 15 rapid invalid connects → assert log output has 10 per-event + 1 summary via captured `bytes.Buffer` logger
- [ ] Run: `go test -count=1 -race -run 'TestAcceptLoop' ./muxcore/owner/...`
  AC: 4 table rows pass · race test passes with `-race` · rate-limit test asserts exact counts · all cleanup `os.Remove(tempPath)` · swap FR-28 impl body → accept all ⇒ cases A,B,D MUST fail
  [EXECUTOR: sonnet]

### T6.6 FR-29: Hardened socket-listen wrapper
- [ ] Create `muxcore/sockperm/sockperm.go` (package `sockperm`) — exported `Listen(network, addr string) (net.Listener, error)`
- [ ] `sockperm.Listen` delegates to `listenWithMode(network, addr, 0600)` from build-tag files
- [ ] Create `muxcore/sockperm/sockperm_unix.go` (`//go:build unix`) — uses package-level `sync.Mutex` + `syscall.Umask(0177)`-wrapped `net.Listen`, restores original umask on defer
- [ ] Create `muxcore/sockperm/sockperm_windows.go` (`//go:build windows`) — no-op wrapper, delegates directly to `net.Listen`. **MANDATORY** package-level doc comment per C5 citing Windows 10 1803+ AF_UNIX default DACL behavior.
- [ ] Rationale in Unix file comment: "syscall.Umask is process-global. The mutex is required — two goroutines calling Listen concurrently could race: G1 sets umask(0177), G2 sets umask(0177), G1 restores original, G2 creates socket with wrong umask."
- [ ] Verify `go build ./muxcore/...`
  AC: package compiles on Unix + Windows · build tags resolve correctly (`go list -f '{{.GoFiles}}' ./muxcore/sockperm`) · Unix file has mutex · Windows file has doc comment citing C5 text verbatim · swap sockperm.Listen → plain net.Listen ⇒ FR-29 perm tests MUST fail
  [EXECUTOR: sonnet]

### T6.7 FR-29: Migrate 4 call sites
- [ ] `muxcore/serverid/serverid.go:188` — replace `net.Listen("unix", path)` with `sockperm.Listen("unix", path)`
- [ ] `muxcore/serverid/serverid.go:195` — same
- [ ] `cmd/mcp-mux/daemon.go:67` — same (daemon control socket)
- [ ] `muxcore/ipc/transport.go:25` — same (engine-consumer IPC — absorbs FR-9)
- [ ] Add import `"github.com/thebtf/mcp-mux/muxcore/sockperm"` to each file (or use `muxcore.Listen` if preferred public re-export — decide in implementation)
- [ ] Verify `go build ./...` root module + `go build ./...` muxcore submodule
- [ ] `go vet ./...` both modules
  AC: 4 call sites migrated · `grep -rn "net.Listen(\"unix\"" muxcore/ cmd/` returns 0 for these 4 paths · all builds pass · swap sockperm.Listen → net.Listen in any one file ⇒ that site's perm test MUST fail
  [EXECUTOR: sonnet]

### T6.8 FR-29: Regression tests (unix-only)
- [ ] Create `muxcore/sockperm/sockperm_unix_test.go` (`//go:build unix`)
- [ ] Case A: single `sockperm.Listen("unix", tempPath)` → `os.Stat(tempPath)`, assert `info.Mode() & 0777 == 0600`
- [ ] Case B: 50 concurrent goroutines, each calls `sockperm.Listen` on its own temp path → all 50 files `mode == 0600` (proves mutex serializes)
- [ ] Case C: restored-umask test — wrap-then-direct-call: call `sockperm.Listen`, then `net.Listen("unix", other)` → assert `other` has a "normal" mode (NOT 0600), proving the umask was restored
- [ ] Cleanup: `os.Remove(path)` after each subtest; use `t.TempDir()` for the base path
- [ ] Run: `go test -count=1 -race -run 'TestSockperm' ./muxcore/sockperm/...` (on WSL if native Windows)
  AC: 3 test cases · race test passes with `-race` (Unix only) · skipped on Windows via build tag · swap umask(0177) → umask(0) ⇒ case A MUST fail
  [EXECUTOR: sonnet]

### T6.9 /code-review lite on PR-E diff (MANDATORY pre-commit gate)
- [ ] Run `Skill("code-review", "lite")` on the worktree diff
- [ ] Fix every finding — CRITICAL, HIGH, MEDIUM, LOW. No "non-blocking".
- [ ] Re-run code-review until clean
  AC: review output contains "no findings" or all findings marked resolved · no Edit calls outstanding · swap this gate → skip ⇒ would permit regressions
  [EXECUTOR: MAIN]

### T6.10 PR-E commit, push, invoke review
- [ ] `go test -count=1 -race ./...` both modules green
- [ ] `go build ./...` + `go vet ./...` clean both modules
- [ ] Commit format: `feat(multi-user-hardening): FR-28 token enforcement + FR-29 socket 0600 perms`
- [ ] Push branch; `gh pr create` with body referencing spec Amendment section + C1-C5 resolutions
- [ ] `pr_invoke { agent: "all" }` to trigger AI reviewers
- [ ] `pr_await_reviews` in polling loop; process every comment via workers; resolve all threads
  AC: PR open on GitHub · AI review threads all resolved · build+tests green in CI (3 OS matrix) · swap any fix → revert ⇒ CI MUST fail
  [EXECUTOR: MAIN]

### T6.11 PR-E merge
- [ ] After all reviews resolved + CI green, squash-merge via `gh pr merge --squash`
- [ ] Delete worktree: `git worktree remove ../mcp-mux-wt/multi-user-hardening`
- [ ] Verify master contains the merge commit (`git log --oneline -1`)
  AC: PR merged · worktree removed · branch deleted on remote · master HEAD is the merge commit · swap merge → keep open ⇒ release gate T7.x blocked
  [EXECUTOR: MAIN]

### Validation Gate (PR-E)
Before PR-E is marked complete, verify:
- [ ] FR-28 + FR-29 both implemented (not just one)
- [ ] 4 socket call sites migrated (none missed)
- [ ] Build-tag separation working (`go vet` on both platforms)
- [ ] All regression tests pass under `-race`
- [ ] Rejection log does NOT contain token values (grep test output)
- [ ] Code-review lite clean before commit
- [ ] AI PR review threads all resolved
- [ ] No existing tests regressed

---

## Amendment Release: muxcore/v0.20.4 + mcp-mux/v0.9.10

### T7.1 Tag muxcore/v0.20.4
- [ ] `cd muxcore && git tag muxcore/v0.20.4 && git push origin muxcore/v0.20.4`
- [ ] Write release notes at `.agent/data/release-notes-muxcore-v0.20.4.md` — mirror v0.20.3 style (table + consumer notes + full audit trail)
  AC: tag exists on remote · release notes file created · notes reference FR-28 + FR-29 + S8-001 + S5-001 · swap tag → missing ⇒ T7.2 blocked
  [EXECUTOR: MAIN]

### T7.2 Bump muxcore dep + tag mcp-mux/v0.9.10
- [ ] In root module: `go get github.com/thebtf/mcp-mux/muxcore@v0.20.4 && go mod tidy`
- [ ] Update AGENTS.md muxcore version reference to v0.20.4
- [ ] Commit: `chore: bump muxcore to v0.20.4 (FR-28 + FR-29)`
- [ ] `git tag v0.9.10 && git push origin v0.9.10`
- [ ] Write release notes at `.agent/data/release-notes-v0.9.10.md`
- [ ] `gh release create v0.9.10 -F .agent/data/release-notes-v0.9.10.md`
  AC: go.mod updated to v0.20.4 · AGENTS.md updated · v0.9.10 tag pushed · GitHub release published · swap dep version → old ⇒ `go build` MUST fail against FR-28 API
  [EXECUTOR: MAIN]

### T7.3 Deploy + verify (critical — do NOT skip)
- [ ] Build `mcp-mux.exe~` locally
- [ ] `mcp-mux upgrade --restart` (per feedback_deploy_procedure.md — never manual rename)
- [ ] Post-restart: `mux_list` shows identical session count vs pre-deploy
- [ ] 30-min window: daemon log has ZERO supervisor-storm events (same measurement as v0.9.8/v0.9.9)
- [ ] Re-run security scan on deployed binary: S8-001 + S5-001 both closed
- [ ] Run `/nvmd-platform:production-ready-check --quick` → verdict MUST be READY (not CONDITIONALLY) for multi-user deployment
  AC: upgrade succeeds without user input · mux_list delta = 0 · storm counter = 0 for 30 min · security re-scan: 0 HIGH findings · PRC verdict = READY · swap upgrade → manual copy ⇒ deploy verification MUST fail
  [EXECUTOR: MAIN]

### T7.4 Update CONTINUITY.md + close spec
- [ ] Update `.agent/CONTINUITY.md` — mark Amendment PR-E + release v0.9.10 as shipped
- [ ] Mark spec `Status: Active` → `Status: Implemented` in `.agent/specs/post-audit-remediation/spec.md` frontmatter
- [ ] Close engram issues #84 (aimux), #87 (engram) once upstream projects bump to muxcore/v0.20.4
  AC: CONTINUITY.md has 2026-04-18 release entry · spec.md Status = Implemented · engram issues closed · swap Status → Draft ⇒ /nvmd-specify AMEND would re-open
  [EXECUTOR: MAIN]

### Out of Tasks (Amendment)

- **Multi-user trust boundary documentation** — README section explaining "mcp-mux is safe for multi-user Unix with v0.9.10+" deferred to docs-refresh sprint, not blocking release.
- **FR-10 snapshot HMAC signing** — still deferred to v0.20.5 hardening spec (too big for this amendment).
- **cclsp v0.0.104 stability follow-up** — tracked separately in engram #79; orthogonal to this amendment.
