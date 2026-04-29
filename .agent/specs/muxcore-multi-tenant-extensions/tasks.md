# Tasks — muxcore Multi-Tenant Extensions

**Slug:** muxcore-multi-tenant-extensions
**Spec:** spec.md · **Plan:** plan.md · **Architecture:** architecture.md
**Target:** muxcore/v0.24.0 · **Closes:** #109, #110, #111, #112
**Mode:** TDD by default (RED → GREEN → REFACTOR per task; tests precede implementation within each phase)
**Output path:** legacy fallback (slug-only, no `open_crs` in spec frontmatter)

> **Phase 0 Value-Sequence Planner: PHASE_0_COMPLETE.** Phases ordered by user-story priority (P1 stories US1+US2 in Phase 1+2, P2 story US3 in Phase 3). Each phase ends with a GATE task. Each phase Independent Test states what becomes user-observable.

## Dependencies

- Phase 1 (G001) → Phase 2 + Phase 3 (Phase 1 establishes ConnInfo/SessionMeta types; both later phases consume them)
- Phase 2 (G002) → Phase 4 release (deny path is part of v0.24 surface)
- Phase 3 (G003) → Phase 4 release (frame hook is part of v0.24 surface)
- Phase 4 (G004) — final release gate

Phase 2 and Phase 3 can run **partially in parallel** after T-2.1/T-3.1 type definitions land — the implementations touch different functions in `owner.go` (acceptLoop region vs handleDownstreamMessage region).

---

## Phase 1 — Foundation (US1, P1)

**User-value anchor:** When this phase is done, an aimux SessionHandler implementing `NotificationHandlerWithSessionMeta` receives a verifiable peer PID for every notification (`meta.Conn.PeerPid == os.Getpid()` of the connecting shim) on Linux, Windows, and Darwin.

**Independent Test:** Build a minimal SessionHandler that implements both legacy `HandleRequest` and new `HandleRequestWithSessionMeta`. Spawn a test client at known PID; connect; send one tool/list request. Assert: (a) handler called via WithSessionMeta path; (b) `meta.Conn.PeerPid == client.PID`; (c) `meta.Conn.Platform` matches OS; (d) handler NOT called via legacy path. PASS = all four assertions hold on all 3 CI runners.

**Checkpoint:** Phase 1 complete when G001 GATE passes.

### Task T001 — Define `ConnInfo` value type

**File:** `muxcore/connection_info.go` (NEW)
**Description:** Create `ConnInfo` struct with `PeerPid int`, `PeerUid int`, `Platform string` — OS facts only (no consumer-policy fields per ADR-004 revised). Add comprehensive godoc explaining zero-value semantics ("zero PeerPid/PeerUid valid runtime state, indicates extraction failure or unsupported transport"). Define stable Platform identifier constants: `PlatformLinuxUnix`, `PlatformWindowsNamedPipe`, `PlatformDarwinUnix`, `PlatformUnknown`.
**AC:** File compiles standalone; `go vet ./muxcore` clean; godoc renders without warnings; existing tests still pass.

### Task T002 — Define `SessionMeta` value type

**File:** `muxcore/session_meta.go` (NEW)
**Description:** Create `SessionMeta` struct with embedded `Conn ConnInfo` (by value), `TenantID string`, `AuthorizedAt time.Time`. Add `IsAuthorized() bool` helper returning `!sm.AuthorizedAt.IsZero()`. Document discriminator semantics: empty TenantID + non-zero AuthorizedAt = authorized without tenant (FR-3 amendment CHK013); empty TenantID + zero AuthorizedAt = AuthorizeSession not configured.
**AC:** File compiles; `IsAuthorized()` returns false for zero-value SessionMeta and true for `SessionMeta{AuthorizedAt: time.Now()}`; godoc explicit about discriminator.
**Depends on:** T001

### Task T003 [P] — Real Windows peer-PID via `Fd()` interface assertion

**File:** `muxcore/owner/peer_pid_windows.go` (REWRITE — currently returns -1)
**Description:** Replace stub with real implementation. Define `interface{ Fd() uintptr }` locally. Type-assert on `net.Conn`; if asserted, call `windows.GetNamedPipeClientProcessId(windows.Handle(g.Fd()), &pid)`. Try `windows.GetNamedPipeClientProcessId` from `golang.org/x/sys/windows` first; if that import fails (function not exposed), declare via `windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeClientProcessId")` (CHK022/C2). Return -1 on assertion failure or syscall error (preserve existing rejectionLogger contract).
**AC:** Loopback test: connect winio client at known PID → `readPeerPID` returns that PID. CI passes on Windows runner.
**PARALLEL-WITH:** T004, T005, T006, T007 — verified no file overlap (5 distinct build-tagged files).
**IF-WRONG:** assertion fails on real winio Conn (winio API change) → fall back to `unsafe.Pointer + reflect` getter on private `win32File.handle` field (EC-9 contingency); if that also fails, return -1 + log marker `peer_creds_no_fd platform=windows-named-pipe`. Defer real fix to follow-up arc.

