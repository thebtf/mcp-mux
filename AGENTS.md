# mcp-mux — MCP Stdio Multiplexer

## STACKS

```yaml
STACKS: [GO]
```

## PROJECT OVERVIEW

A local stdio multiplexer/proxy for MCP (Model Context Protocol) servers. Allows multiple Claude Code sessions to share a single instance of each MCP server, reducing process count and memory usage by ~3x.

### Problem

Each Claude Code session spawns its own copy of every configured MCP server (stdio transport). With 4 parallel sessions × 12 MCP servers = ~48 node processes consuming ~4.8 GB RAM. Most of these servers are stateless — they don't need per-session isolation.

### Solution

`mcp-mux` is a transparent command wrapper. User prefixes their MCP server command with `mcp-mux`:

```json
{ "command": "mcp-mux", "args": ["uvx", "--from", "...", "serena", ...] }
```

First mcp-mux instance for a given server becomes the "owner" (spawns upstream, listens on IPC). Subsequent instances connect as clients.

```
CC 1 ──stdio──> mcp-mux ──IPC──┐
CC 2 ──stdio──> mcp-mux ──IPC──┤──> mcp-mux (owner) ──stdio──> engram (1×)
CC 3 ──stdio──> mcp-mux ──IPC──┤
CC 4 ──stdio──> mcp-mux ──IPC──┘
```

### Key Concepts

- **Upstream**: A real MCP server process (e.g., engram, tavily)
- **Downstream**: A CC session connecting via mcp-mux wrapper
- **Owner**: First mcp-mux instance — spawns upstream, accepts IPC connections
- **Client**: Subsequent mcp-mux instances — connect to owner via IPC
- **Shared** (default): One upstream serves all clients
- **Isolated** (`--isolated`): Each client gets its own upstream

## CONVENTIONS

- Investigation reports: `.agent/reports/YYYY-MM-DD-topic.md`
- Diagnostic data: `.agent/data/topic.md`
- Action plans: `.agent/plans/topic.md`
- Specifications: `.agent/specs/topic.md`
- Architecture decisions: `.agent/arch/decisions/NNN-title.md`

## RULES

| Rule | Description |
|------|-------------|
| **No stubs** | Complete, working implementations only |
| **No guessing** | Verify with tools before using |
| **Reasoning first** | Document WHY before implementing |
| **Spec compliance** | MCP protocol spec is authoritative — verify all protocol behavior against it |

## muxcore Library API (v0.20.4)

