# Feature: muxcore Multi-Tenant Extensions

**Slug:** muxcore-multi-tenant-extensions
**Created:** 2026-04-29
**Status:** Draft
**Author:** AI Agent (orchestrator) — to be reviewed by user
**Target version:** muxcore/v0.24.0
**Closes GitHub issues:** thebtf/mcp-mux#109, #110, #111, #112

> **Provenance:** Specified by Claude Opus 4.7 on 2026-04-29.
> Evidence from: 4 GitHub issues (filed by `thebtf` 2026-04-28), `user_job_statement.md` (Phase 0), `architecture.md` (7 ADRs decided 2026-04-29), source code reads — `muxcore/handler.go`, `muxcore/owner/owner.go::acceptLoop`/`handleDownstreamMessage`/`dispatchToSessionHandler`, `muxcore/owner/peer_pid_*.go`, `muxcore/owner/file.go::win32File`, `muxcore/jsonrpc/message.go`.
> Confidence: VERIFIED (G12) for all source code references; INFERRED (G12) for downstream aimux integration shape (extracted from issue body code samples).

## Overview

Add four additive engine.Config-level extensions to muxcore that surface OS-level peer credentials (PID/UID/Platform) on every Notification/Session handler dispatch, gate session admission with a single-shot authorize callback, and intercept frames pre-dispatch with a per-frame admission hook. Enables downstream consumers (specifically aimux AIMUX-12) to enforce multi-tenant isolation at the earliest possible boundary inside one daemon process. Backward compatible: zero source changes for existing consumers; opt-in interfaces and nil-defaultable callbacks.

## Clarifications

### Session 2026-04-29 (auto-applied recommendations)

| # | Category | Question | Resolution | Severity | Date |
|---|----------|----------|------------|----------|------|
| C1 | Misc / Domain Model | TenantID placement: `ConnInfo.TenantID` field vs new `SessionMeta` struct with embedded `ConnInfo`? | **Option B — `SessionMeta` with embedded `ConnInfo`.** Rationale: clean separation (OS facts vs consumer policy); extensibility for v0.25+ (Roles/AuthorizedAt/Claims slot in naturally); discriminator `AuthorizedAt.IsZero()` prevents empty-string Hyrum's-Law misuse. | HIGH | 2026-04-29 |
| C2 | Constraints | `windows.GetNamedPipeClientProcessId` already exposed in `golang.org/x/sys/windows` or declare via `NewLazySystemDLL`? | **Verify in implementation; declare via `windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeClientProcessId")` if absent.** Pure stdlib pattern, zero new dependency. | LOW | 2026-04-29 |
| C3 | Functional Scope | aimux migration acceptance scope: actual aimux PR or documentation snippet only? | **Documentation snippet only in AGENTS.md migration template.** Actual aimux migration is separate task per CONTINUITY.md Next item 1, out-of-scope for this arc. | MEDIUM | 2026-04-29 |

## Context

The muxcore engine session lifecycle currently hides OS-level peer identity from consumer handlers and accepts every handshake-complete session unconditionally. Downstream consumers (aimux) building multi-tenant isolation cannot complete the integration without API extensions.

