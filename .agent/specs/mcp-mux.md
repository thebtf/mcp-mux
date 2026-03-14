# Specification: mcp-mux — MCP Stdio Multiplexer

## Overview

A local proxy that allows multiple MCP clients (Claude Code sessions) to share single instances of MCP servers. Eliminates the N×M process explosion when running N parallel sessions with M MCP servers.

## Functional Requirements

- FR1: Accept multiple downstream stdio connections from MCP clients via named pipes (Windows) or Unix domain sockets (Linux/macOS)
- FR2: Maintain a single upstream stdio connection per configured MCP server
- FR3: Multiplex downstream requests to shared upstream servers with JSON-RPC id remapping to prevent collisions
- FR4: Route responses back to the correct downstream client based on remapped id
- FR5: Handle MCP `initialize` handshake — first client triggers real init, subsequent clients receive cached `InitializeResult`
- FR6: Support server categorization: `shared` (stateless, safe to multiplex) vs `isolated` (per-session state, spawn dedicated process)
- FR7: Broadcast server-to-client notifications to all connected downstream clients (for shared servers)
- FR8: Handle client disconnect gracefully — clean up id mappings, do not terminate shared upstream
- FR9: Terminate upstream server when last client disconnects (configurable: keep-alive or shutdown)
- FR10: Provide health/status endpoint showing connected clients, active upstreams, and memory usage
- FR11: Configuration via TOML/JSON file specifying upstream servers, their commands/args/env, and share mode

## Non-Functional Requirements

- NFR1: Latency overhead < 1ms per proxied request (JSON parse + id remap + route)
- NFR2: Memory footprint of mux process itself < 50 MB
- NFR3: Support at least 8 simultaneous downstream clients
- NFR4: Cross-platform: Windows (primary), Linux, macOS
- NFR5: Zero dependencies beyond Node.js stdlib (no npm packages for core proxy logic)
- NFR6: Startup time < 500ms (excluding upstream server spawn time)

## Acceptance Criteria

- [ ] AC1: 4 CC sessions connect to mcp-mux, each sees full tool list from all configured servers
- [ ] AC2: Request from Session 1 to engram returns only to Session 1, not to others
- [ ] AC3: Two sessions sending concurrent requests with same JSON-RPC id=1 both get correct responses
- [ ] AC4: Upstream server spawned only once — second client reuses existing connection
- [ ] AC5: Client disconnect does not crash mux or affect other clients
- [ ] AC6: `isolated` mode spawns separate upstream per client (for stateful servers like SSH)
- [ ] AC7: Memory usage with 4 clients × 6 shared upstreams < 200 MB total (vs ~1.5 GB current)
- [ ] AC8: Works on Windows with named pipes as downstream transport

## Out of Scope

- HTTP/SSE MCP transport (focus on stdio only)
- Authentication/authorization between clients and mux (local-only, trusted)
- Remote/network MCP proxy (mux runs on same machine as CC)
- Auto-discovery of MCP servers (config is explicit)
- Load balancing across multiple upstream instances

## Dependencies

- Node.js >= 20 (for native pipe/socket support)
- MCP protocol spec 2025-06-18 (JSON-RPC 2.0 over stdio)
- No external npm packages for core (optional: testing framework)

## Architecture Overview

```
┌─────────────────────────────────────────────────────┐
│                     mcp-mux                          │
│                                                      │
│  ┌──────────┐    ┌───────────────┐    ┌───────────┐ │
│  │Downstream│    │  Router &     │    │ Upstream   │ │
│  │ Listener │───>│  ID Remapper  │───>│ Manager    │ │
│  │(pipes)   │<───│               │<───│(processes) │ │
│  └──────────┘    └───────────────┘    └───────────┘ │
│       ↑                                     │       │
│  CC sessions                          MCP servers   │
│  (1..N)                               (1..M)        │
└─────────────────────────────────────────────────────┘
```

### Core Components

1. **Downstream Listener**: Accepts stdio connections from CC sessions via named pipes
2. **Router & ID Remapper**: Parses JSON-RPC, rewrites `id` field with session prefix, routes to correct upstream
3. **Upstream Manager**: Spawns and manages MCP server processes, maintains connection pool
4. **Config Loader**: Reads server definitions from config file
5. **Init Cache**: Stores `InitializeResult` per upstream for replaying to new clients

### ID Remapping Strategy

```
Downstream (Session 1): { "id": 1, "method": "tools/call", ... }
    ↓ remap
Upstream:               { "id": "s1:1", "method": "tools/call", ... }
    ↓ response
Upstream:               { "id": "s1:1", "result": ... }
    ↓ de-remap
Downstream (Session 1): { "id": 1, "result": ... }
```

### Tool Routing

CC sends `tools/call` with tool name like `search`. mcp-mux must know which upstream handles which tool. Strategy:
- On upstream init, capture `tools/list` response
- Build tool→upstream routing table
- Route `tools/call` by tool name lookup

### Configuration Format

```toml
[mux]
listen = "\\\\.\\pipe\\mcp-mux"  # Windows named pipe
# listen = "/tmp/mcp-mux.sock"   # Unix socket

[servers.engram]
command = "node"
args = ["path/to/engram/index.js"]
mode = "shared"  # shared | isolated

[servers.tavily]
command = "cmd"
args = ["/c", "npx", "tavily-mcp@latest"]
env = { TAVILY_API_KEY = "..." }
mode = "shared"

[servers.nvmd-ssh]
command = "node"
args = ["path/to/nvmd-ssh/index.js"]
mode = "isolated"  # per-session state (SSH connections)
```

### Lifecycle

1. mcp-mux starts, reads config, does NOT spawn upstreams yet (lazy)
2. First CC session connects via pipe → mux spawns all `shared` upstreams, sends `initialize` to each, caches results
3. CC sends `initialize` → mux returns cached result (merged capabilities from all upstreams)
4. CC sends `tools/list` → mux returns merged tool list from all upstreams
5. CC sends `tools/call` → mux routes to correct upstream by tool name, remaps id
6. Additional CC sessions connect → reuse existing upstreams, get cached init
7. CC disconnects → cleanup id mappings, if last client and keep-alive=false → shutdown upstreams

### Edge Cases

- **Tool name collision**: Two upstreams expose same tool name → prefix with server name, or reject config
- **Upstream crash**: Detect via process exit, notify all downstream clients with error, attempt restart
- **Slow upstream**: Timeout per request (configurable), return JSON-RPC error to downstream
- **Backpressure**: If upstream is slow, queue per-session (bounded), reject if queue full