### Task T004 [P] — Real Darwin peer-PID via `LOCAL_PEERPID`

**File:** `muxcore/owner/peer_pid_darwin.go` (REWRITE)
**Description:** Replace stub. Use `unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)` via `SyscallConn().Control()` on `*net.UnixConn`. Return -1 on type-assertion failure or syscall error.
**AC:** Loopback test: Unix-socket client at known PID → `readPeerPID` returns PID. CI passes on Darwin runner.
**PARALLEL-WITH:** T003, T005, T006, T007.

### Task T005 [P] — Linux peer-UID extraction (extend ucred)

**File:** `muxcore/owner/peer_uid_linux.go` (NEW), modify `peer_pid_linux.go` to extract UID via same `ucred` call (no second syscall — ucred already returns Pid+Uid+Gid).
**Description:** Add `readPeerUID(conn net.Conn) int` returning `ucred.Uid` from existing SO_PEERCRED path. Refactor `peer_pid_linux.go` if needed to share ucred extraction without breaking existing `readPeerPID` API.
**AC:** Loopback test: client UID read matches `os.Getuid()`. Existing `readPeerPID` tests still pass.
**PARALLEL-WITH:** T003, T004, T006, T007.

### Task T006 [P] — Darwin peer-UID via `getpeereid`

**File:** `muxcore/owner/peer_uid_darwin.go` (NEW)
**Description:** `readPeerUID(conn net.Conn) int` using `unix.Getpeereid(fd)`. Return -1 on error.
**AC:** Loopback test: UID matches `os.Getuid()` on Darwin runner.
**PARALLEL-WITH:** T003, T004, T005, T007.

### Task T007 [P] — Windows peer-UID stub

**File:** `muxcore/owner/peer_uid_windows.go` (NEW)
**Description:** `readPeerUID(conn net.Conn) int { return 0 }`. Document that Windows has no UID concept comparable to Unix; consumer policy uses PID + Token-based identity instead. Future enhancement candidate (out of v0.24 scope).
**AC:** File compiles on Windows runner.
**PARALLEL-WITH:** T003, T004, T005, T006.

### Task T008 — Shared `peerCreds` dispatcher

**File:** `muxcore/owner/peer_creds.go` (NEW, no build tag)
**Description:** Define `peerCreds(conn net.Conn) muxcore.ConnInfo` that calls platform-specific `readPeerPID(conn)` + `readPeerUID(conn)` (resolved via build tags) and returns `ConnInfo{PeerPid, PeerUid, Platform: <platform-const>}`. Platform constant resolved via build-tagged const file (`peer_platform_linux.go` etc., one-line each).
**AC:** Cross-platform compile (`go build ./muxcore/owner/...`); function returns ConnInfo with non-zero Platform on all 3 OSes.
**Depends on:** T001, T003, T004, T005, T006, T007

### Task T009 — Add handler interface upgrades

**File:** `muxcore/handler.go` (MODIFY)
**Description:** Add two new optional interfaces below existing `NotificationHandler` declaration:
```go
type NotificationHandlerWithSessionMeta interface {
    NotificationHandler
    HandleNotificationWithSessionMeta(ctx context.Context, project ProjectContext, meta SessionMeta, notification []byte)
}

type SessionHandlerWithSessionMeta interface {
    HandleRequestWithSessionMeta(ctx context.Context, project ProjectContext, meta SessionMeta, request []byte) (response []byte, err error)
}
```
Add comprehensive godoc per FR-1/FR-2 — explaining: (a) optional upgrade pattern; (b) handler implementing both legacy and WithSessionMeta sees only WithSessionMeta dispatch (EC-7); (c) SessionMeta same value across requests + notifications for same session.
**AC:** File compiles; existing handler tests still pass; interface assertion `var _ SessionHandlerWithSessionMeta = (*mockSessionHandlerExtended)(nil)` works in test scaffolding.
**Depends on:** T002

### Task T010 — Add `Session.meta` cached field

**File:** `muxcore/owner/owner.go` (MODIFY — Session struct)
**Description:** Add `meta muxcore.SessionMeta` field to Session struct. Document: populated in acceptLoop after Bind succeeds; mutated once on AuthAllow (Phase 2); immutable after first read by any dispatch goroutine.
**AC:** Session struct compiles; existing tests pass; `s.meta` reads zero-value `SessionMeta` until populated.
**Depends on:** T002

### Task T011 — Wire ConnInfo population in acceptLoop

**File:** `muxcore/owner/owner.go::acceptLoop` (MODIFY)
**Description:** After `sessionMgr.Bind(token, ownerKey, s)` succeeds, set `s.meta = muxcore.SessionMeta{Conn: peerCreds(conn)}`. Position: between successful Bind and `addSession` call. Emit log marker `peer_creds_extracted sid=N pid=P uid=U platform=X` at INFO level.
**AC:** acceptLoop tests still pass; new log marker visible in test output when SessionHandler is wired.
**Depends on:** T008, T010

### Task T012 — Type-assert WithSessionMeta in dispatchToSessionHandler

