# Feature: Session-Aware Handler API

**Slug:** session-handler
**Created:** 2026-04-14
**Status:** Draft
**Author:** AI Agent (reviewed by user)

> **Provenance:** Specified from architecture.md produced during v0.17.1 session.
> Evidence from: muxcore/owner/owner.go routing code, muxcore/upstream/process.go
> NewProcessFromHandler, aimux multi-session problem analysis, v0.17.1 _meta hotfix.
> Confidence: VERIFIED (all code paths traced in this session).

## Overview

Add a structured session-aware handler interface to muxcore so that in-process
MCP handlers can receive per-request session identity, lifecycle events, and
targeted notification delivery — without parsing `_meta` from raw JSON-RPC bytes.

## Context

muxcore multiplexes N Claude Code sessions through one in-process handler via
`io.Pipe`. The handler sees a flat byte stream where all sessions are interleaved.
v0.17.1 added `_meta` injection as a hotfix, but handlers must parse it manually
and still lack lifecycle awareness (connect/disconnect) and targeted notifications.

Consumers (aimux) need session identity to:
- Isolate jobs/state per CC session
- Route CWD-dependent operations
- Clean up state on disconnect
- Rate-limit per session
- Send progress to the originating session

The current `Handler func(ctx, stdin, stdout)` signature cannot support these
capabilities without breaking the abstraction boundary.

## Functional Requirements

### FR-1: SessionHandler interface
The system must provide a `SessionHandler` interface with a single method
`HandleRequest(ctx, ProjectContext, request) (response, error)` that Owner
calls directly instead of writing to a pipe. ProjectContext carries session ID,
CWD, and env diff as a value struct.

### FR-2: Concurrent dispatch
Owner must call `HandleRequest` concurrently from multiple goroutines (one
per incoming request). The handler is responsible for its own synchronization.
The context must be cancelled when the originating CC session disconnects or
the owner shuts down. Owner applies a context deadline from `x-mux.toolTimeout`
(or a default) as a safety net — handler can finish earlier via its own
cancellation. If deadline fires, Owner returns a JSON-RPC timeout error to
the session.

### FR-3: Original IDs
`HandleRequest` must receive the original JSON-RPC request bytes with the
client's original ID — not the remapped `s1:n:5` format. Owner handles
remapping transparently. The handler never sees mux-internal ID prefixes.

### FR-4: Session lifecycle hooks
The system must support optional `OnProjectConnect(ProjectContext)` and
`OnProjectDisconnect(projectID)` callbacks via a separate interface
(`ProjectLifecycle`). Owner calls these when CC sessions join or leave.

### FR-5: Targeted notifications
The system must allow the handler to send JSON-RPC notifications to a
specific CC session or broadcast to all sessions. This is exposed via
a `Notifier` interface that Owner implements and passes to the handler
via optional `SetNotifier` callback.

### FR-6: Backward compatibility
The existing `Handler func(ctx, stdin, stdout)` must continue to work
unchanged. `engine.Config` accepts both `Handler` and `SessionHandler`.
If both are set, `SessionHandler` takes priority. Legacy handlers use
the pipe path with `_meta` injection (v0.17.1 behavior).

### FR-7: Request routing with caching
For `SessionHandler` consumers: the first initialize request goes to
`HandleRequest` — handler produces the response, Owner caches it.
Subsequent sessions get the cached replay (same as pipe path).
Handler sees initialize and can customize capabilities.
For tools/list, prompts/list etc: same pattern — first call goes to
handler, response is cached, subsequent sessions get replay.

### FR-8: Client notification handling
The system must support an optional `NotificationHandler` interface with
`HandleNotification(ctx, ProjectContext, notification []byte)`. Owner checks
via type assertion. Handlers that don't implement it: client notifications
(e.g. `notifications/cancelled`) are handled by Owner internally or dropped.
`HandleRequest` is never called with notifications — clean type separation.

### FR-9: Upgrade mechanism for engine consumers
muxcore must export the upgrade/restart mechanism currently implemented in
`cmd/mcp-mux/main.go` (atomic rename + graceful restart with snapshot
serialization) as a library API. Engine consumers (aimux, engram) must be
able to call `engine.Upgrade(newBinaryPath)` instead of manual binary swap.
Ref: github.com/thebtf/mcp-mux/issues/45

## Non-Functional Requirements

### NFR-1: Performance
`HandleRequest` dispatch overhead must be less than the current pipe path
(no serialization/deserialization of `_meta`, no ID remap round-trip).
Zero-copy where possible — request bytes passed through, not cloned.

### NFR-2: API surface
Total new exported types: max 6 (ProjectContext, SessionHandler,
ProjectLifecycle, Notifier, NotifierAware — plus engine.Config field).
No new packages. Types defined in `muxcore/` root or `muxcore/handler.go`.

### NFR-3: Test coverage
Each new interface must have at least one integration test using a mock
handler. The dispatch path must be tested with concurrent requests from
multiple sessions. Lifecycle hooks must be tested for connect/disconnect
ordering.

### NFR-4: Zero breaking changes
No existing public API signature changes. New fields are additive.
`go get muxcore@v0.18.0` must compile without changes for existing consumers.

## User Stories

### US1: Session-grouped job tracking (P1)
**As an** aimux developer, **I want** each `HandleRequest` to include the
CC session identity, **so that** I can group jobs by originating session
and implement "cancel all my jobs".

**Acceptance Criteria:**
- [ ] HandleRequest receives ProjectContext with non-empty ID for each request
- [ ] Two concurrent sessions produce different ProjectContext.ID values
- [ ] ProjectContext.Cwd matches the CC session's working directory