### Upgrade

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.21.19
```

v0.21.10 fixes conn flush before shutdown — explicit `conn.Close()` before
`afterFn()` ensures response reaches the caller. v0.21.9's `afterFn`
callback was necessary but insufficient: `defer conn.Close()` never ran
because Shutdown completed instantly post-handoff, killing the goroutine.

v0.21.9 introduced the `afterResponse func()` callback pattern.
**Breaking:** `DaemonHandler.HandleGracefulRestart` signature gains a
third return value `afterResponse func()`.

v0.21.8 fixes control socket conflict during graceful restart handoff.
`daemon.New()` bound the control socket before `loadSnapshot`/`tryHandoffReceive`
— predecessor still held it → successor failed → handoff never reached.
Fix: in handoff mode (env vars present), `loadSnapshot`/`tryHandoffReceive`
run first, then `retryControlBind` polls until predecessor releases the
socket. Non-handoff path unchanged. No breaking API changes.

v0.21.7 fixes `spawnSuccessor` hardcoding `"--daemon"` instead of using
`engine.Config.DaemonFlag` (default `"--muxcore-daemon"`). Both v0.21.7
and v0.21.8 are required for graceful restart to work end-to-end.

v0.20.4 is the multi-user hardening release on top of v0.20.3. Closes the
2 HIGH security findings from the 2026-04-18 PRC audit (S8-001 token
handshake enforcement, S5-001 Unix socket 0600 permissions). Ships FR-28
and FR-29 from the post-audit-remediation spec amendment. No breaking API
changes — two additive exports (`muxcore.ListenUnix`, `SessionManager.IsPreRegistered`).

For historical v0.19.3 concurrency fixes, see [muxcore/v0.19.3 release notes](https://github.com/thebtf/mcp-mux/releases/tag/muxcore%2Fv0.19.3).

| Fix | Where | Severity |
|-----|-------|----------|
| Owner Serve loop CPU spin after failed background spawn (BUG-001) | `owner/owner.go` | P1 |
| `ownerNotifier.Notify` held RLock across 30s write deadline (BUG-002) | `owner/owner.go` | P1 |
| JSON escape regression in `dispatchToSessionHandler` — invalid JSON on Windows paths/quotes (H1) | `owner/owner.go` | P1 |
| `daemon.cleanupDeadOwner` TOCTOU identity gap (BUG-003) | `daemon/daemon.go` | P1 |
| `control.Server handleConn` no read/write deadlines — silent-client DoS (BUG-004) | `control/server.go` | P1 |
| `daemon.Spawn` recursion on stuck placeholder + findSharedOwner argv collision | `daemon/daemon.go` | P2 |
| `drainOrphanedInflight` silent stdout write failures — CC hang with no log | `owner/resilient_client.go` | P2 |
| `findSharedOwner` lock-semantics doc ambiguity + stale-iterator hazard | `daemon/daemon.go` | P2 |

Every fix ships with at least one regression test. Full release notes:
https://github.com/thebtf/mcp-mux/releases/tag/muxcore/v0.19.3

v0.19.2 is a bug fix release on top of v0.19.1 — fixes a recursive goroutine
leak in `daemon.Spawn` when a concurrent placeholder wait times out. No API
changes, zero consumer code modifications required.

v0.19.1 was a refactor-only release on top of v0.19.0 — zero behaviour change.
Adds `engine.Config.SkipSnapshot` as an opt-in field (zero-value preserves the
prior hardcoded default). All v0.19.0 migration notes below still apply.

### Upgrade from v0.18.x

#### Breaking changes

| Change | Migration |
|--------|-----------|
| `DaemonControlPath(baseDir)` → `DaemonControlPath(baseDir, name)` | Add engine name as second arg. Empty string = "mcp-mux" (backward compat). engine.Config.Name handles this automatically. |
| `DaemonLockPath(baseDir)` → `DaemonLockPath(baseDir, name)` | Same as above. |

#### New: SessionHandler (replaces Handler for multi-session awareness)

```go
import muxcore "github.com/thebtf/mcp-mux/muxcore"

type MyHandler struct{}

func (h *MyHandler) HandleRequest(ctx context.Context, p muxcore.ProjectContext, req []byte) ([]byte, error) {
    // p.ID  — deterministic project ID (from worktree root hash)
    // p.Cwd — CC session working directory
    // p.Env — per-session env diff (API keys, config)
    return myServer.Handle(req)
}

// Optional: lifecycle hooks
func (h *MyHandler) OnProjectConnect(p muxcore.ProjectContext)    { /* new CC session */ }
func (h *MyHandler) OnProjectDisconnect(projectID string)         { /* CC session left */ }

// Optional: targeted notifications
func (h *MyHandler) SetNotifier(n muxcore.Notifier) { h.notifier = n }
// Then: h.notifier.Notify(projectID, notification) — to one session
//       h.notifier.Broadcast(notification)          — to all sessions

// Optional: client notification handling
func (h *MyHandler) HandleNotification(ctx context.Context, p muxcore.ProjectContext, data []byte) {
    // receives notifications/cancelled etc from CC
}

// Wire it:
e, _ := engine.New(engine.Config{
    Name:           "aimux",
    SessionHandler: &MyHandler{},
})
e.Run(ctx)
```

Legacy `Handler func(ctx, stdin, stdout)` still works. If both set, SessionHandler wins.

#### New: upgrade.Swap (binary update for engine consumers)

```go
import "github.com/thebtf/mcp-mux/muxcore/upgrade"

// Atomic binary swap: current → .old.{pid}, new → current
oldPath, err := upgrade.Swap(currentExe, newExe)

