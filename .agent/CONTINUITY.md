# Continuity State

**Last Updated:** 2026-03-17
**Session:** Global Daemon Architecture — DEPLOYED & VERIFIED

## Done

### Phase 1 PoC (2026-03-15)
- Full multiplexer: jsonrpc, remap, serverid, ipc, upstream, mux, client
- E2E verified, 52 tests

### Transparent Multiplexing H1 (2026-03-16)
- Cache + replay: initialize, tools/list, prompts/list, resources/list, resources/templates/list
- Tool name auto-classifier + auto-enforce (close IPC listener)

### MCP Protocol Full Coverage (2026-03-16)
- sampling/createMessage, elicitation, ping routing
- notifications/cancelled remapping, initialized suppression
- x-mux capability parsing, session-aware mode, _meta.muxSessionId injection
- Cache invalidation, fingerprint validation

### Global Daemon Architecture (2026-03-17) — DEPLOYED
**Phase 1: Owner Decoupling**
- OnZeroSessions, OnUpstreamExit, OnPersistentDetected callbacks
- Nil callbacks = legacy behavior (backward compatible)

**Phase 2: Daemon Core**
- internal/daemon/daemon.go — manages N owners, spawn/remove/status
- control.DaemonHandler interface, spawn/remove commands
- serverid.DaemonControlPath(), DaemonLockPath()

**Phase 3: Reaper/GC**
- internal/daemon/reaper.go — 10s sweep, grace periods, idle auto-exit
- Persistent re-spawn, zombie detection

**Phase 4: Shim + Auto-Start**
- cmd/mcp-mux/daemon.go — ensureDaemon(), spawnViaDaemon(), startDaemonProcess()
- File lock prevents concurrent daemon spawns (LockFileEx/flock)
- Platform-specific detach: HideWindow (Windows), Setsid (Unix)
- Daemon mode is DEFAULT (MCP_MUX_NO_DAEMON=1 to disable)

**Phase 5: Persistence**
- classify.ParsePersistent() — x-mux.persistent: true
- Owner → daemon callback on detection

**Phase 6: Status/Stop/Upgrade**
- mcp-mux status queries daemon first, falls back to per-server scan
- mcp-mux stop stops daemon + legacy instances
- mcp-mux upgrade: atomic rename-swap (build .exe~ → stop → swap → done)

**Control Plane MCP Server (mcp-mux serve)**
- instructions field in initialize response
- prompts: mux-guide, mux-status-summary
- Improved tool descriptions
- Added to user scope ~/.claude.json

**Fixes Applied:**
- Console windows on Windows (HideWindow: true, removed DETACHED_PROCESS conflict)
- Daemon spawn race (LockFileEx file lock)
- CC env vars forwarding to daemon (env diff: only CC-specific vars)
- Spawn timeout increased to 30s (uvx/npx startup time)
- mux_version auto-detected from git hash (debug.ReadBuildInfo)
- protocolVersion: 2025-11-25 (verified via GitHub API)

## Verified
- 15/16 servers running through daemon (aimux = isolated, separate)
- mux_version: 7601535c-dirty (all owners on same version)
- No console windows on startup
- cclsp and nia working (env forwarding confirmed)
- Atomic upgrade flow tested end-to-end
- All tests green (10 packages)

## Architecture
```
┌──────────────────────────────────────────────┐
│  mcp-muxd (Daemon)                            │
│  Owner A (engram) ──stdio──> upstream          │
│  Owner B (tavily) ──stdio──> upstream          │
│  Owner C (aimux)  ──stdio──> upstream          │
│  Reaper (10s sweep, grace periods)            │
│  Control: mcp-muxd.ctl.sock                   │
└──────────────────────────────────────────────┘
     ▲ IPC (per-server .sock)
     │
  Shim (thin: ensureDaemon → spawn → RunClient)
     │ stdio
  CC Session
```

## Key Files
- internal/daemon/daemon.go, reaper.go (+tests)
- cmd/mcp-mux/daemon.go, daemon_windows.go, daemon_unix.go
- cmd/mcp-mux/main.go (shim flow, upgrade, collectEnv)
- internal/mcpserver/server.go (instructions, prompts)
- internal/mux/owner.go (callbacks, version, checkPersistent)
- internal/control/protocol.go (DaemonHandler, spawn fields)

## Git
- master: 8b6d8ae (diag: timing logs — latest)
- Prior: 7601535 (spawn timeout), fc73b3e (env diff), 4943ce9 (env forwarding), e29f4f0 (console fix), cc6f74d (daemon architecture)

## Next
- Remove timing diagnostic logs (after confirming stability)
- Test daemon survival across CC restarts (grace period)
- Async spawn + request queue (defer — not needed now, spawn is 0.1s)
- Migrate mcp-mux to user scope (~/.claude.json) for all projects
- PR to main branch

## Blockers
None