### US2: Session cleanup on disconnect (P1)
**As an** aimux developer, **I want** to receive a callback when a CC
session disconnects, **so that** I can cancel running jobs and clean up
per-session state.

**Acceptance Criteria:**
- [ ] OnProjectDisconnect called exactly once per session when CC closes
- [ ] OnProjectDisconnect called with the same ID as OnProjectConnect
- [ ] Handler can cancel running jobs in OnProjectDisconnect (ctx not yet cancelled)

### US3: Per-session progress notifications (P2)
**As an** aimux developer, **I want** to send progress notifications
to a specific CC session, **so that** session A doesn't see progress
for session B's jobs.

**Acceptance Criteria:**
- [ ] Notifier.Notify(sessionID, data) delivers to exactly that session
- [ ] Notifier.Notify with invalid sessionID returns error (not panic)
- [ ] Notifier.Broadcast delivers to all connected sessions

### US4: Legacy handler unchanged (P1)
**As an** engram developer, **I want** my existing `Handler func(ctx, stdin, stdout)`
to keep working without code changes after upgrading muxcore.

**Acceptance Criteria:**
- [ ] Existing engine.Config{Handler: myFunc} compiles and works on v0.18.0
- [ ] Legacy handler receives _meta injection (v0.17.1 behavior preserved)
- [ ] No new required fields in engine.Config

## Edge Cases

- **Handler panics in HandleRequest:** Owner recovers, returns JSON-RPC internal
  error to the session. Does not crash the daemon.
- **Session disconnects mid-request:** ctx cancelled, handler observes cancellation.
  Response (if produced) is discarded — session IPC already closed.
- **Notify after disconnect:** `Notifier.Notify` returns error. Handler must
  handle gracefully (not crash).
- **Concurrent connect/disconnect:** `OnProjectConnect` and `OnProjectDisconnect`
  may be called concurrently for different sessions. Never called concurrently
  for the SAME session (connect happens-before disconnect).
- **Both Handler and SessionHandler set:** SessionHandler wins. Handler field
  is ignored with a log warning.
- **SessionHandler is nil and Handler is nil:** Owner logs error and rejects
  spawn (existing behavior — no upstream and no handler).

## Out of Scope

- **Streaming responses:** `HandleRequest` returns complete response.
  Streaming (SSE, chunked) requires a different method signature — deferred.
- **Request batching:** JSON-RPC batch requests are not supported by MCP.
- **Multi-handler routing:** One owner = one handler. No per-method dispatch.
- **Handler hot-reload:** Handler is set at construction, not swappable at runtime.
- **Subprocess session awareness:** Subprocess upstreams continue using
  `_meta` injection or `x-mux.sharing` capability. SessionHandler is
  in-process only.

## Dependencies

- muxcore/owner: routing logic (handleSessionRequest, session management)
- muxcore/upstream: NewProcessFromHandler (legacy path, unchanged)
- muxcore/session: SessionManager, Session struct (ID, Cwd, Env fields)
- muxcore/jsonrpc: message parsing (existing)
- No external dependencies

## Success Criteria

- [ ] aimux can implement SessionHandler and receive ProjectContext per request
- [ ] aimux can track per-CC-session state via lifecycle hooks
- [ ] aimux can send progress to specific CC session via Notifier
- [ ] Existing consumers (engram) upgrade without code changes
- [ ] No performance regression vs pipe path (benchmark)
- [ ] All tests pass on Linux, macOS, Windows

## Open Questions

1. ~~**Initialize request routing:**~~ **RESOLVED** — Handler receives
   initialize requests via `HandleRequest`. Owner still caches the response
   for replay, but handler can customize the response (e.g. capabilities).
   This gives consumers full control without forcing it.

2. ~~**Notification FROM client:**~~ **RESOLVED** — Separate optional
   `NotificationHandler` interface with `HandleNotification(ctx, ProjectContext, []byte)`.
   Handler that doesn't implement it: notifications are dropped (Owner handles
   `cancelled` internally). Clean type separation — HandleRequest always returns response.

3. ~~**ProjectContext identity stability:**~~ **RESOLVED** — ID is a deterministic
   hash of the **worktree root** (not CWD). Same worktree = same ID across CC
   restarts. Different worktree of same repo = different ID (separate session).
   
   Worktree root resolution:
   - `.git` is a directory → root = that directory (main checkout)
   - `.git` is a file (`gitdir: ...`) → root = that directory (linked worktree,
     NOT resolved back to main repo — each worktree is a separate session)
   - No `.git` found → fall back to canonical CWD
   
   CWD subdirectories within the same worktree produce the same session ID.
   This means aimux can use ProjectContext.ID as a persistent key for jobs,
   state, and rate counters.

## Clarifications

### Session 2026-04-14

| # | Category | Question | Resolution | Date |
|---|----------|----------|------------|------|
| C1 | Functional | Notifications from CC — via HandleRequest or separate interface? | Separate `NotificationHandler` interface (optional) | 2026-04-14 |
| C2 | Data Model | ProjectContext.ID stability contract | Deterministic from worktree root; no `.git` → canonical CWD | 2026-04-14 |
| C3 | Functional | Handler sees initialize requests? | Yes — handler can customize capabilities, Owner caches response | 2026-04-14 |
| C4 | Reliability | HandleRequest timeout | Both: Owner applies toolTimeout deadline, handler can finish earlier | 2026-04-14 |
| C5 | Terminology | "Session" overload (CC/mux/aimux) | Rename to `ProjectContext` / `ProjectLifecycle` | 2026-04-14 |