**File:** `muxcore/owner/owner.go::dispatchToSessionHandler` (MODIFY)
**Description:** Replace existing `o.sessionHandler.HandleRequest(...)` call with type-assertion preference:
```go
if h, ok := o.sessionHandler.(muxcore.SessionHandlerWithSessionMeta); ok {
    resp, err = h.HandleRequestWithSessionMeta(ctx, project, s.meta, msg.Raw)
} else {
    resp, err = o.sessionHandler.HandleRequest(ctx, project, msg.Raw)
}
```
Same pattern in `handleDownstreamMessage` notification branch for `NotificationHandlerWithSessionMeta`.
**AC:** Existing dispatch_test.go tests pass unchanged (legacy path); new test (T013) covers WithSessionMeta path.
**Depends on:** T009, T010, T011

### Task T013 [P] — Cross-platform peer-creds tests

**File:** `muxcore/owner/peer_creds_linux_test.go` + `peer_creds_windows_test.go` + `peer_creds_darwin_test.go` (NEW, one per platform)
**Description:** Each file contains a single test function `TestPeerCreds_LoopbackPID_<Platform>` that: (a) starts platform-appropriate listener (UDS / winio); (b) spawns a client process at known PID; (c) on accept, calls `peerCreds(conn)`; (d) asserts `ConnInfo.PeerPid == client.PID`, `ConnInfo.Platform == "expected-platform-string"`, `ConnInfo.PeerUid` matches expectations (non-zero on Unix, zero on Windows).
**AC:** All 3 tests run on respective CI runners; each asserts non-zero PID matching client.
**PARALLEL-WITH:** Each test file is independent — verified no file overlap (3 distinct build-tagged files).
**Depends on:** T008

### Task T014 — Dispatch precedence test

**File:** `muxcore/owner/dispatch_test.go` (MODIFY — add new test)
**Description:** Add `TestDispatch_WithSessionMetaPreferredOverLegacy`. Build `mockSessionHandlerBoth` implementing both `HandleRequest` and `HandleRequestWithSessionMeta`. Send one request. Assert: (a) only `HandleRequestWithSessionMeta` was called (call counter); (b) legacy `HandleRequest` was NOT called.
**AC:** Test fails before T012 lands; passes after.
**Depends on:** T009, T012

### Task T015 — Backward-compat regression test

**File:** `muxcore/owner/dispatch_test.go` (MODIFY)
**Description:** Add `TestDispatch_LegacyHandlerByteIdentical_v23`. Use existing `mockSessionHandler` (which implements ONLY legacy `HandleRequest`). Send 5 representative requests. Assert: (a) `HandleRequest` called 5 times with `(ctx, project, raw)` signature unchanged; (b) `s.meta` was populated regardless (cached but unused); (c) responses match v0.23.0 fixture byte-for-byte.
**AC:** Test passes — no behavior drift for legacy handlers.
**Depends on:** T011, T012

### GATE Task G001 — Phase 1 verification

**Description:** Phase 1 complete: ConnInfo + SessionMeta types defined, peer-creds working on 3 platforms, *WithSessionMeta interfaces wired, dispatch upgrade applied with backward compat preserved.
**RUN:**
```bash
go test ./muxcore/... -race -run 'TestPeerCreds|TestDispatch|TestSessionMeta'
go vet ./muxcore/...
golangci-lint run ./muxcore/... # if available
```
**CHECK:**
- All listed tests pass on Linux runner
- Windows + Darwin CI matrix runs green (T013 platform-specific tests pass)
- `go vet` clean across all muxcore packages
- Existing test suite (muxcore + muxcore/owner) passes — no regression
**ENFORCE:** PHASE 1 BLOCKED if any check fails. Do NOT proceed to Phase 2.
**RESOLVE:** If failures: `mcp__aimux__investigate(action="start", topic="phase 1 gate failure: <error>")`. Apply fix. Re-run gate.
**SAVE:** Test output captured to `.agent/reports/2026-04-29-phase1-gate.txt`. Engram store on PASS.
**Depends on:** T001, T002, T003, T004, T005, T006, T007, T008, T009, T010, T011, T012, T013, T014, T015

---

## Phase 2 — AuthorizeSession (US2, P1)

**User-value anchor:** When this phase is done, an aimux operator wiring `engine.Config.AuthorizeSession` can deny unenrolled peers at handshake; the denied client receives JSON-RPC -32000 with the consumer-supplied reason and the connection closes without spawning upstream or registering the session.

**Independent Test:** Build aimux-style SessionHandler. Wire `AuthorizeSession` callback that returns `AuthDeny{Reason: "test-deny"}` for any connection. Spawn client; connect. Assert: (a) client receives `{"jsonrpc":"2.0","error":{"code":-32000,"message":"test-deny"}}`; (b) connection closed (read returns EOF/closed); (c) `mux_list` does NOT show new session for that peer; (d) no upstream process spawned (assert by inspecting Owner state). Wire callback returning `AuthAllow{TenantID:"t1"}`; connect again; assert handler dispatch sees `meta.TenantID == "t1"`.

