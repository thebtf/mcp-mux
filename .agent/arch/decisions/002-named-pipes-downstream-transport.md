# ADR-002: Named Pipes as Downstream Transport

## Status

Accepted

## Date

2026-03-14

## Context

mcp-mux needs to accept connections from multiple CC sessions. Standard stdio is 1:1 (one stdin/stdout per process). We need a mechanism for multiple clients to connect to a single mux process.

Options considered:

1. **Named pipes** (Windows: `\\.\pipe\mcp-mux`, Unix: `/tmp/mcp-mux.sock`) — each client opens a new pipe connection
2. **TCP localhost** — listen on a local port, clients connect via TCP
3. **Wrapper script per session** — spawn mux as a child of each CC session, mux internally shares upstreams

## Decision

**Named pipes** (option 1) as primary, with wrapper script (option 3) as Phase 1 MVP.

### Phase 1 (MVP): Wrapper Script

CC config points to a wrapper script instead of the real MCP server. The wrapper:
1. Connects to a running mux daemon (or starts one)
2. Bridges CC's stdio to the mux's named pipe
3. CC sees standard stdio, mux sees a new client

```json
{
  "mcpServers": {
    "mux": {
      "command": "mcp-mux-client",
      "args": ["--pipe", "\\\\.\\pipe\\mcp-mux"]
    }
  }
}
```

### Phase 2: Native Named Pipe

If CC gains support for custom transports or pipe connections directly, the wrapper becomes unnecessary.

## Rationale

- Named pipes are the standard IPC mechanism on both Windows and Unix
- TCP adds unnecessary network stack overhead for local-only communication
- Named pipes support multiple simultaneous connections (Windows `CreateNamedPipe` with `PIPE_UNLIMITED_INSTANCES`)
- The wrapper script approach works with unmodified CC — it just sees a stdio command

## Consequences

- Windows and Unix have different pipe APIs — need platform abstraction
- Named pipe security: restrict to current user (Windows: DACL, Unix: file permissions)
- Need a daemon management strategy (auto-start on first client, auto-shutdown on last disconnect)
