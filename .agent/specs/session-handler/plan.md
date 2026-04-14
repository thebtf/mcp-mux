# Implementation Plan: Session-Aware Handler API

**Spec:** .agent/specs/session-handler/spec.md
**Created:** 2026-04-14
**Status:** Draft

> **Provenance:** Planned from spec.md + architecture.md, both produced this session.
> Evidence from: muxcore/owner/owner.go (routing), muxcore/upstream/process.go
> (NewProcessFromHandler), muxcore/serverid/serverid.go (path resolution),
> muxcore/engine/engine.go (Config), cmd/mcp-mux/main.go (upgrade mechanism).
> Confidence: VERIFIED (all code paths traced).

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.23+ | Existing stack |
| Interfaces | Go interface + type assertion | Idiomatic optional interface pattern |
| ID generation | crypto/sha256 + canonical path | Deterministic, existing pattern in serverid.go |
| Upgrade | atomic rename + signal | Existing pattern in cmd/mcp-mux/main.go |
| Testing | go test + mock handler | No new deps |

## Architecture

```
Owner dispatch:
  if sessionHandler != nil → dispatchToSessionHandler()  [NEW]
  else if handlerFunc != nil → pipe + _meta injection    [EXISTING]
  else                       → subprocess upstream       [EXISTING]
```

Two new files, rest is edits to existing:
- `muxcore/handler.go` — type definitions (ProjectContext, SessionHandler, etc.)
- `muxcore/upgrade/upgrade.go` — exported upgrade mechanism

### Reversibility

| Decision | Tag | Rollback |
|----------|-----|----------|
| Add SessionHandler dispatch path in Owner | REVERSIBLE | Remove dispatch branch, delete handler.go |
| ProjectContext.ID from worktree root | REVERSIBLE | Change hash input, IDs reset |
| Export upgrade mechanism | REVERSIBLE | Unexport, keep internal |
| Dual Config fields (Handler + SessionHandler) | REVERSIBLE | Remove SessionHandler field |

## API Contracts

### ProjectContext (value struct)

```go
// muxcore/handler.go
type ProjectContext struct {
    ID  string            // sha256(worktreeRoot)[:16], deterministic
    Cwd string            // raw CWD from CC session
    Env map[string]string // per-session env diff
}
```

### SessionHandler (core interface)

```go
type SessionHandler interface {
    HandleRequest(ctx context.Context, project ProjectContext, request []byte) ([]byte, error)
}
```

### Optional interfaces

```go
type ProjectLifecycle interface {
    OnProjectConnect(project ProjectContext)
    OnProjectDisconnect(projectID string)
}

type NotificationHandler interface {
    HandleNotification(ctx context.Context, project ProjectContext, notification []byte)
}

type Notifier interface {
    Notify(projectID string, notification []byte) error
    Broadcast(notification []byte)
}

type NotifierAware interface {
    SetNotifier(n Notifier)
}
```

### engine.Config addition

```go
type Config struct {
    // ... existing fields ...
    SessionHandler SessionHandler // new, optional, takes priority over Handler
}
```

### upgrade.Upgrade (FR-9)

```go
// muxcore/upgrade/upgrade.go
func Upgrade(currentExe, newExe string, restart bool) error
```

## File Structure

```
muxcore/
  handler.go          NEW — ProjectContext, SessionHandler, optional interfaces
  handler_test.go     NEW — unit tests for ProjectContext ID generation
  upgrade/
    upgrade.go        NEW — exported upgrade mechanism
    upgrade_test.go   NEW — tests
  owner/
    owner.go          EDIT — add dispatchToSessionHandler, lifecycle hooks, Notifier impl
    owner_test.go     EDIT — tests for new dispatch path
  engine/
    engine.go         EDIT — add SessionHandler to Config, pass to daemon
  serverid/
    serverid.go       EDIT — add WorktreeRoot(cwd) function (don't resolve linked worktrees)
    serverid_test.go  EDIT — test WorktreeRoot
  daemon/
    daemon.go         EDIT — pass SessionHandler through to Owner config
```

## Phases

### Phase 1: Types + ProjectContext ID (FR-1, FR-3, clarification C2)

**What:** Define types in `handler.go`. Add `WorktreeRoot()` to serverid.
Build `ProjectContext` from session CWD.

**Files:**
- `muxcore/handler.go` — NEW: ProjectContext, SessionHandler, ProjectLifecycle, NotificationHandler, Notifier, NotifierAware
- `muxcore/handler_test.go` — NEW: TestProjectContextID (worktree, linked worktree, no git, subdirectory)
- `muxcore/serverid/serverid.go` — ADD: `WorktreeRoot(cwd string) string` (like findGitRoot but does NOT resolve linked worktrees back to main repo)
- `muxcore/serverid/serverid_test.go` — ADD: TestWorktreeRoot cases

**Deliverable:** Types compile, ID generation tested.

**Why first:** Everything else depends on these types.

### Phase 2: Owner dispatch (FR-1, FR-2, FR-3, FR-7, clarification C3, C4)

**What:** Add `dispatchToSessionHandler` in Owner. Handle concurrent dispatch,
original IDs (no remap), ctx deadline from toolTimeout, cache routing.