**Checkpoint:** Phase 2 complete when G002 GATE passes.

### Task T016 — Define `AuthDecision` + `SessionAuth` types

**File:** `muxcore/session_auth.go` (NEW)
**Description:** Define `AuthDecision` int type with `AuthAllow` (=0) and `AuthDeny` (=1) consts. Define `SessionAuth` struct with `Decision AuthDecision`, `TenantID string`, `Reason string`. Document: TenantID ignored when Decision == AuthDeny; Reason surfaced in JSON-RPC error message on AuthDeny; AuthAllow with empty TenantID is legitimate (FR-3 amendment CHK013).
**AC:** File compiles; consts numerically ordered (Allow=0, Deny=1); godoc clear about empty-TenantID semantics.

### Task T017 — Add `AuthorizeSession` field to `engine.Config`

**File:** `muxcore/engine/config.go` (or `engine.go` — wherever Config struct lives) (MODIFY)
**Description:** Add `AuthorizeSession func(ctx context.Context, conn muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth` to Config struct. Comprehensive godoc per FR-3: when nil = no gate (preserves v0.23 behavior); when non-nil = single-shot per session, fires after Bind before AddSession; AuthDeny path closes connection; AuthAllow constructs SessionMeta with TenantID + AuthorizedAt.
**AC:** Config compiles; existing engine tests pass; nil-default preserves byte-identical behavior.
**Depends on:** T001, T016

### Task T018 — Wire AuthorizeSession in acceptLoop

**File:** `muxcore/owner/owner.go::acceptLoop` (MODIFY)
**Description:** After `s.meta = SessionMeta{Conn: peerCreds(conn)}` (T011), check if `o.cfg.AuthorizeSession != nil` (Owner needs access to engine.Config callback — pass via NewOwner constructor or shared field). If non-nil, invoke callback with `defer recover()` panic guard. On panic: treat as AuthDeny{Reason: "authorize panic"} + log marker `auth_panic sid=N`. Switch on Decision:
- AuthDeny: write `buildJSONRPCErrorBytes(nil, -32000, verdict.Reason)` to conn, close conn, log `auth_deny sid=N reason=X`, RETURN (no AddSession, no upstream)
- AuthAllow: set `s.meta.TenantID = verdict.TenantID; s.meta.AuthorizedAt = time.Now()`, log `auth_allow sid=N tenant=X`, FALL THROUGH to AddSession
**Shutdown handling (CHK015 / EC-11):** if `<-o.done` fires while callback in flight, let callback complete, then close conn regardless of verdict (no AddSession post-shutdown).
**AC:** Wired into acceptLoop; existing tests still pass (cfg.AuthorizeSession == nil path unchanged); new tests T020-T024 will cover the callback path.
**Depends on:** T011, T017
**IF-WRONG:** AuthorizeSession callback ABI redesign requested by aimux during PR review (e.g., consumer wants to receive SessionMeta instead of ConnInfo) → revisit ADR-003 in clarify-amend mode (G13 amend, NOT new spec). Cost: 1 CR, ~30 LOC, callback signature change.

### Task T019 — Implement deny-path JSON-RPC error response

**File:** `muxcore/owner/owner.go` (MODIFY — within acceptLoop deny branch from T018)
**Description:** Use existing `buildJSONRPCErrorBytes(id json.RawMessage, code int, message string) ([]byte, error)` helper (already used for handler-panic path in `dispatchToSessionHandler`). Pass `nil` for id (response not tied to specific request — handshake-level rejection). Write via `s.WriteRaw` or direct `conn.Write`. Use the same approach as existing rejection logger emits.
**AC:** Deny response is valid JSON-RPC; client decoder parses `{"jsonrpc":"2.0","error":{"code":-32000,"message":"<reason>"}}`.
**Depends on:** T018

### Task T020 — Test: AuthorizeSession returns AuthDeny

**File:** `muxcore/owner/authorize_test.go` (NEW)
**Description:** `TestAuthorize_DenyClosesConnection`. Build minimal Owner with engine.Config.AuthorizeSession returning `AuthDeny{Reason:"test-deny"}`. Connect test client. Assert: (a) client reads JSON-RPC -32000 response; (b) connection closes (read returns io.EOF or use-of-closed); (c) `o.sessions` does NOT contain new session; (d) upstream spawn count remains 0.
**AC:** Test fails before T018; passes after.
**Depends on:** T018, T019

### Task T021 — Test: AuthorizeSession returns AuthAllow

**File:** `muxcore/owner/authorize_test.go` (MODIFY)
**Description:** `TestAuthorize_AllowPopulatesSessionMeta`. Build SessionHandler implementing `HandleRequestWithSessionMeta`. Wire AuthorizeSession returning `AuthAllow{TenantID:"frontend-team"}`. Connect; send tool/list. Assert: (a) handler called with `meta.TenantID == "frontend-team"`; (b) `!meta.AuthorizedAt.IsZero()`; (c) `meta.Conn.PeerPid != 0` (existing creds populated).
**AC:** Test passes; verifies single-shot mutation cached correctly.
**Depends on:** T018, T021_dep_T012

