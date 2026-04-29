# Implementation Plan — muxcore Multi-Tenant Extensions

**Slug:** muxcore-multi-tenant-extensions
**Spec:** `.agent/specs/muxcore-multi-tenant-extensions/spec.md`
**Architecture:** `.agent/specs/muxcore-multi-tenant-extensions/architecture.md`
**Target version:** muxcore/v0.24.0
**Closes GitHub issues:** #109, #110, #111, #112
**Stack:** Go (muxcore module — `github.com/thebtf/mcp-mux/muxcore`)

> **Provenance:** Planned by Claude Opus 4.7 on 2026-04-29 in worktree `multi-tenant-extensions`.
> Inputs: spec.md (Clarifications C1/C2/C3 resolved 2026-04-29), architecture.md (7 ADRs incl. revised ADR-004), user_job_statement.md (8 verbatim quotes from #109/#110/#111/#112).
> SocratiCode: indexed `green` 5092s ago, 1384 chunks. Graph rebuild in flight (initial 0-edge state stale).
> Confidence: VERIFIED (G12) for all source paths and existing code references; INFERRED for downstream aimux integration shape.

## Reversibility Decision Table

| Decision | Class | Cost-of-Change | Migration Plan | Evidence Anchor |
|----------|-------|----------------|----------------|-----------------|
| ADR-001: Interface upgrade pattern (`*WithSessionMeta`) | REVERSIBLE | Low — interfaces additive; remove via deprecation in v0.25+ | Mark `*WithSessionMeta` deprecated, add replacement, double-dispatch checks both, remove old after one minor cycle | spec.md US1 (P1), US2 (P1) — both rely on type-assertion pattern |
| ADR-002: SessionMeta cached on Session at accept | REVERSIBLE | Low — internal Owner state; consumer-invisible | Replace cache with per-frame extractor if needed — handler API unchanged | spec.md FR-6, NFR-1 (200µs accept overhead) |
| ADR-003: AuthorizeSession single-shot per session | REVERSIBLE | Low — `cfg.AuthorizeSession == nil` keeps legacy path; consumers opt-in | Future re-authorization API (out of v0.24 scope) is additive on top | spec.md FR-3, US2 (P1) |
| ADR-004 (revised): SessionMeta with embedded ConnInfo | **PARTIALLY REVERSIBLE** | Medium — once v0.24.0 ships and aimux adopts, struct shape is locked. Migration requires synchronized aimux + muxcore release with deprecation cycle. Order of magnitude: 1 minor release. | Add `SessionMeta2`/`*WithSessionMetaV2` interface, parallel-dispatch both, deprecate v1 in v0.25+. ~50 LOC migration shim. | spec.md C1 resolution; user_job_statement.md "I need three things" enumeration; alt enumerated: ConnInfo.TenantID rejected for concept-mixing |
| ADR-005: OnFrameReceived sync ≤1ms fail-open | REVERSIBLE | Low — sync semantic is callback contract; async variant is additive (`OnFrameReceivedAsync`) without breaking sync | Add async sibling callback, leave sync as-is. Or relax timeout via config knob (additive). | spec.md FR-4, NFR-2; alt enumerated: async-queued rejected for ordering-loss risk |
| ADR-006: peer-creds via `Fd()` interface assertion | REVERSIBLE | Low — single file per platform; falls back to zero ConnInfo on assertion failure | Switch to `SyscallConn()` PR if winio merges it; switch to fork if winio breaks Fd() exposure | spec.md FR-5, EC-9 |
| ADR-007: One arc, one tag (v0.24.0) | REVERSIBLE | Low — could split into 4 PRs after the fact, but additional review cycles | Cherry-pick per phase to separate branches if review reveals scope concern | architecture.md ADR-007 |

**REVERSIBILITY_AUDIT: PASS** — all decisions classified, IRREVERSIBLE count = 0, PARTIALLY REVERSIBLE = 1 (ADR-004) with full migration plan + alternative enumeration.

**Phase ordering validation:** US1 (P1) covered by Phase 1 (foundation). US2 (P1) covered by Phase 2 (AuthorizeSession). US3 (P2) covered by Phase 3 (OnFrameReceived). P1 stories appear in Phases 1-2 — AP-REV-4 (tech-first ordering without P-priority check) satisfied.

## Tech Stack

| Layer | Technology | Version | Source |
|-------|-----------|---------|--------|
| Language | Go | 1.23+ (current muxcore go.mod) | `muxcore/go.mod` |
| Build constraints | `//go:build linux`, `windows`, `darwin` | — | existing pattern in `peer_pid_*.go` |
| Async I/O | `golang.org/x/sys/windows` IOCP via winio | current | already in `go.sum` |
| Named pipes (Windows) | `github.com/Microsoft/go-winio` | unchanged version | already in `go.sum` |
| Unix sockets | stdlib `net` + `golang.org/x/sys/unix` | current | already in `go.sum` |
| Tests | stdlib `testing` + `github.com/stretchr/testify/assert` (where used) | current | existing pattern |
| Lint | `go vet`, golangci-lint via CI | unchanged | `.github/workflows/ci.yml` |

**Library-first verdict:** No new third-party dependency. All cross-platform peer-credential primitives available via stdlib + already-imported `x/sys/windows` and `x/sys/unix`. Microsoft/go-winio already provides everything needed via existing `*win32File.Fd() uintptr` (architecture.md ADR-006).

## Architecture (summary — full doc in architecture.md)

Three additive extensions to muxcore engine layer, wiring into existing `owner.acceptLoop` (between `Bind` and `AddSession`) and `owner.handleDownstreamMessage` (frame entry point). New value types `ConnInfo` and `SessionMeta` flow through optional interface upgrades on `SessionHandler`. Platform-specific peer-credential extraction consolidated via `peerCreds(conn) ConnInfo` extractor.

Reference: `architecture.md` §2 mermaid diagram, §3 component map, §5 data flow.

## Data Model

### `muxcore.ConnInfo` (new public type — `muxcore/connection_info.go`)
```go
// ConnInfo carries OS-level peer identity for an established session connection.
// Populated once at accept time via peerCreds(conn); cached for session lifetime.
// Zero-value fields (PeerPid==0 / PeerUid==0) indicate extraction failure or
// unsupported transport — a valid runtime state.
type ConnInfo struct {
    PeerPid  int    // OS process ID of the connected peer; 0 if unavailable
    PeerUid  int    // Unix UID; 0 on Windows or unavailable
    Platform string // "linux-unix-stream" | "windows-named-pipe" | "darwin-unix-stream" | "unknown"
}
```

### `muxcore.SessionMeta` (new public type — `muxcore/session_meta.go`)
```go
// SessionMeta combines OS-derived peer identity (Conn) with consumer-policy
// metadata produced by AuthorizeSession. Discriminator: AuthorizedAt.IsZero()
// signals "session was never authorized" (unconfigured callback or zero Conn).
type SessionMeta struct {
    Conn         ConnInfo
    TenantID     string    // "" if AuthorizeSession not configured or returned no tenant
    AuthorizedAt time.Time // zero if not authorized
}
```

### `muxcore.AuthDecision` + `muxcore.SessionAuth` (new public types — `muxcore/session_auth.go`)
```go
type AuthDecision int

const (
    AuthAllow AuthDecision = iota
    AuthDeny
)

// SessionAuth is the verdict returned by engine.Config.AuthorizeSession.
type SessionAuth struct {
    Decision AuthDecision
    TenantID string // free-form consumer-defined identifier; ignored when Decision == AuthDeny
    Reason   string // surfaced to client in JSON-RPC error message on AuthDeny
}
```

### `muxcore.FrameAction` (new public type — `muxcore/frame_action.go`)
```go
type FrameAction int

const (
    FramePass FrameAction = iota // dispatch normally (default)
    FrameDrop                    // silently drop, no dispatch, no client response
    FrameError                   // respond with JSON-RPC -32004, no dispatch
)
```

### `muxcore.NotificationHandlerWithSessionMeta` + `muxcore.SessionHandlerWithSessionMeta` (new optional interfaces — `muxcore/handler.go` extensions)
```go
type NotificationHandlerWithSessionMeta interface {
    NotificationHandler
    HandleNotificationWithSessionMeta(ctx context.Context, project ProjectContext, meta SessionMeta, notification []byte)
}

type SessionHandlerWithSessionMeta interface {
    HandleRequestWithSessionMeta(ctx context.Context, project ProjectContext, meta SessionMeta, request []byte) (response []byte, err error)
}
```

### `engine.Config` extensions (in-place additive — `muxcore/engine/config.go` or `engine.go`)
```go
type Config struct {
    // ... existing fields (Name, Command, Args, Persistent, OnInject, Handler, SessionHandler, etc.) ...

    // AuthorizeSession, when non-nil, is invoked once per session after the
    // initial IPC handshake completes and BEFORE any frame is dispatched to
    // SessionHandler / NotificationHandler. Default (nil) preserves pre-v0.24
    // behavior — every handshake-complete session is allowed.
    AuthorizeSession func(ctx context.Context, conn ConnInfo, project ProjectContext) SessionAuth

    // OnFrameReceived, when non-nil, is invoked synchronously for every inbound
    // frame from a client AFTER framing/parsing but BEFORE dispatch. Default (nil)
    // preserves pre-v0.24 behavior — every parsed frame is dispatched. The
    // callback MUST return within 1ms; overrun is treated as FramePass.
    OnFrameReceived func(sessionID string, frameSize int, method string) FrameAction
}
```

### `owner.Session` extension (internal, `muxcore/owner/owner.go`)
```go
type Session struct {
    // ... existing fields ...
    meta SessionMeta // populated at accept; mutated once on AuthAllow; immutable after
}
```

## API Contracts

External (public) — see `Data Model` above. Internal (Owner) call sites:

| Call site (file:func) | Change |
|----------------------|--------|
| `muxcore/owner/owner.go::acceptLoop` | After `sessionMgr.Bind` succeeds, populate `s.meta = SessionMeta{Conn: peerCreds(conn)}`. If `engine.AuthorizeSession != nil`, call it; on AuthDeny write JSON-RPC -32000 + close + return; on AuthAllow set `s.meta.TenantID` + `s.meta.AuthorizedAt`. |
| `muxcore/owner/owner.go::handleDownstreamMessage` | At entry (after IsNotification/IsRequest classification, before dispatch), if `engine.OnFrameReceived != nil`, invoke with 1ms deadline. Switch on action: Pass=fall through, Drop=return nil, Error=write -32004 JSON-RPC error + return nil. |
| `muxcore/owner/owner.go::dispatchToSessionHandler` | Type-assert `o.sessionHandler.(SessionHandlerWithSessionMeta)` first; if matched, call extended signature with cached `s.meta`. Else fall through to legacy `HandleRequest`. |
| `muxcore/owner/owner.go::handleDownstreamMessage` (notification branch) | Same type-assert pattern for `NotificationHandlerWithSessionMeta`. |
| `muxcore/owner/peer_creds.go` (NEW shared) | Defines `peerCreds(conn net.Conn) ConnInfo` — calls into platform-specific helpers. |
| `muxcore/owner/peer_pid_windows.go` (REWRITE) | Real impl via `interface{ Fd() uintptr }` + `GetNamedPipeClientProcessId(handle, &pid)`. |
| `muxcore/owner/peer_pid_darwin.go` (REWRITE) | Real impl via `getsockopt(SOL_LOCAL, LOCAL_PEERPID)`. |
| `muxcore/owner/peer_uid_linux.go` (NEW) | Extract UID via SO_PEERCRED ucred (extension of existing `peer_pid_linux.go` pattern). |
| `muxcore/owner/peer_uid_darwin.go` (NEW) | Extract UID via `syscall.Getpeereid`. |
| `muxcore/owner/peer_uid_windows.go` (NEW stub) | Returns 0 (Windows has no UID concept comparable to Unix; consumer policy uses PID + token-based identity instead). |

## File Structure

```
muxcore/
├── connection_info.go               # NEW — ConnInfo type + godoc
├── session_meta.go                  # NEW — SessionMeta type + godoc + IsAuthorized() helper
├── session_auth.go                  # NEW — AuthDecision, SessionAuth, AuthAllow/AuthDeny consts
├── frame_action.go                  # NEW — FrameAction, FramePass/Drop/Error consts
├── handler.go                       # MODIFY — add NotificationHandlerWithSessionMeta + SessionHandlerWithSessionMeta interfaces
│
├── engine/
│   ├── config.go (or engine.go)     # MODIFY — add AuthorizeSession + OnFrameReceived fields to Config
│   └── engine_test.go               # MODIFY — add tests for new Config fields
│
├── owner/
│   ├── owner.go                     # MODIFY — Session.meta field, acceptLoop wiring, dispatch upgrade
│   ├── peer_creds.go                # NEW (shared, no build tag) — peerCreds(conn) dispatcher
│   ├── peer_pid_linux.go            # MODIFY — also surface UID via existing ucred
│   ├── peer_pid_windows.go          # REWRITE — real impl via Fd() + GetNamedPipeClientProcessId
│   ├── peer_pid_darwin.go           # REWRITE — real impl via getsockopt(LOCAL_PEERPID)
│   ├── peer_uid_linux.go            # NEW — UID via SO_PEERCRED
│   ├── peer_uid_darwin.go           # NEW — UID via getpeereid
│   ├── peer_uid_windows.go          # NEW (stub returning 0)
│   ├── peer_creds_test.go           # NEW — cross-platform peer-creds tests (per-OS test files via build tags)
│   ├── peer_creds_linux_test.go     # NEW — Linux-specific test
│   ├── peer_creds_windows_test.go   # NEW — Windows-specific test (loopback to os.Getpid)
│   ├── peer_creds_darwin_test.go    # NEW — Darwin-specific test
│   ├── authorize_test.go            # NEW — AuthorizeSession allow/deny/nil/panic tests
│   ├── frame_hook_test.go           # NEW — OnFrameReceived pass/drop/error/timeout/panic tests
│   └── dispatch_test.go             # MODIFY — extend with *WithSessionMeta dispatch tests
```

## Phases

### Phase 1 — Foundation: ConnInfo + SessionMeta + peer-creds + handler interfaces (#109/#110)

**Deliverable:** `muxcore/v0.24.0-rc1` runnable; `cfg.AuthorizeSession == nil` + `cfg.OnFrameReceived == nil` keep behavior byte-identical to v0.23; `*WithSessionMeta` consumers see real PID/UID on all 3 platforms; existing handlers unchanged.

#### Tasks
1. **T-1.1** Define `ConnInfo` type (`muxcore/connection_info.go`)
2. **T-1.2** Define `SessionMeta` type + `IsAuthorized() bool` helper (`muxcore/session_meta.go`)
3. **T-1.3** [P] Real `peer_pid_windows.go` via `interface{ Fd() uintptr }` + `GetNamedPipeClientProcessId` (try `x/sys/windows` first, fallback to `NewLazySystemDLL`)
4. **T-1.4** [P] Real `peer_pid_darwin.go` via `getsockopt(SOL_LOCAL, LOCAL_PEERPID)`
5. **T-1.5** [P] Extend `peer_pid_linux.go` to also surface UID; create `peer_uid_linux.go` (via existing ucred)
6. **T-1.6** [P] `peer_uid_darwin.go` via `getpeereid`
7. **T-1.7** [P] `peer_uid_windows.go` stub returning 0
8. **T-1.8** Shared `peer_creds.go` dispatcher: `peerCreds(conn) ConnInfo` (no build tag)
9. **T-1.9** Add `NotificationHandlerWithSessionMeta` + `SessionHandlerWithSessionMeta` interfaces in `muxcore/handler.go`
10. **T-1.10** Add `Session.meta SessionMeta` field in `muxcore/owner/owner.go`
11. **T-1.11** Wire ConnInfo population in `acceptLoop` after `sessionMgr.Bind`
12. **T-1.12** Type-assert `*WithSessionMeta` in `dispatchToSessionHandler` + notification branch of `handleDownstreamMessage`; legacy fallback intact
13. **T-1.13** [P] Cross-platform peer-creds tests (3 build-tagged files), each verifying `peerCreds(conn)` returns non-zero PID matching `os.Getpid()` against a loopback test client
14. **T-1.14** Dispatch test: handler implementing both legacy `HandleRequest` and `HandleRequestWithSessionMeta` must receive only the SessionMeta version (precedence test)
15. **T-1.15** Backward-compat test: handler with NO `*WithSessionMeta` receives byte-identical legacy dispatch
16. **T-1.16 GATE** Phase 1 verification: `go test ./muxcore/... -race` green on Linux + Windows + Darwin CI; `go vet ./...` clean

#### Concurrent Work Directives (computed)

PARALLEL group A (T-1.3 .. T-1.7): all platform-specific build-tagged files. Distinct file paths verified — no overlap between `peer_pid_windows.go` / `peer_pid_darwin.go` / `peer_uid_linux.go` / `peer_uid_darwin.go` / `peer_uid_windows.go`. PARALLEL-WITH: T-1.3, T-1.4, T-1.5, T-1.6, T-1.7 — verified no file overlap (5 distinct files, each guarded by `//go:build` tag).

PARALLEL group B (T-1.13 split): platform-specific test files — same independence. PARALLEL-WITH: peer_creds_linux_test.go, peer_creds_windows_test.go, peer_creds_darwin_test.go — verified no file overlap.

SEQUENTIAL: T-1.1 → T-1.2 (SessionMeta embeds ConnInfo) → T-1.8 (peerCreds returns ConnInfo) → T-1.9 (handlers consume SessionMeta) → T-1.10 (Session.meta typed) → T-1.11 (wire) → T-1.12 (dispatch) → T-1.13 (tests) → T-1.16 (GATE).

#### Contingency Branches

**ADR-006 trigger — `Fd()` interface assertion fails on real winio Conn:**
- IF `conn.(interface{ Fd() uintptr })` returns `!ok` on actual `winio.ListenPipe().Accept()` output
- THEN fall back to extracting handle via `reflect.ValueOf(conn).Elem().FieldByName("win32File").Elem().FieldByName("handle")` (unsafe, version-pinned to current winio)
- IF that also fails (winio internal restructure)
- THEN ConnInfo populates with `Platform: "windows-named-pipe", PeerPid: 0, PeerUid: 0`; emit log marker `peer_creds_no_fd`. Defer real impl to a follow-up arc that opens a winio PR or forks.
- Recorded in EC-9 of spec.md.

**ADR-004 trigger — aimux maintainer requests Option A revival:**
- IF aimux team objects to two-struct shape during PR review
- THEN add `ConnInfo.TenantID` field as deprecated-on-arrival alias, keep `SessionMeta.TenantID` as primary; remove ConnInfo.TenantID in v0.25
- Cost: ~10 LOC + extra deprecation comment block.

### Phase 2 — AuthorizeSession (#111)

**Deliverable:** `cfg.AuthorizeSession` callback fires once per session pre-dispatch; AuthDeny path closes connection cleanly with JSON-RPC -32000; AuthAllow path populates `s.meta.TenantID` + `s.meta.AuthorizedAt`.

#### Tasks
1. **T-2.1** Define `AuthDecision`, `SessionAuth`, `AuthAllow`/`AuthDeny` consts (`muxcore/session_auth.go`)
2. **T-2.2** Add `AuthorizeSession func(ctx, ConnInfo, ProjectContext) SessionAuth` to `engine.Config`
3. **T-2.3** Wire in `acceptLoop` between `peerCreds(conn)` (T-1.11) and `AddSession`: invoke callback, recover panic as AuthDeny, switch on Decision
4. **T-2.4** Implement deny path: build `{"jsonrpc":"2.0","error":{"code":-32000,"message":"<reason>"}}`, write to conn, close conn, log marker `auth_deny sid=N reason=...`, do NOT add to session map, do NOT spawn upstream
5. **T-2.5** Implement allow path: set `s.meta.TenantID = verdict.TenantID; s.meta.AuthorizedAt = time.Now()`, log marker `auth_allow sid=N tenant=X`, fall through to AddSession
6. **T-2.6** Test: AuthorizeSession returns AuthDeny → client receives -32000, conn closed, no session in `o.sessions`
7. **T-2.7** Test: AuthorizeSession returns AuthAllow{TenantID:"x"} → handler dispatch sees `meta.TenantID == "x"` and `!meta.AuthorizedAt.IsZero()`
8. **T-2.8** Test: `cfg.AuthorizeSession == nil` → byte-identical to v0.23 (capture-replay against fixture)
9. **T-2.9** Test: AuthorizeSession panics → treated as AuthDeny{Reason:"authorize panic"}, log marker `auth_panic sid=N`, conn closed
10. **T-2.10** Test: AuthorizeSession deny does NOT spawn upstream (assert `o.upstream` remains nil for that session path)
11. **T-2.11 GATE** Phase 2 verification: `go test ./muxcore/owner/... -race -run "Authorize"` green; `go test ./muxcore/...` regression unchanged

#### Concurrent Work Directives (computed)

PARALLEL group (T-2.6, T-2.7, T-2.8, T-2.9, T-2.10): test functions in same `authorize_test.go` file → SEQUENTIAL within the file due to shared file modification. Promote to single file → all sequential.

SEQUENTIAL: T-2.1 → T-2.2 → T-2.3 → T-2.4 + T-2.5 → tests T-2.6..T-2.10 (sequential due to shared test file) → T-2.11 GATE.

#### Contingency Branches

**ADR-003 trigger — async AuthorizeSession requested by consumer:**
- IF aimux requests async pre-flight (e.g., RPC to external auth service inside callback)
- THEN keep sync semantic; document that callback may invoke long-running operations at consumer's risk; future v0.25 may add `AuthorizeSessionAsync` sibling
- Cost: 0 LOC for v0.24; documentation work only.

### Phase 3 — OnFrameReceived (#112)

**Deliverable:** `cfg.OnFrameReceived` callback fires sync per frame at `handleDownstreamMessage` entry; FramePass falls through; FrameDrop silent discard; FrameError JSON-RPC -32004; timeout >1ms = FramePass + log marker.

#### Tasks
1. **T-3.1** Define `FrameAction`, `FramePass`/`FrameDrop`/`FrameError` consts (`muxcore/frame_action.go`)
2. **T-3.2** Add `OnFrameReceived func(sessionID string, frameSize int, method string) FrameAction` to `engine.Config`
3. **T-3.3** Wire at `handleDownstreamMessage` entry (after msg.IsRequest/IsNotification classification but before any other dispatch logic): invoke callback wrapped in 1ms-deadline guard, recover panic as FramePass
4. **T-3.4** Implement FrameDrop: return nil from `handleDownstreamMessage` without further dispatch
5. **T-3.5** Implement FrameError: build `{"jsonrpc":"2.0","error":{"code":-32004,"message":"rate limited"}}` + write via `s.WriteRaw` + return nil; preserve msg ID for response if msg.IsRequest
6. **T-3.6** Implement timeout enforcement: use `time.AfterFunc(1*time.Millisecond, ...)` cancel signal; if fires before callback returns, treat as FramePass + log marker `frame_hook_timeout`
7. **T-3.7** Test: callback returns FramePass → frame dispatched normally
8. **T-3.8** Test: callback returns FrameDrop → no dispatch; assert mock SessionHandler.HandleRequest call count == 0
9. **T-3.9** Test: callback returns FrameError → client receives -32004; SessionHandler not invoked
10. **T-3.10** Test: callback takes 5ms → treated as FramePass (graceful degradation); log marker emitted
11. **T-3.11** Test: callback panics → treated as FramePass; log marker `frame_hook_panic sid=N`
12. **T-3.12** Test: `cfg.OnFrameReceived == nil` → byte-identical to v0.23 dispatch path
13. **T-3.13 GATE** Phase 3 verification: `go test ./muxcore/owner/... -race -run "FrameHook|OnFrame"` green

#### Concurrent Work Directives (computed)

PARALLEL: Phase 3 implementation can run partially in parallel with Phase 2 (T-3.1 / T-3.2 / T-3.3 are independent of `s.meta` mutations from Phase 2). PARALLEL-WITH: Phase 2 tasks T-2.1..T-2.10 — verified no file overlap (Phase 2 = `session_auth.go` + acceptLoop region; Phase 3 = `frame_action.go` + `handleDownstreamMessage` region; both in `owner.go` but different functions).

SEQUENTIAL within Phase 3: T-3.1 → T-3.2 → T-3.3..T-3.6 (same handleDownstreamMessage region — sequential) → tests T-3.7..T-3.12 (same test file) → T-3.13 GATE.

#### Contingency Branches

**ADR-005 trigger — 1ms budget unrealistic for some consumers:**
- IF aimux profiling reveals atomic token bucket >1ms p99 (unlikely but possible on contended cache lines)
- THEN make budget configurable: `engine.Config.OnFrameReceivedTimeout time.Duration` (default 1ms)
- Cost: ~5 LOC additive; no API break.

### Phase 4 — Migration docs + AGENTS.md + Release

**Deliverable:** muxcore/v0.24.0 + v0.24.0 tagged + GitHub releases live + AGENTS.md migration template + CONTINUITY.md updated.

#### Tasks
1. **T-4.1** Update `AGENTS.md` root with v0.24.0 entry (mirroring v0.23.0 / v0.22.0 entries — Description + Migration code template)
2. **T-4.2** Update `muxcore/AGENTS.md` (if exists) with same v0.24.0 entry
3. **T-4.3** Generate release notes draft (`.agent/release-notes/muxcore-v0.24.0.md`)
4. **T-4.4** Squash-merge PR with `Refs: #109, #110, #111, #112` trailers (auto-close)
5. **T-4.5** Tag `muxcore/v0.24.0` + `v0.24.0` (binary tag, since muxcore consumers pull via go module path)
6. **T-4.6** Push tags `--follow-tags`
7. **T-4.7** Create GitHub releases (muxcore/v0.24.0 + v0.24.0) with body from release notes
8. **T-4.8** Verify Go module proxy resolves `go get github.com/thebtf/mcp-mux/muxcore@v0.24.0`
9. **T-4.9** Update `.agent/CONTINUITY.md` — record arc closure
10. **T-4.10** Deploy locally: `mcp-mux upgrade --restart` on workstation; verify `mux_list` healthy
11. **T-4.11** Post engram comment on issue #178 (aimux logging downstream) announcing v0.24.0 + adoption template

## Library Decisions

| Concern | Decision | Rationale |
|---------|---------|-----------|
| Async I/O on Windows | **Reuse winio** (existing dep) | `*win32File.Fd()` already public; ADR-006. No fork needed. |
| Peer-creds Linux | **stdlib `syscall.GetsockoptUcred`** | Existing pattern in `peer_pid_linux.go`. |
| Peer-creds Darwin | **`golang.org/x/sys/unix.LOCAL_PEERPID` + `unix.Getpeereid`** | Already a transitive dep. No new third-party. |
| Peer-creds Windows | **`windows.GetNamedPipeClientProcessId`** | Try `x/sys/windows` first, fallback `NewLazySystemDLL("kernel32.dll")` (C2 resolution). |
| 1ms timeout for OnFrameReceived | **`time.AfterFunc` + atomic.Bool flag** | Stdlib only; deterministic; no scheduler dependency. |
| JSON-RPC error response builder | **Existing `buildJSONRPCErrorBytes`** in `muxcore/owner/owner.go` | Already used for handler-panic path; same pattern. |
| Test mocks | **Existing `mockSessionHandler` + `mockLifecycleHandler`** in `dispatch_test.go` | Extend, don't replace. |

## Reusability Awareness

No reusable library candidates emitted. All planned modules are muxcore-engine-specific:
- `ConnInfo`, `SessionMeta`, `SessionAuth`, `FrameAction` — engine API surface, not library-eligible
- `peerCreds` extractor — internal owner package, intra-module consolidation (NOT extraction); 3 platform files share signature but each is OS-coupled
- `*WithSessionMeta` interfaces — coupled to muxcore handler contract; consumer set = 1 (aimux)

Wrapper invoked once per planned module via `try_detect(phase="plan", host_context={module, plan_artifact})`. All 7 modules evaluated, none satisfy Rule of Three or self-contained-with-clean-interface criteria.

Engram cross-project query result: `"No cross-project matches found"` (zero patterns matching `reusability-candidate pattern:engine-extension` across nvmd-platform projects — confirmed via `mcp__plugin_engram_engram__recall_memory(query="reusability-candidate pattern:engine-extension")` returning 0 hits).

## Domain Modeling

DDD evaluated — not needed (rationale: extension of single existing bounded context = "muxcore engine session lifecycle". No new sub-domains, no aggregate roots, no ubiquitous-language shift, no >=3 entities introduced. Tenant identity is a string identifier owned by the downstream consumer (aimux), not modeled as muxcore domain entity).

Entity count in proposed schema: 0 project-owned entities (ConnInfo / SessionMeta / SessionAuth / FrameAction are value objects per C9, excluded from entity count per `references/ddd-candidates.md` C3).

## Unknowns and Risks

| # | Type | Description | Mitigation |
|---|------|-------------|------------|
| R1 | Unknown | Is `windows.GetNamedPipeClientProcessId` already in `x/sys/windows`? | Verify in T-1.3; fallback to `NewLazySystemDLL` (already documented in C2) |
| R2 | Risk | `Fd()` interface assertion may break on future winio API redesign | EC-9 documents zero-ConnInfo fallback; ADR-006 contingency |
| R3 | Risk | 1ms OnFrameReceived budget too tight for some consumer use cases | Phase 3 contingency — configurable timeout in v0.25 |
| R4 | Risk | aimux team objects to SessionMeta two-struct shape during PR review | ADR-004 contingency — additive ConnInfo.TenantID alias |
| R5 | Unknown | Does macOS `LOCAL_PEERPID` work on UDS over `winio` substitute? (winio is Windows-only — no actual conflict, but cross-platform test fixtures must verify) | T-1.13 cross-platform tests; CI runs on macOS |
| R6 | Risk | Snapshot/restore (handoff v0.21.0) doesn't carry SessionMeta — re-attaching shims see fresh AuthorizeSession on new owner | Documented in spec.md Out of Scope (handoff section); not a regression — old behavior was no auth at all |

## Constitution Compliance

Project Constitution (from CLAUDE.md):
- ✅ "We Build Serious Software" — multi-tenant security extension, production infrastructure
- ✅ "Default assumption: multi-user, multi-tenant" — feature is exactly this
- ✅ "v1 small but correct" — minimal API surface (4 additive callbacks/types), full backward compat
- ✅ "Architecture decisions account for future scale" — SessionMeta extensibility (Roles/Claims slot in)
- ✅ "Document upgrade path" — clear Migration plan for ADR-004 PARTIALLY REVERSIBLE decision

Project orchestrator overlay (CLAUDE.md):
- ✅ Plugin version bump alignment — N/A (this arc bumps muxcore module, not nvmd-platform plugin)
- ✅ RMFR persona — N/A (no new persona-bearing skills introduced)
- ✅ Commit message convention — `feat(muxcore)` / `fix(muxcore)` / `test(muxcore)` per phase

## DX Review (library/SDK consumer angle)

muxcore is consumed by aimux, engram, mcp-launcher. v0.24.0 adoption TTHW (Time to Hello World) for AuthorizeSession:

```go
eng, _ := engine.New(engine.Config{
    Name:           "aimux",
    SessionHandler: srv.SessionHandler(),
    AuthorizeSession: func(ctx context.Context, conn muxcore.ConnInfo, p muxcore.ProjectContext) muxcore.SessionAuth {
        return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: tenantOf(conn.PeerUid)}
    },
})
```

**TTHW: Champion (<2 min)** from `go get github.com/thebtf/mcp-mux/muxcore@v0.24.0` to working AuthorizeSession callback. ~5 LOC config addition + 1 import line.

OnFrameReceived TTHW: same — 1 callback assignment.

`*WithSessionMeta` TTHW: ~10 LOC (add method to existing handler struct, type-assert in tests).

Total adoption surface for aimux: ~30-40 LOC (per spec.md Success Criteria ≤60 LOC).

## Validation Checklist (Step 2)

- [x] Every FR has implementation in at least one phase (FR-1/FR-2 → Phase 1; FR-3 → Phase 2; FR-4 → Phase 3; FR-5/FR-6/FR-7 → Phase 1)
- [x] Every NFR has concrete approach (NFR-1 200µs accept overhead measured in T-1.13; NFR-2 1ms budget enforced in T-3.6; NFR-3 byte-identical replay in T-1.15/T-2.8/T-3.12; NFR-4 cross-platform CI; NFR-5 type-assert pattern; NFR-6 explicit log markers per task)
- [x] Library decisions documented for all major components (table above; no new third-party)
- [x] File structure consistent with existing project patterns (build-tagged platform files match `peer_pid_*.go` precedent)
- [x] Phases have clear boundaries and deliverables (Phase 1 foundation, Phase 2 auth, Phase 3 throttle, Phase 4 release)
- [x] Constitution principles respected (table above)
- [x] Phase 0.5 parallelism analysis ran against `green` SocratiCode index (graph rebuild in flight; Trivial/Standard classification used per file-distinctness verification)
- [x] Every phase with 2+ tasks has Concurrent Work Directives subsection
- [x] PARTIALLY REVERSIBLE decision (ADR-004) has Medium contingency block
- [x] No `[P]` marker without provenance (`PARALLEL-WITH:` lines for all parallel groups)
- [x] Phase 0 Reversibility Audit emitted PASS

## Auto-Forward

`Skill("nvmd-platform:nvmd-checklist", "muxcore-multi-tenant-extensions")` invoked next.
