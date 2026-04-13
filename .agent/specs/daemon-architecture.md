# Specification: Global Daemon Architecture

## Overview

Migrate mcp-mux from per-server owner model to a single global daemon that manages
all upstream MCP server processes. CC sessions connect as thin shims via IPC.
Daemon handles lifecycle, GC, reaping, health monitoring, and persistence.

## Functional Requirements

- FR1: First `mcp-mux <command>` invocation auto-starts the daemon if not running
- FR2: All subsequent `mcp-mux` invocations connect to the daemon as shims (stdio↔IPC bridge)
- FR3: Daemon spawns and manages upstream MCP server processes (one per server identity)
- FR4: Daemon survives CC session disconnects — upstream processes continue running
- FR5: New CC sessions reconnect to existing upstream processes via daemon
- FR6: Daemon reaps dead upstream processes and cleans up their resources
- FR7: Daemon periodically sweeps stale IPC sockets and zombie processes
- FR8: Servers declaring `x-mux.persistent: true` keep upstream alive even with zero connected sessions
- FR9: Non-persistent upstreams are shut down after last session disconnects (configurable grace period)
- FR10: `mcp-mux status` queries daemon for all upstream statuses
- FR11: `mcp-mux stop` sends shutdown to daemon (drains all upstreams)
- FR12: Daemon auto-exits when no upstreams remain and no sessions connected (idle timeout)

## Non-Functional Requirements

- NFR1: Shim startup latency <50ms (connect to daemon, not spawn upstream)
- NFR2: Daemon memory overhead <10MB (routing + bookkeeping only)
- NFR3: Zero data loss on CC restart for persistent servers
- NFR4: Backward compatible — existing .mcp.json configs work unchanged

## Acceptance Criteria

- [ ] AC1: CC session restart → same upstream PID, no re-initialization
- [ ] AC2: `mcp-mux status` shows all upstreams from single daemon
- [ ] AC3: Kill CC process → upstream survives, next CC reconnects
- [ ] AC4: Upstream crash → daemon detects, cleans up, next request re-spawns
- [ ] AC5: `mcp-mux stop` → all upstreams drained and stopped
- [ ] AC6: Daemon idle for N minutes with no sessions → auto-exit
- [ ] AC7: Persistent server (aimux) keeps running through CC restarts

## Out of Scope

- HTTP/SSE transport (STDIO only)
- Multi-machine daemon (single host only)
- Auth between shim and daemon (trusted local IPC)
- GUI/dashboard (CLI status only)

## Dependencies

- Existing IPC transport (Unix domain sockets)
- Existing control protocol (control.Server)
- Existing upstream.Process management
- Existing session/owner routing logic