### Task T022 — Test: AuthorizeSession nil = byte-identical to v0.23

**File:** `muxcore/owner/authorize_test.go` (MODIFY)
**Description:** `TestAuthorize_NilConfigUnchangedDispatch`. cfg.AuthorizeSession = nil. Connect 5 sessions; verify accept-loop, dispatch, response timing match v0.23 baseline (capture v0.23 reference fixture if needed; otherwise assert structural equivalence — same number of log markers, same dispatch counts).
**AC:** No drift from baseline.
**Depends on:** T018

### Task T023 — Test: AuthorizeSession panic recovery

**File:** `muxcore/owner/authorize_test.go` (MODIFY)
**Description:** `TestAuthorize_PanicTreatedAsDeny`. Wire callback that panics via `panic("test-panic")`. Connect client. Assert: (a) client receives -32000 with reason "authorize panic"; (b) log marker `auth_panic sid=N` emitted; (c) accept-loop survives — subsequent connections succeed.
**AC:** Panic does not crash daemon; treated as AuthDeny.
**Depends on:** T018

### Task T024 — Test: AuthorizeSession AuthAllow with empty TenantID (CHK013 / EC-12)

**File:** `muxcore/owner/authorize_test.go` (MODIFY)
**Description:** `TestAuthorize_AllowEmptyTenantIdLegitimate`. Wire callback returning `AuthAllow{TenantID:""}`. Connect; dispatch request. Assert: (a) handler dispatched (not denied); (b) `meta.TenantID == ""` AND `!meta.AuthorizedAt.IsZero()` — discriminator distinguishes "allowed without tenant" from "AuthorizeSession not configured".
**AC:** Empty TenantID is valid AuthAllow result.
**Depends on:** T018, T021_dep_T012

### Task T025 — Test: AuthorizeSession deny does NOT spawn upstream

**File:** `muxcore/owner/authorize_test.go` (MODIFY)
**Description:** `TestAuthorize_DenyNoUpstreamSpawn`. Wire AuthorizeSession returning AuthDeny. Capture pre-connection upstream PID list (via `o.upstream` inspection). Connect denied client. Assert: post-connection upstream PID list IDENTICAL — no new process spawned, no resource allocated.
**AC:** Deny path zero-cost on resources.
**Depends on:** T018

### GATE Task G002 — Phase 2 verification

**RUN:** `go test ./muxcore/owner/... -race -run 'TestAuthorize'`
**CHECK:**
- T020 (deny closes conn) PASS
- T021 (allow populates meta) PASS
- T022 (nil unchanged) PASS
- T023 (panic recovery) PASS
- T024 (empty TenantID legitimate) PASS
- T025 (deny no upstream) PASS
- Phase 1 G001 still PASS (regression check)
**ENFORCE:** PHASE 2 BLOCKED if any check fails.
**RESOLVE:** investigate, fix, re-run.
**SAVE:** `.agent/reports/2026-04-29-phase2-gate.txt`. Engram store on PASS.
**Depends on:** T016, T017, T018, T019, T020, T021, T022, T023, T024, T025

---

## Phase 3 — OnFrameReceived (US3, P2)

**User-value anchor:** When this phase is done, an aimux operator wiring `engine.Config.OnFrameReceived` to a per-tenant token bucket can drop hostile-tenant frames before dispatch — `FrameDrop` results in zero invocations of any SessionHandler method, zero queue space consumed beyond the read buffer.

**Independent Test:** Build SessionHandler counting dispatch invocations. Wire OnFrameReceived returning `FrameDrop` for first 100 frames, then `FramePass` thereafter. Send 200 frames. Assert: (a) dispatch counter == 100 (last 100 only); (b) frames 1-100 produced no dispatch (first 100 dropped silently); (c) no client received -32004 errors. Switch callback to `FrameError` for next batch — assert client receives -32004 + dispatch counter unchanged.

**Checkpoint:** Phase 3 complete when G003 GATE passes.

### Task T026 — Define `FrameAction` type + consts

**File:** `muxcore/frame_action.go` (NEW)
**Description:** Define `FrameAction` int with `FramePass` (=0), `FrameDrop` (=1), `FrameError` (=2). Godoc each — Pass = dispatch normally, Drop = silent discard no client response, Error = JSON-RPC -32004 + no dispatch.
**AC:** File compiles; numeric ordering preserved (Pass=0 makes nil-callback / fail-open default sane).
**PARALLEL-WITH:** T016 (Phase 2 type definition) — verified no file overlap (different files in same `muxcore/` directory; both leaves of dependency tree).

### Task T027 — Add `OnFrameReceived` field to `engine.Config`

**File:** `muxcore/engine/config.go` (MODIFY)
**Description:** Add `OnFrameReceived func(sessionID string, frameSize int, method string) FrameAction` to Config. Godoc per FR-4: invoked sync on reader goroutine, 1ms budget, fail-open on timeout, scope = inbound frames only (CHK014 amendment).
**AC:** Config compiles; nil default preserves v0.23 path.
**Depends on:** T026
**PARALLEL-WITH:** T017 — different fields on same Config struct; no edit conflict if both append at end of struct.