// Clean stale binaries (.old.*, .bak, ~~)
cleaned := upgrade.CleanStale(exePath)
```

For full graceful restart with snapshot serialization, the daemon-specific logic
(signal, wait, re-start) remains in the consumer's main.go — `upgrade.Swap` handles
only the atomic file rename.

#### Bug fixes included in v0.18.0–v0.19.0

| Fix | Issue |
|-----|-------|
| Full env passthrough — remove diffEnv, pass complete session env | #50 |
| Stale daemon socket cleanup for engine consumers | #48 |
| Daemon CPU spin (60-80%) when owner shutdown with nil upstream | #46 |
| Crash circuit breaker (5 crashes/60s → spawn rejected) | #43 |
| CWD-aware dedup (no cross-project context leaks) | #42 |
| Per-engine daemon sockets (no collision between mcp-mux + aimux) | #43 |
| `_meta` injection for in-process handlers | #44 |
| SpawnUpstreamBackground HandlerFunc fix | #41 |

### For aimux

```bash
cd aimux && go get github.com/thebtf/mcp-mux/muxcore@v0.19.3
```

Key changes to adopt:
1. **SessionHandler** — replace `srv.StdioHandler()` with SessionHandler implementation to get per-CC-session ProjectContext
2. **upgrade.Swap** — replace manual binary rename with `upgrade.Swap(exe, newExe)` + `upgrade.CleanStale(exe)`
3. **v0.19.3 concurrency fixes** — included automatically, no code changes needed. The CPU-spin-on-failed-background-spawn (BUG-001) and ownerNotifier.Notify RLock hold (BUG-002) are both hidden behind existing aimux code paths and required no consumer-side changes.
4. **Circuit breaker** — included automatically, protects against upstream crash loops

### For engram

```bash
cd engram && go get github.com/thebtf/mcp-mux/muxcore@v0.19.3
```

Key changes:
1. **DaemonControlPath** — if you call it directly, add name parameter: `DaemonControlPath(baseDir, "engram")`
2. **v0.19.3 concurrency fixes** — included automatically
3. **SessionHandler** — optional. engram can stay on legacy `Handler` until multi-session support is needed

### v0.21.10 — Flush conn before afterFn (#99)

- v0.21.9 `afterFn` ran after `writeResponse` but `defer conn.Close()` never executed — post-handoff Shutdown completes instantly (0 owners), process exits, goroutine killed, kernel send buffer lost.
- **Fix:** explicit `conn.Close()` before `afterFn()` in `handleConn`. 1 line.
- **No breaking changes.** No API changes.

### v0.21.9 — Defer shutdown until after response write (#99)

- `HandleGracefulRestart` called `go d.Shutdown()` before the control handler wrote the response. On Windows AF_UNIX, data in the kernel send buffer is lost on process exit → caller sees `i/o timeout` instead of confirmation.
- **Fix:** `HandleGracefulRestart` returns `afterResponse func()` callback. Control server writes response first, then invokes callback.
- **Breaking:** `DaemonHandler.HandleGracefulRestart` signature: `(int) (string, func(), error)`. Consumers must update implementations and mocks.
- **Combined with v0.21.7 + v0.21.8:** all three fixes required for end-to-end graceful restart with confirmed response.

### v0.21.8 — Defer control socket binding in handoff mode (#99)

- `daemon.New()` bound the control socket **before** `loadSnapshot`/`tryHandoffReceive`. Predecessor still holds socket → successor fails → handoff never reached → 30s fallback.
- **Fix:** in handoff mode (env vars present), `loadSnapshot`/`tryHandoffReceive` run first, then `retryControlBind` polls (500ms × 60 = 30s max) until predecessor releases the socket via `Shutdown()`. Non-handoff path unchanged.
- **No breaking changes.** Internal constructor reordering only.
- **Combined with v0.21.7:** both fixes required for graceful restart to work end-to-end (v0.21.7 = DaemonFlag, v0.21.8 = socket ordering).

### v0.21.7 — Fix spawnSuccessor DaemonFlag hardcode (#99)

- `spawnSuccessor` hardcoded `"--daemon"` as the successor CLI flag. `engine.isDaemonMode()` checks `cfg.DaemonFlag` (default `"--muxcore-daemon"`). Mismatch caused successor to enter client mode — `tryHandoffReceive` never ran — graceful restart always fell back to kill-and-respawn after 30s.
- **Fix:** `daemon.Config` gains `DaemonFlag string` field. `engine.runDaemon` passes `e.cfg.DaemonFlag`. `spawnSuccessor` uses configured flag. Empty value defaults to `"--daemon"` for backward compat.
- **No breaking changes.** Additive field only.

### v0.21.1 — Shim reconnect token refresh (F2)

- **`HandleRefreshSessionToken(prevToken string) (newToken string, err error)`** added to `control.DaemonHandler` — lets a shim mint a fresh handshake token for the same owner after its original token is consumed, without triggering a full owner respawn.
- **`session.Manager` bound history** — `Bind` now records a 30-min TTL entry keyed by token; `RegisterReconnect(prev, ownerAlive)` looks up that history and returns a new pending token, or `ErrUnknownToken` / `ErrOwnerGone`.
- **`ResilientClientConfig.RefreshToken` + `MaxRefreshAttempts`** (default 3) — shim tries `HandleRefreshSessionToken` up to N times before falling back to the full `HandleSpawn` path. Fallback also fires immediately on `ErrOwnerGone`. Zero-value `RefreshToken` field preserves pre-F2 behaviour (skip refresh entirely).
- **Structured markers and counters** — `shim.reconnect.refresh_ok|refresh_fail|fallback_spawn` log markers; `shim_reconnect_refreshed`, `shim_reconnect_fallback_spawned`, `shim_reconnect_gave_up` counters exposed via `HandleStatus`. Note: `fallback_spawned` increments only when `HandleSpawn` is called with `ReconnectReason == "fallback_spawn"` (v0.21.x shims only); legacy shims that call bare `HandleSpawn` are invisible to all three counters.
- **Back-compat** — pre-v0.21.1 shims that have no knowledge of `refresh-token` are still rejected cleanly after their token is consumed; they recover via the existing `HandleSpawn` path, identical to a cold first-time spawn from the daemon's perspective.
- **Breaking change (internal API)** — `session.Manager.Bind` signature extended from `Bind(token string, session *Session)` to `Bind(token, ownerKey string, session *Session)`. Only call site is `owner.acceptLoop`; external consumers of muxcore do not call `Bind` directly.

### v0.21.2 — Engine accessors

- Adds read-only accessors to the `engine.Engine` type for observability and testing: `OwnerCount()`, `SessionCount()`, `HandleStatus()`, and `Entry(serverID)`. No breaking changes; all additive.

### v0.21.4 — Defensive ipc.Listen + upgrade --restart split-state fix

- **`ipc.Listen` now refuses to rebind an actively-served path.** Before removing the stale socket file and calling `sockperm.Listen`, `Listen` calls `IsAvailable`. If a live listener is detected, it returns `"ipc: listener already active at <path> (another process is serving)"`. Callers that previously relied on silent socket-steal semantics will now receive a loud error instead. This is a breaking behavioural change, but such reliance was always a bug — a second `Listen` on an active path would have silently disconnected all existing clients. The guard turns that into a diagnosable failure; the caller (snapshot restore, owner startup) can log and skip the conflicting owner rather than corrupting it.

- **`upgrade --restart` fallback-shutdown branch now waits for old daemon exit.** Before spawning the new daemon, `runUpgrade` mirrors the graceful-restart branch's 20×500 ms `isDaemonRunning` poll into both fallback sub-branches (`graceful-restart not available` and `graceful-restart failed`). Prevents the race where the new daemon rebinds per-owner sockets via `ipc.Listen` while the old daemon's Owner structs and `sessionMgr` are still live, causing shim handshake tokens registered in the new daemon to be routed to the old daemon's accept loop and rejected.

- **No API changes.** Both fixes are additive or strictly defensive. `ipc.Listen` signature is unchanged; callers that passed a stale-file path continue to work. The poll loop in `runUpgrade` is internal to the binary.

- **Bundled release:** muxcore/v0.21.4 + v0.21.4 binary.

- **Investigation:** `.agent/debug/upgrade-restart-split-state/investigation.md`

### v0.21.3 — OwnerConfig.UpstreamWriter (proposed in PR #93)

- `owner.OwnerConfig.UpstreamWriter io.Writer` — optional field that, when set, replaces the default subprocess stdout pipe with a caller-supplied writer. Enables in-process upstream implementations that do not want to go through a subprocess. PR #93 is open at time of writing; adopt after merge.

## INSTRUCTION HIERARCHY

```
System prompts > Task/delegation > Global rules > Project rules > Defaults
```