> **Evidence anchor (FM-10 guard):** From `user_job_statement.md` Current Struggle —
> "Today this is unreachable because `NotificationHandler.HandleNotification` receives `ProjectContext` (containing `project.ID = hash(CWD)`) but NOT the underlying `net.Conn` — so peer credential calls are physically impossible on this code path." (#109)
>
> "Today muxcore accepts every session that completes the handshake; aimux can only filter at dispatch time, which is too late — the session is already established, queued frames count against shared resources." (#111)

The architecture decision document (`architecture.md`) records 7 ADRs anchoring the design. ADR-006 in particular found that the third-party `Microsoft/go-winio` library — long thought to hide its `windows.Handle` — already exposes `Fd() uintptr` publicly via `*win32File`, promoted through embedding into `win32Pipe`/`win32MessageBytePipe`. Type-assertion on `interface{ Fd() uintptr }` retrieves the handle from any `winio.ListenPipe().Accept()` net.Conn. No fork required.

Today the data path looks like:
```
Accept → readToken → IsPreRegistered → Bind(token, ownerKey) → AddSession → readSession → handleDownstreamMessage → dispatchToSessionHandler
```

The four extensions inject hooks at three points:
1. **Between Bind and AddSession** — populate `ConnInfo`, run `AuthorizeSession` (deny path closes connection with JSON-RPC -32000 before any upstream resource allocation)
2. **At the start of `handleDownstreamMessage`** — invoke `OnFrameReceived` (sync ≤1 ms, fail-open on timeout)
3. **At dispatch time** — type-assert `*WithSessionMeta` interfaces; pass cached `SessionMeta` if implemented; fall through to legacy handler signature otherwise

## Domain Modeling

DDD evaluated — not needed (rationale: this is an additive API extension within muxcore's existing single bounded context = "engine session lifecycle". No new domain entities, no aggregate roots, no ubiquitous-language shift. Tenant identity ownership lives in aimux (downstream consumer); muxcore exposes the hook surface only).

## Functional Requirements

### FR-1: Expose peer connection credentials to NotificationHandler

The system MUST expose two new value types: (a) `muxcore.ConnInfo` carrying OS-level peer identity ONLY (`PeerPid int`, `PeerUid int`, `Platform string`); (b) `muxcore.SessionMeta` embedding `ConnInfo` and adding consumer-policy fields (`TenantID string`, `AuthorizedAt time.Time`). The system MUST provide a new optional interface `NotificationHandlerWithSessionMeta` extending the existing `NotificationHandler` with `SessionMeta` as an additional argument. When a `SessionHandler` implementation also satisfies `NotificationHandlerWithSessionMeta`, the Owner MUST dispatch notifications via the extended signature; otherwise it MUST fall through to the existing `NotificationHandler.HandleNotification` signature byte-identically. (C1: SessionMeta replaces earlier ConnInfo-only design.)

**Evidence:** `user_job_statement.md` Current Struggle quote 1 (#109) — "peer credential calls are physically impossible on this code path"; quote 2 (#109) — "aimux had to retract FR-12's enforcement promise (CR-002 honesty rewrite, 2026-04-28) until upstream API exposes the connection".

### FR-2: Expose peer connection credentials to SessionHandler

The system MUST provide a new optional interface `SessionHandlerWithSessionMeta` extending `SessionHandler.HandleRequest` with `SessionMeta` as an additional argument. When a `SessionHandler` implementation also satisfies `SessionHandlerWithSessionMeta`, the Owner MUST dispatch requests via the extended signature; otherwise fall through to legacy `SessionHandler.HandleRequest`. The `SessionMeta` value passed to request and notification handlers for the same session MUST be byte-identical (single source of session metadata per session). (C1: SessionMeta replaces earlier ConnInfo-only design.)

**Evidence:** `user_job_statement.md` Current Struggle quote 3 (#110) — "the daemon would have to cache peer identity per session and trust the cache — error-prone, race-prone (session reuse across tenants on Windows named pipes)"; quote 4 (#110) — "All these dispatch decisions need verified peer identity at call time."

### FR-3: Single-shot session authorization gate

The system MUST provide a new optional callback `engine.Config.AuthorizeSession func(ctx context.Context, conn ConnInfo, project ProjectContext) SessionAuth` that fires exactly once per session — after the handshake `Bind` completes (so `ConnInfo`, `project.Cwd`, `project.Env` are populated) and BEFORE the session is added to the Owner's session map. The callback receives `ConnInfo` (OS facts) — NOT `SessionMeta` — because at authorization time the `SessionMeta.TenantID` is what the callback is producing. When the callback returns `AuthDeny`, the system MUST write a JSON-RPC error response (code `-32000`, message from `SessionAuth.Reason`) to the client, close the connection, and not invoke any handler, not allocate upstream, not register the session. When the callback returns `AuthAllow`, the system MUST construct `SessionMeta{Conn: connInfo, TenantID: verdict.TenantID, AuthorizedAt: time.Now()}` and cache it on the Session for all subsequent handler dispatches. When `cfg.AuthorizeSession == nil`, the system MUST proceed exactly as in muxcore v0.23.x (no gate); the cached `SessionMeta` has `TenantID == ""` and `AuthorizedAt == time.Time{}` (zero value — discriminator for "not authorized"). (C1)

**[CHECKLIST-ADDED 2026-04-29 / CHK013]** When `AuthorizeSession` returns `AuthAllow` with empty `TenantID` (i.e., the consumer explicitly authorized without assigning a tenant — multi-tenant feature off, single-tenant deployment), the system MUST proceed: `SessionMeta.TenantID == ""` AND `!SessionMeta.AuthorizedAt.IsZero()` — discriminator semantics still hold (the session WAS authorized, just not tenant-tagged). Consumers reading `meta.TenantID == ""` MUST distinguish via `meta.AuthorizedAt.IsZero()` between "no AuthorizeSession configured" (zero AuthorizedAt) and "AuthorizeSession allowed without tenant" (non-zero AuthorizedAt + empty TenantID). The system MUST NOT validate that AuthAllow carries a non-empty TenantID — empty TenantID is a legitimate consumer choice, not an error.

**[CHECKLIST-ADDED 2026-04-29 / CHK015]** When the daemon initiates shutdown (`o.done` closed) WHILE an `AuthorizeSession` callback is in flight, the system MUST: (a) NOT cancel the in-flight callback — let it complete naturally; (b) on its return, regardless of verdict, close the connection without registering the session (shutdown supersedes admission); (c) NOT invoke any handler dispatch even if verdict was AuthAllow (Owner's session map is no longer accepting). This is consistent with existing muxcore shutdown semantics — accept-loop already exits on `<-o.done` per `acceptLoop()` design.

**Evidence:** `user_job_statement.md` Current Struggle quote 5 (#111) — "a hostile shim from Tenant A must NOT be allowed to spawn a session that the daemon will subsequently dispatch tool calls or log_forward notifications from on behalf of Tenant B"; quote 6 (#111) — "muxcore accepts every session that completes the handshake; aimux can only filter at dispatch time, which is too late — the session is already established, queued frames count against shared resources".

### FR-4: Per-frame admission hook

The system MUST provide a new optional callback `engine.Config.OnFrameReceived func(sessionID string, frameSize int, method string) FrameAction` invoked synchronously on the reader goroutine for every parsed **inbound** frame BEFORE dispatch. The callback MUST execute within a 1 ms latency budget; if exceeded, the system MUST treat the result as `FramePass` (fail-open) and emit a structured log marker `frame_hook_timeout sid=<id> method=<method>`. When the callback returns `FrameDrop`, the frame MUST be silently discarded without dispatch and without client notification. When the callback returns `FrameError`, the system MUST write a JSON-RPC error response (code `-32004`, message `"rate limited"`) to the client and not dispatch. When `cfg.OnFrameReceived == nil`, the system MUST proceed exactly as in muxcore v0.23.x (every frame dispatched).

**[CHECKLIST-ADDED 2026-04-29 / CHK014]** The hook scope is **inbound only** — frames flowing client→server (downstream→Owner). The system MUST NOT invoke `OnFrameReceived` for: (a) outbound responses (Owner→client) generated by SessionHandler; (b) outbound notifications sent via `Notifier.Notify` / `Notifier.Broadcast`; (c) cached responses replayed from `o.cachedInitSessions`; (d) busy/idle `notifications/x-mux/*` synthetic frames. Rate-limiting outbound traffic is consumer responsibility outside muxcore's scope.

**Evidence:** `user_job_statement.md` Current Struggle quote 7 (#112) — "a hostile shim from Tenant A must NOT be able to flood the daemon with notifications/aimux/log_forward frames at a rate that starves Tenant B's legitimate traffic"; quote 8 (#112) — "Without an inbound hook, aimux can only rate-limit at dispatch time — by which point the frame has already consumed reader-goroutine cycles and queue space".

### FR-5: Cross-platform peer-credential extraction

The system MUST populate `ConnInfo.PeerPid` and `ConnInfo.PeerUid` for every session at accept time, on Linux, Windows, and Darwin runtimes, using OS-native introspection. Platforms MUST share a single `peerCreds(conn) ConnInfo` extractor surface; per-platform implementations live in build-tagged files. On Linux the implementation MUST use `SO_PEERCRED` via `syscall.GetsockoptUcred` (existing in `peer_pid_linux.go`, extend to also surface UID). On Windows the implementation MUST use `GetNamedPipeClientProcessId(handle, &pid)` where `handle` is obtained via `interface{ Fd() uintptr }` type-assertion on the `winio.ListenPipe().Accept()` `net.Conn` (no winio fork; see ADR-006). The implementation MUST first try `windows.GetNamedPipeClientProcessId` from `golang.org/x/sys/windows`; if absent, declare via `windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeClientProcessId")` (C2 — pure stdlib, zero new dependency). On Darwin the implementation MUST use `getsockopt(SOL_LOCAL, LOCAL_PEERPID)` for PID and `getpeereid` for UID. When extraction fails on any platform, `ConnInfo.PeerPid`/`ConnInfo.PeerUid` MUST be `0` (zero value) — a valid runtime state — and `ConnInfo.Platform` MUST be set to a stable string identifier (`"linux-unix-stream"`, `"windows-named-pipe"`, `"darwin-unix-stream"`, `"unknown"`).

**Evidence:** `architecture.md` ADR-006; `user_job_statement.md` Current Struggle (entire) — "the runtime knows who's connecting at the OS level — that's a Windows named-pipe HANDLE I can ask `GetNamedPipeClientProcessId` against, or a Unix socket with SO_PEERCRED. But muxcore swallows that".

### FR-6: SessionMeta cached at accept, not per-frame

The system MUST extract `ConnInfo` exactly once per session — in `acceptLoop` immediately after `sessionMgr.Bind` succeeds — and cache it on the `Session` struct, wrapped in a `SessionMeta{Conn: connInfo, TenantID: "", AuthorizedAt: time.Time{}}`. All subsequent dispatch calls (notifications, requests) MUST read the cached `SessionMeta`; the system MUST NOT issue per-frame syscalls for peer-credential lookup. After `AuthorizeSession` returns `AuthAllow`, the system MUST mutate the cached `SessionMeta` exactly once: set `TenantID` from `verdict.TenantID` and `AuthorizedAt` from `time.Now()`. After this single mutation `SessionMeta` is treated as immutable for the session lifetime (no further mutation; consumer reads only). (C1)

**Evidence:** `architecture.md` ADR-002; performance + TOCTOU rationale.

### FR-7: Backward compatibility (zero source change)

Existing muxcore consumers (aimux ≤v0.23.x, engram, mcp-launcher, mcp-mux itself) MUST compile, link, and run byte-identically against muxcore/v0.24.0 without source modification. This is enforced by: (a) `engine.Config.AuthorizeSession == nil` and `engine.Config.OnFrameReceived == nil` zero-value defaults preserving pre-v0.24 behavior; (b) `*WithSessionMeta` interfaces are optional upgrades — handlers that do not implement them keep receiving legacy signatures; (c) no field added to `ProjectContext`; (d) `muxcore.ConnInfo` and `muxcore.SessionMeta` are new types, not modifications of any existing type.

**Evidence:** `architecture.md` ADR-007; pattern consistency with prior additive releases (v0.22.0 `OnInject`, v0.23.0).

## Non-Functional Requirements

### NFR-1: Performance — peer-credential extraction overhead
Peer-credential extraction MUST NOT add more than 200 µs to accept-loop latency on any platform (measured: time from `Accept()` return to `AddSession` call, p99 over 1000 fresh connections). `peerCreds` is called once per session — per-frame overhead is zero (FR-6).

### NFR-2: Performance — OnFrameReceived budget
`OnFrameReceived` callback execution MUST be bounded at 1 ms per frame. Implementation MUST measure and emit a log marker on overrun. Fail-open semantics (FR-4) prevent reader-goroutine starvation by misbehaving callbacks.

### NFR-3: Backward compatibility — observable behavior
With `cfg.AuthorizeSession == nil` AND `cfg.OnFrameReceived == nil` AND no handler implementing `*WithSessionMeta`: muxcore engine accept-loop, dispatch path, message ordering, error codes, log markers, and snapshot/restore behavior MUST be byte-identical to muxcore/v0.23.0 (verified by replay test against captured v0.23.0 trace).

### NFR-4: Cross-platform parity
All four extensions (FR-1..FR-4) MUST behave equivalently across Linux, Windows, Darwin runtimes. Platform-specific code is confined to `peer_pid_*.go` / `peer_uid_*.go` build-tagged files. Test suite MUST pass on all three CI runners.

### NFR-5: Type safety — interface upgrade pattern
Optional handler upgrades MUST use compile-time interface assertion (`if h, ok := o.sessionHandler.(SessionHandlerWithSessionMeta); ok { … }`), consistent with existing muxcore patterns (`NotificationHandler`, `ProjectLifecycle`, `NotifierAware`). Runtime reflection MUST NOT be used.

### NFR-6: Observability
Every new code path MUST emit a structured log marker with consistent prefix `mux.session.auth.` (for AuthorizeSession lifecycle) or `mux.frame.hook.` (for OnFrameReceived lifecycle). Markers list: `auth_invoked sid=N`, `auth_allow sid=N tenant=X`, `auth_deny sid=N reason=Y`, `auth_panic sid=N`, `frame_hook_pass sid=N`, `frame_hook_drop sid=N method=M`, `frame_hook_error sid=N method=M`, `frame_hook_timeout sid=N method=M elapsed_us=K`, `frame_hook_panic sid=N`. Counters MUST be exposed via `HandleStatus` JSON snapshot.

## User Stories

### US1: aimux populates `[role-pid-sess]` log tag from real peer PID (P1)

**As an** aimux daemon operator running multi-tenant log forwarding,
**I want** every `notifications/aimux/log_forward` frame to carry a verifiable peer PID in its log line,
**so that** I can attribute every shim log entry to its originating process for forensics and tenant accounting.

**Acceptance Criteria:**
- [ ] Given an aimux SessionHandler implementing `NotificationHandlerWithSessionMeta`, when a shim PID 12345 sends a notification, the handler receives `meta.Conn.PeerPid == 12345` (verified on Linux + Windows + Darwin).
- [ ] Given a non-aimux consumer (engram, mcp-mux self-tests) that does NOT implement `NotificationHandlerWithSessionMeta`, the legacy `NotificationHandler.HandleNotification(ctx, project, raw)` signature is invoked unchanged.
- [ ] When aimux's `LogIngester` writes the role-pid-session tag, the rendered tag is `[shim-12345-sess_a1b2c3d4]` and includes the verified PID.

### US2: aimux denies cross-tenant session at handshake (P1)

**As an** aimux daemon operator,
**I want** to reject sessions whose peer UID is not enrolled in any tenant before muxcore allocates an upstream,
**so that** an unauthorized peer cannot consume daemon memory, queue space, or upstream resources.

**Acceptance Criteria:**
- [ ] Given `engine.Config.AuthorizeSession` is wired to a callback that returns `AuthDeny{Reason: "uid not enrolled"}` for unknown UIDs, when an unenrolled peer connects, the client receives `{"error":{"code":-32000,"message":"uid not enrolled"}}`, the connection is closed, and the daemon's `mux_list` does NOT show a new session for that peer.
- [ ] Given an enrolled peer connects, `AuthorizeSession` returns `AuthAllow{TenantID: "frontend-team"}`, the session proceeds normally, and every subsequent handler dispatch sees `meta.TenantID == "frontend-team"` and `!meta.AuthorizedAt.IsZero()`.
- [ ] When `engine.Config.AuthorizeSession == nil` (e.g., engram, vanilla mcp-mux), every session that passes the existing token handshake is allowed — byte-identical to v0.23.x.

### US3: aimux rate-limits hostile tenant without dispatch cost (P2)

**As an** aimux daemon operator,
**I want** to drop excess `notifications/aimux/log_forward` frames from a hostile tenant before dispatch,
**so that** the legitimate tenant's frame budget is not consumed by attacker noise.

**Acceptance Criteria:**
- [ ] Given `engine.Config.OnFrameReceived` is wired to a per-tenant token bucket, when tenant A exceeds its budget, the callback returns `FrameDrop` and the next 100 frames from tenant A's session are silently discarded without invoking any SessionHandler dispatch.
- [ ] When the callback returns `FrameError`, the client receives `{"error":{"code":-32004,"message":"rate limited"}}` and the frame is not dispatched.
- [ ] When the callback execution exceeds 1 ms, the system emits `frame_hook_timeout` log marker, treats the result as `FramePass`, and dispatches the frame normally.

## Edge Cases

- **EC-1: Connection torn down between accept and ConnInfo populate** — When `conn.Close()` happens between `Accept()` return and `peerCreds(conn)` syscall, `peerCreds` returns zero `ConnInfo` (not error) and accept-loop logs `peer_creds_failed sid=N`. AuthorizeSession still runs with zero ConnInfo (consumer policy decides whether zero ConnInfo passes).
- **EC-2: AuthorizeSession callback panics** — Panic is recovered; treated as `AuthDeny{Reason: "authorize panic"}`; log marker `auth_panic sid=N`; connection closed cleanly.
- **EC-3: AuthorizeSession callback blocks indefinitely** — Out of scope for v0.24.0. Callback contract: synchronous, expected to return within reasonable wall time. Document as consumer-responsibility. Future work item if observed in production.
- **EC-4: OnFrameReceived callback panics** — Panic is recovered; treated as `FramePass`; log marker `frame_hook_panic sid=N`; frame dispatched normally.
- **EC-5: Multiple sessions sharing one CWD with different tenants (Shared mode)** — Each session has its own ConnInfo with its own peer PID/UID; AuthorizeSession is invoked separately per session; TenantID is per-session, not per-CWD.
- **EC-6: Token-handshake-disabled mode** (`o.tokenHandshake == false`) — ConnInfo population still runs; AuthorizeSession still runs (if configured); no token-related changes.
- **EC-7: `*WithSessionMeta` handler also implements legacy interface** — Type-assertion checks `*WithSessionMeta` first; if matched, legacy signature is NOT invoked (no double-dispatch).
- **EC-8: Snapshot owner restored from disk (template-restored owner)** — ConnInfo extraction happens at fresh accept on the restored owner; AuthorizeSession runs normally; no special template-handling code.
- **EC-9: Windows `Fd()` interface assertion fails (winio API change)** — `peerCreds` returns `ConnInfo{Platform:"windows-named-pipe"}` with PID/UID == 0 and emits log marker `peer_creds_no_fd platform=windows-named-pipe`. Consumer policy decides whether zero ConnInfo passes AuthorizeSession.
- **EC-10: AuthorizeSession deny-path race with concurrent legitimate session** — Deny path closes only the denied connection; other sessions for the same upstream are unaffected. No upstream teardown.
- **EC-11 [CHECKLIST-ADDED CHK015]: Daemon shutdown during in-flight AuthorizeSession** — Callback completes naturally (no forced cancellation); regardless of verdict, the connection is closed without registering session post-shutdown. See FR-3 amendment for full semantics.
- **EC-12 [CHECKLIST-ADDED CHK013]: AuthorizeSession returns AuthAllow with empty TenantID** — Legitimate single-tenant deployment scenario; SessionMeta.TenantID = "" + AuthorizedAt non-zero distinguishes from "no AuthorizeSession configured" (both have empty TenantID, but only the latter has zero AuthorizedAt). See FR-3 amendment for full semantics.

## Out of Scope

- **Re-authorization** — `AuthorizeSession` is single-shot per session. Re-running on session lifetime extension, role change, or token refresh is out of scope for v0.24.0.
- **Async OnFrameReceived** — Hook is sync only. Async/queued variants are out of scope.
- **TenantID-based notification routing** — `Notifier.Notify` continues to use `projectID` (CWD hash), not TenantID. Tenant-aware notification routing is downstream consumer concern (aimux).
- **Roles / claims / authorization expiry** — `SessionAuth.TenantID` is the only consumer-set field. Adding `Roles`, `AuthorizedAt`, `Claims` is a v0.25+ candidate (see Open Question #1 alternative).
- **AuthorizeSession seeing request payload** — Callback fires once before any frame; per-request authorization is OnFrameReceived's domain.
- **Cross-process AuthorizeSession** — Callback runs in the muxcore daemon process. Out-of-process auth (e.g., RPC to external auth service) is consumer-implementable inside the callback but not provided as primitive.
- **macOS handoff / FD passing** — Existing muxcore handoff machinery (v0.21.0) is unchanged. Handoff snapshots do NOT carry `ConnInfo` or `SessionAuth` state; the new owner re-runs `AuthorizeSession` for re-attaching shims.
- **Forking Microsoft/go-winio** — ADR-006 found `Fd()` already public. The pre-existing `thebtf/go-winio` GitHub fork remains as a backup for unrelated future patches; it is NOT a dependency of this arc.

## Dependencies

- **muxcore v0.23.0+** — current baseline; `engine.Config` already carries `OnInject` (v0.23) and `Persistent`/`Name` (v0.22). New fields are additive on the same struct.
- **`golang.org/x/sys/windows`** — already a transitive dependency. Required surface: `windows.Handle`, `windows.GetNamedPipeClientProcessId`. If `GetNamedPipeClientProcessId` is not exposed in `x/sys/windows` directly (likely available via `kernel32.dll`), declare via `windows.NewLazySystemDLL("kernel32.dll").NewProc(...)` — pure stdlib, no third-party.
- **`github.com/Microsoft/go-winio`** — current dependency, unchanged version. Used only for the `winio.ListenPipe().Accept()` Conn return; `Fd()` interface assertion does not require any winio API change.
- **`golang.org/x/sys/unix`** — already a transitive dependency. Required surface: `unix.SO_PEERCRED` (Linux), `unix.LOCAL_PEERPID` and `unix.Getpeereid` (Darwin).
- **No new third-party dependencies.**

## Success Criteria

- [ ] `muxcore/v0.24.0` Go module tag pushed and resolvable via `go get github.com/thebtf/mcp-mux/muxcore@v0.24.0`
- [ ] All four optional API surfaces (FR-1..FR-4) compile and pass unit tests
- [ ] Cross-platform peer-credential extraction (FR-5) verified by integration tests on Linux, Windows, Darwin CI runners
- [ ] Existing muxcore consumer regression suite (aimux unit tests pinned to v0.23.0, engram session tests, mcp-mux integration tests) passes against v0.24.0 with zero source change
- [ ] aimux sample wiring: PR or commit on aimux side wires `engine.Config.AuthorizeSession` + `OnFrameReceived` and runs `mcp-launcher persist` regression — total adoption code ≤ 60 LOC
- [ ] Production deploy: `mcp-mux upgrade --restart` on workstation succeeds; `mux_list` shows session activity unchanged
- [ ] All 4 GitHub issues (#109, #110, #111, #112) auto-closed on PR squash-merge via `Refs: #109, #110, #111, #112` trailers
- [ ] AGENTS.md v0.24.0 entry added to root + muxcore migration notes

## Open Questions

### Resolved 2026-04-29 (--auto)

All three open questions resolved by `nvmd-clarify --auto`. See `## Clarifications` section above for tracking table (C1, C2, C3). Original options retained below for audit trail.

**OQ-1 → C1 — RESOLVED Option B (`SessionMeta` with embedded `ConnInfo`).** ConnInfo holds OS facts only; SessionMeta adds TenantID + AuthorizedAt + future-proofs Roles/Claims. Discriminator `AuthorizedAt.IsZero()` prevents Hyrum's-Law misuse.

**OQ-2 → C2 — RESOLVED.** Try `windows.GetNamedPipeClientProcessId` from `x/sys/windows` first; fall back to `NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeClientProcessId")` if absent. Pure stdlib path, zero new deps.

**OQ-3 → C3 — RESOLVED documentation only.** AGENTS.md migration snippet sufficient; actual aimux PR is separate task per CONTINUITY.md.

---

**Self-review (Step 4):**
- ✅ Placeholder scan: no TODO/TBD/etc.
- ✅ Internal consistency: ConnInfo fields, FR/NFR/US naming consistent across sections
- ✅ Scope check: every FR (FR-1..FR-7) traces to ≥1 US (US1-US3) or NFR; every US has ≥1 FR
- ✅ Ambiguity check: 3 [NEEDS CLARIFICATION] markers — at exactly the cap; all are decision-points, not scope ambiguity

**Quality table:**
| Criterion | Status |
|-----------|--------|
| Completeness | ✅ FR↔US mapping complete |
| Clarity | ✅ Measurable thresholds in NFRs |
| Testability | ✅ Every AC binary pass/fail |
| Scope | ✅ Out of Scope non-empty (8 explicit exclusions) |
| Independence | ✅ Self-contained — only existing muxcore types referenced |
| NEEDS CLARIFICATION count | ✅ 3 / 3 cap |