### Task T028 — Wire OnFrameReceived hook in handleDownstreamMessage

**File:** `muxcore/owner/owner.go::handleDownstreamMessage` (MODIFY)
**Description:** At entry of `handleDownstreamMessage`, BEFORE existing notifications/cancelled and cached-response handling logic but AFTER `IsNotification`/`IsRequest` classification — invoke OnFrameReceived (if cfg.OnFrameReceived != nil). Use 1ms `time.AfterFunc` deadline; if timer fires before callback returns, treat as FramePass + log marker `frame_hook_timeout sid=N method=M elapsed_ms=K`. Wrap callback in `defer recover()` panic guard → FramePass + log `frame_hook_panic sid=N`. Switch on result:
- FramePass: fall through (existing logic)
- FrameDrop: return nil (no dispatch, no client response)
- FrameError: build `buildJSONRPCErrorBytes(msg.ID, -32004, "rate limited")` and write via `s.WriteRaw`; return nil
**AC:** Hook fires exactly once per inbound frame; existing dispatch flow unchanged when cfg.OnFrameReceived == nil; benchmark p99 hook overhead < 100µs (NFR-2 measurement point).
**Depends on:** T027
**IF-WRONG:** 1ms budget too tight (aimux profiling shows >1ms p99 atomic token bucket on contended cache lines) → revisit ADR-005 in clarify-amend mode. Cost: ~5 LOC additive `Config.OnFrameReceivedTimeout time.Duration` knob; default 1ms.

### Task T029 — Test: OnFrameReceived FramePass

**File:** `muxcore/owner/frame_hook_test.go` (NEW)
**Description:** `TestFrameHook_PassDispatchesNormally`. Wire callback returning FramePass. Send 5 requests. Assert: dispatch counter == 5, no client errors received, hook invocation counter == 5.
**AC:** Pass = transparent.
**Depends on:** T028

### Task T030 — Test: OnFrameReceived FrameDrop

**File:** `muxcore/owner/frame_hook_test.go` (MODIFY)
**Description:** `TestFrameHook_DropSilentDiscard`. Wire callback returning FrameDrop. Send 10 requests. Assert: (a) dispatch counter == 0; (b) client receives no errors (no -32004); (c) hook invocation counter == 10.
**AC:** Drop is silent and total.
**Depends on:** T028

### Task T031 — Test: OnFrameReceived FrameError

**File:** `muxcore/owner/frame_hook_test.go` (MODIFY)
**Description:** `TestFrameHook_ErrorRespondsWithJSONRPC`. Callback returns FrameError. Send 1 request. Assert: (a) client receives `{"error":{"code":-32004,"message":"rate limited"}}`; (b) dispatch counter == 0; (c) request msg.ID preserved in error response (so client can correlate).
**AC:** -32004 response structurally valid + correlated.
**Depends on:** T028

### Task T032 — Test: OnFrameReceived 1ms timeout = FramePass

**File:** `muxcore/owner/frame_hook_test.go` (MODIFY)
**Description:** `TestFrameHook_TimeoutGracefulDegradation`. Callback that sleeps 5ms then returns FrameDrop. Send 1 request. Assert: (a) request DISPATCHED (treated as Pass via timeout); (b) log marker `frame_hook_timeout` emitted; (c) original callback's late return is logged but ignored (callback completes in background, result discarded).
**AC:** Misbehaving callback does not stall reader goroutine.
**Depends on:** T028

### Task T033 — Test: OnFrameReceived panic recovery

**File:** `muxcore/owner/frame_hook_test.go` (MODIFY)
**Description:** `TestFrameHook_PanicTreatedAsPass`. Callback panics. Send request. Assert: (a) request dispatched (FramePass on panic); (b) log `frame_hook_panic sid=N`; (c) reader goroutine survives — subsequent frames also dispatched.
**AC:** Panic isolation works.
**Depends on:** T028

### Task T034 — Test: OnFrameReceived nil = byte-identical v0.23

**File:** `muxcore/owner/frame_hook_test.go` (MODIFY)
**Description:** `TestFrameHook_NilUnchangedDispatch`. cfg.OnFrameReceived = nil. Send 100 frames. Assert: dispatch counter == 100 (every frame). No log markers from `frame_hook` family. Behavior matches v0.23 fixture.
**AC:** Nil = transparent.
**Depends on:** T028

### Task T035 — Benchmark: OnFrameReceived overhead p99

**File:** `muxcore/owner/frame_hook_test.go` (MODIFY — `BenchmarkFrameHook_NoopOverhead`)
**Description:** Benchmark hook overhead with no-op `func(...) FrameAction { return FramePass }`. Iterate 100k frames. Assert: p99 overhead per frame < 100µs (NFR-2 verification, well under 1ms budget).
**AC:** `go test -bench BenchmarkFrameHook` reports p99 < 100µs on baseline machine.
**Depends on:** T028

