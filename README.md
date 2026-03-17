# mcp-mux — Transparent stdio multiplexer for MCP servers

Wraps any MCP server command so multiple Claude Code sessions share a single upstream
process. One line change in `.mcp.json` — no other configuration required.

## Problem

Each Claude Code session spawns its own copy of every configured MCP server (stdio
transport). With 4 parallel sessions and 12 servers, that is 48 node/Python processes
consuming roughly 4.8 GB of RAM. Most MCP servers are stateless — they don't need
per-session isolation.

## Solution

Prefix your MCP server command with `mcp-mux`. The first instance to start becomes the
**owner**: it spawns the real upstream process and listens on a Unix domain socket. All
subsequent instances connect as **clients** through the owner.

```
CC Session 1 ──stdio──> mcp-mux (shim) ──IPC──┐
CC Session 2 ──stdio──> mcp-mux (shim) ──IPC──┤──> mcp-mux (owner) ──stdio──> upstream (1x)
CC Session 3 ──stdio──> mcp-mux (shim) ──IPC──┘
```

A single **daemon** process manages all owners. Upstreams survive CC session restarts and
are only stopped after a configurable grace period of idle time.

Result: one upstream process per server instead of N, ~3x memory reduction.

## Architecture

```
                    ┌─────────────────────────────────────────┐
                    │              mcp-mux daemon              │
                    │                                          │
                    │  owner: engram ──stdio──> engram proc    │
                    │  owner: tavily ──stdio──> tavily proc    │
                    │  owner: serena ──stdio──> serena proc    │
                    └────────────────┬────────────────────────┘
                                     │  IPC (Unix sockets)
              ┌──────────────────────┼──────────────────────┐
              │                      │                      │
    CC Sess 1 │              CC Sess 2│            CC Sess 3 │
  mcp-mux shim│           mcp-mux shim│         mcp-mux shim│
  (stdio ──> IPC)        (stdio ──> IPC)       (stdio ──> IPC)
```

Each shim connects to the daemon owner for its upstream. If no daemon is running, the
shim auto-starts one. If no owner exists for a given server, the daemon spawns it.

## Quick Start

**1. Build**

```sh
go build -o mcp-mux.exe ./cmd/mcp-mux
```

Place the binary somewhere on your PATH, or reference it by absolute path in `.mcp.json`.

**2. Configure**

Take any MCP server entry in `.mcp.json` and move the `command` into `args`, replacing
`command` with `mcp-mux`:

Before:
```json
{
  "mcpServers": {
    "engram": {
      "command": "uvx",
      "args": ["engram-mcp-server", "--db", "/data/engram.db"]
    }
  }
}
```

After:
```json
{
  "mcpServers": {
    "engram": {
      "command": "mcp-mux",
      "args": ["uvx", "engram-mcp-server", "--db", "/data/engram.db"]
    }
  }
}
```

That's it. On the next CC session start, mcp-mux intercepts the stdio channel, connects
to (or starts) the daemon, and proxies all MCP traffic transparently.

## Sharing Modes

| Mode | Behavior | Use When |
|------|----------|----------|
| `shared` (default) | One upstream serves all sessions. | Stateless servers: search, docs, LLM proxy. |
| `isolated` | Each session gets its own upstream. | Per-session state: browser, SSH, editor buffers. |
| `session-aware` | One upstream; sessions identified by injected `_meta.muxSessionId`. | Stateful servers that can partition by session key. |

Override for a specific server:

```sh
# Force isolation for one invocation
MCP_MUX_ISOLATED=1 mcp-mux uvx my-server

# CLI flag (equivalent)
mcp-mux --isolated uvx my-server
```

## Auto-Classification

When no explicit mode is set, mcp-mux classifies each server automatically using this
priority order:

1. **`x-mux` capability** (highest) — server declares `x-mux.sharing` in its
   `initialize` response. Authoritative; overrides all heuristics.
2. **Tool-name heuristics** — tools with prefixes or substrings matching browser, session,
   editor, navigate, page, tab, process, document, or snapshot patterns trigger isolation.
3. **Default** — `shared`.

If your server is stateless but has tool names that match isolation patterns, add
`x-mux.sharing: "shared"` to your `initialize` capabilities to fix the classification.

