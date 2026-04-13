# ADR-005: Wrapper Pattern Over Separate Daemon

## Status

Accepted

## Date

2026-03-15

## Context

The original design (ADR-002 v1) had a separate daemon process + client wrapper:
- User starts a daemon manually or via init script
- Each CC session runs `mcp-mux-client` which bridges stdio→daemon pipe
- Daemon manages all upstreams centrally

This required: daemon lifecycle management, a separate config file, and user awareness of the daemon.

## Decision

**Self-coordinating wrapper pattern**: mcp-mux IS the command, not a client of a daemon.

```json
{
  "command": "mcp-mux",
  "args": ["uvx", "--refresh", "--from", "git+...", "serena", ...]
}
```

mcp-mux takes `argv[1:]` as the original command. First instance for a given server identity becomes the owner (spawns upstream, listens on IPC). Subsequent instances connect as clients.

## Rationale

- **Zero user friction**: change one line in MCP config (command → mcp-mux, original command moves to args[0])
- **No daemon to manage**: no init scripts, no "is it running?" questions, no orphan daemon cleanup
- **No config file**: server identity derived from command+args hash — everything mcp-mux needs is in the invocation
- **Opt-in per server**: user wraps only the servers they want to share — full control
- **Works with any runtime**: uvx, npx, node, python, go run — mcp-mux doesn't care what's behind it
- **Familiar pattern**: same as `nice`, `strace`, `time` — command prefix that adds behavior

## Consequences

- Need robust owner election: race on first start, OS-level atomic pipe creation
- Need owner crash recovery: clients detect broken IPC, one promotes to owner
- No central config means no central view — `mcp-mux status` must discover active instances by scanning IPC endpoints
- Each server identity is independently managed — no "start all" or "stop all" (unless we add it to CLI)
