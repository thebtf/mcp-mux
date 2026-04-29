# User Job Statement — muxcore Multi-Tenant Extensions

**Persona:** P-EPI1 User-Researcher Epistemic Restrictor
**Source quotes:** GitHub issues #109, #110, #111, #112 (all opened by `thebtf` 2026-04-28, all labelled `enhancement`)
**Downstream consumer cited:** aimux (project AIMUX-12 multi-tenant isolation feature, in flight)

## Current Struggle

The user is integrating multi-tenant isolation into a downstream consumer (aimux) that runs on top of muxcore engine. They cannot complete the integration because muxcore's session lifecycle hides the OS-level identity of the connecting peer.

Verbatim from issue #109:
> "Today this is unreachable because `NotificationHandler.HandleNotification` receives `ProjectContext` (containing `project.ID = hash(CWD)`) but NOT the underlying `net.Conn` — so peer credential calls are physically impossible on this code path."

> "aimux had to retract FR-12's enforcement promise (CR-002 honesty rewrite, 2026-04-28) until upstream API exposes the connection."

Verbatim from issue #110:
> "Without ConnInfo on HandleRequest, the daemon would have to cache peer identity per session and trust the cache — error-prone, race-prone (session reuse across tenants on Windows named pipes)."

> "All these dispatch decisions need verified peer identity at call time."

The struggle has two layers:
1. **No identity** — handlers cannot tell who is calling (PID/UID invisible)
2. **No gate** — every handshake-complete session is automatically dispatched, no opportunity to reject before resource allocation

Verbatim from issue #111:
> "a hostile shim from Tenant A must NOT be allowed to spawn a session that the daemon will subsequently dispatch tool calls or log_forward notifications from on behalf of Tenant B (or as if it were daemon-internal)."

> "Today muxcore accepts every session that completes the handshake; aimux can only filter at dispatch time, which is too late — the session is already established, queued frames count against shared resources."

A third layer — no per-frame admission control:
Verbatim from issue #112:
> "a hostile shim from Tenant A must NOT be able to flood the daemon with `notifications/aimux/log_forward` frames at a rate that starves Tenant B's legitimate traffic."

> "Without an inbound hook, aimux can only rate-limit at dispatch time — by which point the frame has already consumed reader-goroutine cycles and queue space."

## Workaround

The user has tried:
1. Caching peer identity per session at handshake time and trusting the cache during dispatch — **rejected**: "error-prone, race-prone (session reuse across tenants on Windows named pipes)"
2. Filtering at dispatch time inside aimux handler — **rejected**: "too late — the session is already established, queued frames count against shared resources"
3. Rate-limiting at handler entry inside aimux — **rejected**: "the frame has already consumed reader-goroutine cycles and queue space"
4. Dropping the FR-12 enforcement promise temporarily — **partial**: aimux had to do "CR-002 honesty rewrite, 2026-04-28" stating the verification cannot be enforced "until upstream API exposes the connection"

External rate-limiter sidecar was considered and dismissed:
> "External rate limiter sidecar — adds operational complexity. Single-process aimux deployments are the target."

## Friend-Summary

If telling a colleague over coffee, the user would say: "I'm building tenant isolation in aimux on top of muxcore. The runtime knows who's connecting at the OS level — that's a Windows named-pipe HANDLE I can ask `GetNamedPipeClientProcessId` against, or a Unix socket with SO_PEERCRED. But muxcore swallows that and only hands me a hashed CWD identifier. So my tenant-routing code has nothing real to route on. I need three things: (1) the peer's actual PID and UID in every handler call, (2) a one-shot 'is this peer allowed in at all?' gate before muxcore commits any resources to the session, and (3) a per-frame admission hook so I can fairly distribute frame budget between tenants without paying full dispatch cost on dropped frames. All three additive, no breaking changes — old consumers must compile unchanged."

---

**Evidence: 8 verbatim quotes across 4 source issues. Author: thebtf (also project owner). Synthesized from real GitHub issues, not proxy evidence — `PHASE_0_COMPLETE`.**

**FR-to-evidence mapping (for Phase 2):**
- FR-1/FR-2 (peer credentials on Notification + Session handlers) ← #109 + #110 quotes 1-4
- FR-3 (AuthorizeSession single-shot gate) ← #111 quotes 5-6
- FR-4 (OnFrameReceived per-frame hook) ← #112 quotes 7-8
- All FRs share invariant "additive only, zero breaking changes" ← unanimous across all 4 issues