**Files:**
- `muxcore/owner/owner.go` — ADD: `sessionHandler` field, `dispatchToSessionHandler()` method.
  In `handleSessionRequest`: if sessionHandler != nil → new path.
  First initialize goes to handler, Owner caches response.
  Concurrent: one goroutine per request.
  Ctx: deadline from toolTimeout, cancel on session disconnect.
  Panic recovery in goroutine.
- `muxcore/owner/owner.go` — ADD: `ownerNotifier` struct implementing Notifier.
  `Notify(projectID, data)` → lookup session by project ID → write to IPC.
  `Broadcast(data)` → write to all sessions.

**Tests:**
- Mock SessionHandler that echoes request with project ID appended
- Two sessions, concurrent requests → correct project IDs
- Timeout test: handler blocks, ctx deadline fires → timeout error response
- Panic test: handler panics → error response, daemon survives

**Deliverable:** SessionHandler dispatch works end-to-end with mock.

### Phase 3: Lifecycle hooks + NotificationHandler (FR-4, FR-5, FR-8, clarification C1)

**What:** Call OnProjectConnect/OnProjectDisconnect. Wire SetNotifier.
Route client notifications via HandleNotification.

**Files:**
- `muxcore/owner/owner.go` — In session accept: if handler.(ProjectLifecycle) → OnProjectConnect.
  In removeSession: if handler.(ProjectLifecycle) → OnProjectDisconnect.
  In NewOwner: if handler.(NotifierAware) → SetNotifier(ownerNotifier).
  In handleSessionRequest notification branch: if handler.(NotificationHandler) → HandleNotification.

**Tests:**
- Connect/disconnect ordering: connect before first request, disconnect after last
- Concurrent connect/disconnect for different projects
- Notify after disconnect → error
- NotificationHandler receives cancelled notification

**Deliverable:** Full lifecycle API working.

### Phase 4: Engine + daemon wiring (FR-6)

**What:** Add SessionHandler to engine.Config and daemon.Config. Pass through
to Owner. Verify backward compat.

**Files:**
- `muxcore/engine/engine.go` — ADD: SessionHandler field to Config. Pass to daemon.Config.
- `muxcore/daemon/daemon.go` — ADD: sessionHandler field. Pass to OwnerConfig.
- `muxcore/owner/owner.go` — OwnerConfig already has the field from Phase 2.

**Tests:**
- engine.Config{Handler: legacyFunc} → pipe path works (regression)
- engine.Config{SessionHandler: myHandler} → structured path works
- engine.Config{Handler: x, SessionHandler: y} → SessionHandler wins, log warning

**Deliverable:** Engine consumers can use SessionHandler.

### Phase 5: Upgrade mechanism (FR-9, issue #45)

**What:** Extract upgrade logic from cmd/mcp-mux/main.go into muxcore/upgrade/.
Atomic rename + graceful restart with snapshot serialization.

**Files:**
- `muxcore/upgrade/upgrade.go` — NEW: `Upgrade(currentExe, newExe string, restart bool) error`
  Extracted from `runUpgrade()` in main.go. Platform-specific: rename, signal, wait.
- `muxcore/upgrade/upgrade_windows.go` — Windows rename semantics
- `muxcore/upgrade/upgrade_unix.go` — Unix rename + signal
- `cmd/mcp-mux/main.go` — Refactor `runUpgrade()` to call `upgrade.Upgrade()`

**Tests:**
- Rename works (temp file)
- Stale binary cleanup

**Deliverable:** `upgrade.Upgrade()` callable from aimux.

## Library Decisions

| Component | Library | Version | Rationale |
|-----------|---------|---------|-----------|
| All | stdlib only | — | No new dependencies. Types, crypto/sha256, context — all stdlib |

## DX Review (Library/API consumer)

**TTHW (Time to Hello World):**

```go
// From go get to working SessionHandler: ~30 seconds
type MyHandler struct{}

func (h *MyHandler) HandleRequest(ctx context.Context, p muxcore.ProjectContext, req []byte) ([]byte, error) {
    log.Printf("project %s from %s", p.ID, p.Cwd)
    return myServer.Handle(req) // delegate to existing MCP server
}

e, _ := engine.New(engine.Config{
    Name:           "my-server",
    SessionHandler: &MyHandler{},
})
e.Run(ctx)
```

**Rating:** Champion (<2 min). One interface, one method, one struct.

## Unknowns and Risks

| Unknown | Impact | Resolution |
|---------|--------|-----------|
| Performance of goroutine-per-request vs pipe | MED | Benchmark in Phase 2; Go goroutines are cheap (~4KB stack) |
| WorktreeRoot on network filesystems (no .git) | LOW | Falls back to canonical CWD — correct behavior |
| Upgrade mechanism portability (Windows exe locking) | MED | Existing pattern in main.go already handles this |

## Phase Dependencies

```
Phase 1 (types + ID)
  ↓
Phase 2 (owner dispatch) ← depends on types
  ↓
Phase 3 (lifecycle + notifications) ← depends on dispatch
  ↓
Phase 4 (engine wiring) ← depends on dispatch
  |
Phase 5 (upgrade) ← independent, can parallel with 3-4
```

Phases 3, 4, 5 can run in parallel after Phase 2.