### Task T036 — Test: OnFrameReceived inbound-only scope (CHK014)

**File:** `muxcore/owner/frame_hook_test.go` (MODIFY)
**Description:** `TestFrameHook_OutboundFramesNotIntercepted`. Wire callback that increments a counter on EVERY invocation. SessionHandler that returns a response (server→client). Trigger 5 requests. Assert: hook invocation counter == 5 (only inbound frames; outbound responses NOT counted). Also send a `notifications/x-mux/busy` synthetic notification — assert NOT counted (cached/synthetic frames excluded per FR-4 amendment).
**AC:** Scope verified: only client→server frames counted.
**Depends on:** T028

### GATE Task G003 — Phase 3 verification

**RUN:** `go test ./muxcore/owner/... -race -run 'TestFrameHook|BenchmarkFrameHook'`
**CHECK:**
- T029-T036 (8 tests + 1 bench) all PASS
- Benchmark p99 < 100µs
- G001 + G002 still PASS (regression)
**ENFORCE:** PHASE 3 BLOCKED if any fail.
**RESOLVE:** investigate, fix, re-run.
**SAVE:** `.agent/reports/2026-04-29-phase3-gate.txt`.
**Depends on:** T026, T027, T028, T029, T030, T031, T032, T033, T034, T035, T036

---

## Phase 4 — Migration docs + Release

**User-value anchor:** When this phase is done, downstream consumers (aimux, engram, mcp-launcher) can run `go get github.com/thebtf/mcp-mux/muxcore@v0.24.0` and the binary `mcp-mux upgrade --restart` flips the workstation daemon to v0.24.0 with zero session interruption.

**Independent Test:** Run `go get github.com/thebtf/mcp-mux/muxcore@v0.24.0` from a temporary Go module project. Assert: download succeeds, package imports compile, `mcp-mux --version` (after binary build) reports v0.24.0. Run `mcp-mux upgrade --restart` on workstation. Assert: `mux_list` shows continuous session activity, no client disconnections, peer-PID extraction working in test.

**Checkpoint:** Phase 4 complete when G004 GATE passes — release tagged, GitHub releases live, deploy verified.

### Task T037 — Update root `AGENTS.md` with v0.24.0 entry

**File:** `AGENTS.md` (root) (MODIFY)
**Description:** Add v0.24.0 entry mirroring v0.23.0 / v0.22.0 format. Sections: Description (additive ConnInfo + SessionMeta + AuthorizeSession + OnFrameReceived), Migration code template (~50 LOC aimux example), Tests landed list, Cross-version coexistence note.
**AC:** Entry follows existing AGENTS.md style; renders correctly in plaintext + markdown.
**Depends on:** G003

### Task T038 — Generate release notes draft

**File:** `.agent/release-notes/muxcore-v0.24.0.md` (NEW)
**Description:** Structured release notes. Sections: Summary (1 paragraph), New API surface (ConnInfo / SessionMeta / SessionAuth / FrameAction + 4 callbacks + 2 interfaces), Backward-compat guarantees, Migration template (aimux + engram), Closes GH issues #109/#110/#111/#112, Tests landed (count by phase).
**AC:** Notes render cleanly; cite all 4 issues; include version-resolvability proof template.
**Depends on:** G003

### Task T039 — Open PR with squash-merge metadata

**Description:** `gh pr create --base master --head worktree-multi-tenant-extensions --title "muxcore v0.24.0 — multi-tenant extensions (#109, #110, #111, #112)" --body-file .agent/release-notes/muxcore-v0.24.0.md`. Trailers: `Refs: #109, #110, #111, #112`. Add `Closes: #109, #110, #111, #112` to body for auto-close on squash-merge.
**AC:** PR opened; CI matrix triggers.
**Depends on:** T037, T038

### Task T040 — PR review (mcp__pr__pr_invoke)

**Description:** Dispatch `nvmd-platform:pr-reviewer` via `Agent(subagent_type="nvmd-platform:pr-reviewer", model="sonnet", run_in_background=true)`. Wait for completion. Address all findings.
**AC:** All review threads resolved; CI green; CodeRabbit APPROVED.
**Depends on:** T039

### Task T041 — Squash-merge PR

**Description:** `mcp__pr__pr_merge(method:"squash", confirm:true)`. Verify auto-close of #109/#110/#111/#112.
**AC:** PR merged; issues #109-#112 closed via squash-merge trailers.
**Depends on:** T040

### Task T042 — Tag muxcore/v0.24.0 + v0.24.0

**Description:** On merged master: `git pull && git tag -a muxcore/v0.24.0 -m "..." && git tag -a v0.24.0 -m "..." && git push --follow-tags`. Verify tags resolve: `git ls-remote --tags origin | grep v0.24.0`.
**AC:** Both tags pushed; `git ls-remote` confirms.
**Depends on:** T041

### Task T043 — Create GitHub releases

