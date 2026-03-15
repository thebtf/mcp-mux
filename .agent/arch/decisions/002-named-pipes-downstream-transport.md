# ADR-002: Named Pipes as IPC Transport (Revised)

## Status

Superseded — revised 2026-03-15

## Date

2026-03-14 (original), 2026-03-15 (revised)

## Context

mcp-mux needs IPC between mux instances. The original design had a separate daemon + client wrapper with named pipes for downstream CC→mux communication.

The 2026-03-15 brainstorm revised the architecture to a **self-coordinating wrapper pattern**: mcp-mux is the command prefix, first instance becomes the "owner" (spawns upstream, listens on IPC), subsequent instances connect as clients.

Named pipes (Windows) / Unix domain sockets remain the IPC transport, but their role has changed: they now connect mux instances to each other, not CC sessions to a daemon.

## Decision

**Named pipes/Unix sockets for inter-instance IPC**, with self-coordination (no separate daemon).

### How it works

1. CC launches `mcp-mux uvx ... serena` — mcp-mux checks if IPC endpoint exists for this server identity
2. No endpoint → become owner: spawn upstream, listen on `\\.\pipe\mcp-mux-{server_id}` (Windows) or `/tmp/mcp-mux-{server_id}.sock` (Unix)
3. Endpoint exists → become client: connect to owner via IPC, bridge CC stdio ↔ IPC

### Server Identity

```
server_id = hash(command + args + selected_env_keys)
ipc_path  = \\.\pipe\mcp-mux-{server_id}    # Windows
          = /tmp/mcp-mux-{server_id}.sock    # Unix
```

## Rationale

- No separate daemon to manage — the system is self-organizing
- Named pipes remain the best IPC mechanism (low latency, multi-connection, OS-native)
- Server identity derived from command+args means zero configuration
- Owner election is deterministic: first to create the pipe wins

## Consequences

- Need robust owner crash detection: clients must detect broken IPC, one becomes new owner
- Race condition on startup: two simultaneous first instances — use create-or-connect with OS-level atomicity
- Platform abstraction still needed (Windows named pipes vs Unix domain sockets)
