# Spec: Init-Ready Gate — Block Daemon.Spawn Until Upstream Ready

## Problem Statement

When CC starts an MCP server via mcp-mux, `Daemon.Spawn()` returns the IPC path **immediately** after creating the owner, before the upstream process has responded to `initialize`. CC has a hard startup timeout (~5-10s). For slow-starting upstreams (serena via uvx ~15s, mcp-server-time via uvx ~8s), CC kills the shim process before init response arrives. The owner then has 0 sessions, gets reaped, and CC retries — creating an infinite spawn loop (267-305 spawns observed per server per session).

**Root cause investigation:** `.agent/reports/investigate-mcp-mux-spawn-probe-reaper-deadlock-cc-p-2026-04-07T12-21-04.md`

## Functional Requirements

### FR1: Init-Ready Channel on Owner
Owner must expose an `InitReady() <-chan struct{}` channel that is closed when:
- (a) The `initialize` response is cached (`cacheResponse("initialize")` succeeds), OR
- (b) The upstream process exits before init (crash, startup failure)

### FR2: Daemon.Spawn Blocks Until Init Ready
For **newly created** owners (not dedup path), `Daemon.Spawn()` must wait on `owner.InitReady()` before returning the IPC path to the shim. Timeout: 60 seconds.

- If init completes within timeout → return IPC path normally (init is cached, instant replay)
- If timeout expires → return error to shim (shim falls back to legacy owner mode)
- If upstream exits before init → return error to shim

### FR3: Shim Spawn Timeout Increase
`spawnViaDaemon` timeout must increase from 30s to 90s to accommodate slow-starting upstreams + the init-ready wait.

### FR4: First Session Connects to Init-Ready Owner
The first session (shim via `RunResilientClient`) must connect to the owner and immediately receive a cached `initialize` response. CC probe sees the init response instantly and marks the server as "connected".

### FR5: Dedup Path Unchanged
`findSharedOwner` returns existing owners with cached init — no additional wait needed. Only newly spawned owners require the init-ready gate.

## Non-Functional Requirements

### NFR1: No Behavioral Change for Fast Servers
Servers that start in < 5s (playwright, wsl, tavily) must not be affected. The init-ready wait adds at most a few hundred milliseconds for them.

### NFR2: Upstream Crash Detection
If the upstream process crashes during startup (before init response), the init-ready channel must be closed with an error indicator so Spawn returns error instead of blocking forever.

### NFR3: Concurrent Spawn Safety
Multiple concurrent Spawn calls for different servers must not block each other. The init-ready wait is per-owner, not global.

## Success Criteria

1. serena, cclsp, mcp-server-time all show "connected" on fresh CC session start
2. Zero respawn loops in daemon log (each server spawned exactly once)
3. Fast servers (playwright, wsl) behavior unchanged
4. Upstream crash → Spawn returns error within 2s (not 60s wait)
5. All existing tests pass + new tests for init-ready gate

## Out of Scope

- CC-side timeout changes (not controllable)
- Changing reaper grace period (symptom, not root cause)
- Session reconnection logic (unrelated to init-ready)