## Daemon Mode

The daemon is enabled by default. It starts automatically when the first mcp-mux shim
connects and has no owner to hand off to.

**Lifecycle:**

- Shim connects → daemon starts or is reused
- CC session exits → grace period begins (default 30s)
- If no new session reconnects within grace period → owner stops the upstream
- Servers declaring `x-mux.persistent: true` skip the grace period; they stay alive
  indefinitely until explicitly stopped
- Daemon auto-exits after 5 minutes with no owners and no connected sessions

**Disable daemon mode** (legacy per-session owner behavior):

```sh
MCP_MUX_NO_DAEMON=1 mcp-mux uvx my-server
```

## Commands

```sh
# Show all running upstream instances (PID, sessions, classification, cache state)
mcp-mux status

# Stop all running instances and the daemon
mcp-mux stop

# Atomic binary upgrade (see section below)
mcp-mux upgrade

# Start a detached daemon owner manually (for scripting)
mcp-mux daemon uvx my-server --arg value

# Run as control-plane MCP server (exposes mux_list / mux_stop / mux_restart tools)
mcp-mux serve
```

## Atomic Upgrade

Upgrading the mcp-mux binary while sessions are active is safe:

```sh
# Build new binary to a staging path
go build -o mcp-mux.exe~ ./cmd/mcp-mux

# Atomic rename-swap; daemon and running sessions are unaffected
mcp-mux upgrade
```

`upgrade` performs an atomic file rename. The running daemon process continues using the
old binary in memory. New shim invocations after the swap use the new binary. No sessions
are interrupted.

Rollback: if the staged binary is missing or the rename fails, `upgrade` exits with an
error and the existing binary is untouched.

## Configuration

All configuration is via environment variables. No config file is required for normal
operation.

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_MUX_NO_DAEMON` | `0` | Set to `1` to disable daemon mode (legacy per-session owner) |
| `MCP_MUX_ISOLATED` | `0` | Set to `1` to force isolated mode for this server |
| `MCP_MUX_STATELESS` | `0` | Set to `1` to ignore cwd in server identity hash (enables global dedup) |
| `MCP_MUX_GRACE` | `30s` | Grace period before idle owner stops its upstream |
| `MCP_MUX_IDLE_TIMEOUT` | `5m` | Daemon auto-exit after this period with no activity |
| `MCP_MUX_DAEMON` | `0` | Set to `1` to start this invocation in daemon (headless owner) mode |

## Control Plane MCP Server

`mcp-mux serve` exposes an MCP server on stdio with management tools. Add it to
`.mcp.json` like any other server:

```json
{
  "mcpServers": {
    "mcp-mux": {
      "command": "mcp-mux",
      "args": ["serve"]
    }
  }
}
```

Available tools:

- **`mux_list`** — returns all running instances with server ID, PID, session count,
  pending requests, classification, and cache status.
- **`mux_stop`** — gracefully drains and stops an instance by `server_id`. Use
  `force: true` for immediate kill.
- **`mux_restart`** — stops an instance and spawns a fresh daemon owner with the same
  command. Connected sessions reconnect automatically on their next tool call.

Available prompts:

- **`mux-guide`** — full reference on architecture, classification, caching, and
  troubleshooting.
- **`mux-status-summary`** — calls `mux_list` and returns a human-readable summary.

## For MCP Server Authors

Declare your server's sharing preference in the `initialize` response:

```json
{
  "protocolVersion": "2025-11-25",
  "capabilities": {
    "tools": {},
    "x-mux": {
      "sharing": "shared"
    }
  }
}
```

For stateless servers that don't depend on the client's working directory, add
`"stateless": true` to enable global deduplication (one upstream instance regardless of
which directory CC is opened from):

```json
{ "x-mux": { "sharing": "shared", "stateless": true } }
```

For session-aware servers, mcp-mux injects `_meta.muxSessionId` (format: `sess_` +
8 hex chars) into every request. Use it to partition in-process state by session:

```json
{ "x-mux": { "sharing": "session-aware" } }
```

Full protocol specification including implementation examples (TypeScript, Python, Go)
and migration path: [`docs/mux-protocol.md`](docs/mux-protocol.md).

## License

MIT