**Description:** `gh release create muxcore/v0.24.0 --title "muxcore/v0.24.0 — multi-tenant extensions" --notes-file .agent/release-notes/muxcore-v0.24.0.md` + same for `v0.24.0`.
**AC:** Both releases visible at github.com/thebtf/mcp-mux/releases.
**Depends on:** T042

### Task T044 — Verify Go module proxy resolution

**Description:** From temp dir: `GOFLAGS="" go get github.com/thebtf/mcp-mux/muxcore@v0.24.0`. Wait up to 5 min for proxy.golang.org caching. Assert: command succeeds; module hash recorded in go.sum matches commit hash.
**AC:** `go get` succeeds; hash matches.
**Depends on:** T043

### Task T045 — Update `.agent/CONTINUITY.md` arc closure

**File:** `.agent/CONTINUITY.md` (MODIFY)
**Description:** Append session entry: arc CLOSED, v0.24.0 shipped, 4 issues auto-closed, deploy status. Format matches existing CONTINUITY entries (per session-skill phase-0 RMFR).
**AC:** Entry written; "Resumability Test" section updated with v0.24.0 state.
**Depends on:** T044

### Task T046 — Deploy locally `mcp-mux upgrade --restart`

**Description:** On workstation: `mcp-mux upgrade --restart`. Verify: (a) graceful restart (snapshot serialized → new daemon ready → shims reconnect); (b) `mux_list` shows continuous session activity post-upgrade; (c) at least one shim reports peer-PID via new ConnInfo path (validate via test handler).
**AC:** Upgrade succeeds; mux_list healthy; ConnInfo extraction proven on real workstation pipeline.
**Depends on:** T044

### Task T047 — Engram comment on aimux issue #178

**Description:** `mcp__plugin_engram_engram__issues(action="comment", id=178, body="muxcore v0.24.0 shipped — see migration template in AGENTS.md. AuthorizeSession + OnFrameReceived + *WithSessionMeta interfaces ready for AIMUX-12 multi-tenant adoption.")`.
**AC:** Comment posted; aimux maintainer notified.
**Depends on:** T043

### GATE Task G004 — Release verification + arc closure

**RUN:**
```bash
git ls-remote --tags origin | grep "v0.24.0"
gh release view muxcore/v0.24.0
gh release view v0.24.0
mcp-mux status # verify v0.24.0 binary on workstation
```
**CHECK:**
- Both tags pushed and resolvable
- Both GitHub releases live
- Workstation runs v0.24.0 binary (status output confirms)
- `go get muxcore@v0.24.0` succeeds (proxy verified)
- 4 issues #109/#110/#111/#112 closed (gh issue view confirms state=closed)
- CONTINUITY.md updated with arc closure
- Engram issue #178 comment posted
**ENFORCE:** Arc INCOMPLETE if any check fails.
**RESOLVE:** investigate, fix root cause, re-run.
**SAVE:** `.agent/reports/2026-04-29-arc-closure.txt`. Engram decision store: arc closed, key facts.
**Depends on:** T037, T038, T039, T040, T041, T042, T043, T044, T045, T046, T047

---

## Concurrent Execution Map

Parallel groups (computed via plan.md inheritance + per-marker 1:1 ID mapping check):

**Phase 1 PARALLEL Group A (5 tasks):** T003 ∥ T004 ∥ T005 ∥ T006 ∥ T007 — platform-specific build-tagged files. Verified no file overlap (5 distinct paths, each `//go:build <os>`-guarded).

**Phase 1 PARALLEL Group B (3 tasks):** T013 split across 3 build-tagged test files — Linux, Windows, Darwin. Verified independent.

**Cross-Phase PARALLEL:** Phase 2 + Phase 3 implementation can run partially in parallel after T-2.1/T-3.1 type definitions land. Specifically T026 ∥ T016 (different files), T027 ∥ T017 (same Config struct — sequential within file but different field appends are mergeable). Tests within each phase are sequential due to shared test files (`authorize_test.go`, `frame_hook_test.go`).

## Suggested MVP Scope

**Minimum shippable v0.24.0:**
- Phase 1 + GATE G001 (US1: peer-PID surfaces in handlers) — covers #109 + #110
- Phase 4 release tasks (T037-T047)

**Without Phase 2:** AuthorizeSession remains nil-only; #111 stays open (not closed by this release).
**Without Phase 3:** OnFrameReceived absent; #112 stays open.

**Recommended scope:** ALL phases (close all 4 issues in v0.24.0). Estimated total wall-time with autopilot delegation: ~2-3 hours implementation + ~1 hour review + tag/deploy.

## Total Task Count

- Phase 1: 15 tasks + G001 GATE = 16
- Phase 2: 10 tasks + G002 GATE = 11
- Phase 3: 11 tasks + G003 GATE = 12
- Phase 4: 11 tasks + G004 GATE = 12
- **Total: 47 tasks (43 work tasks + 4 GATE tasks)**

## Auto-Forward

`Skill("nvmd-platform:nvmd-validate", ".agent/specs/muxcore-multi-tenant-extensions/")` invoked next.
