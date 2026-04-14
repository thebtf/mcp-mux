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

## muxcore Library API (v0.18.0)

### Upgrade from v0.17.x

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.18.0
```

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

#### Bug fixes included in v0.18.0

| Fix | Issue |
|-----|-------|
| Daemon CPU spin (60-80%) when owner shutdown with nil upstream | #46 |
| Crash circuit breaker (5 crashes/60s → spawn rejected) | #43 |
| CWD-aware dedup (no cross-project context leaks) | #42 |
| Per-engine daemon sockets (no collision between mcp-mux + aimux) | #43 |
| `_meta` injection for in-process handlers | #44 |
| SpawnUpstreamBackground HandlerFunc fix | #41 |

### For aimux

```bash
cd aimux && go get github.com/thebtf/mcp-mux/muxcore@v0.18.0
```

Key changes to adopt:
1. **SessionHandler** — replace `srv.StdioHandler()` with SessionHandler implementation to get per-CC-session ProjectContext
2. **upgrade.Swap** — replace manual binary rename with `upgrade.Swap(exe, newExe)` + `upgrade.CleanStale(exe)`
3. **CPU spin fix** — included automatically, no code changes needed
4. **Circuit breaker** — included automatically, protects against upstream crash loops

### For engram

```bash
cd engram && go get github.com/thebtf/mcp-mux/muxcore@v0.18.0
```

Key changes:
1. **DaemonControlPath** — if you call it directly, add name parameter: `DaemonControlPath(baseDir, "engram")`
2. **CPU spin fix** — included automatically
3. **SessionHandler** — optional. engram can stay on legacy `Handler` until multi-session support is needed

## INSTRUCTION HIERARCHY

```
System prompts > Task/delegation > Global rules > Project rules > Defaults
```
