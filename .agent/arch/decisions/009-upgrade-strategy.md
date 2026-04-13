# ADR-009: Upgrade Strategy — Rename-Swap Without Daemon Restart

## Status
Accepted

## Context
mcp-mux daemon manages N upstream processes via stdio pipes (kernel pipe objects).
When daemon dies, OS closes pipe fd → upstreams get broken pipe → connections lost.

User needs: update mcp-mux binary without losing any connections.

## Decision
**Rename-swap only.** `mcp-mux upgrade` renames running binary and places new one.
Daemon continues running old code from memory. All connections preserved.
New shim processes use new binary. Daemon gets new code on next natural restart.

## Alternatives Considered

### fd passing (nginx-style exec)
- `cloudflare/tableflip` passes listener fd to new process
- Works for listener sockets (net.Listener)
- **Does NOT work for stdio pipes** — `*exec.Cmd` pipes are kernel objects tied to process
- **Windows: no equivalent** — AdditionalInheritedHandles has limitations
- **Rejected:** cannot pass upstream stdio pipes

### Unix socket passing (SCM_RIGHTS)
- Passes arbitrary fd via Unix domain socket
- Same limitation: works for sockets, not for stdio pipes
- Windows: no SCM_RIGHTS equivalent
- **Rejected:** same pipe limitation

### Owner as separate process
- Each Owner runs as independent process, not goroutine in daemon
- Daemon restart doesn't kill Owners
- **Rejected:** major refactor, complexity explosion, IPC overhead

### Intermediate proxy between daemon and upstreams
- Instead of direct stdio pipes, use Unix sockets
- Daemon reconnects to sockets after restart
- **Rejected:** MCP servers expect stdio, not sockets

## Consequences
- Upgrade is instant and non-disruptive (rename only)
- Daemon code updates lag behind binary updates
- For critical daemon logic changes: explicit `mcp-mux stop` needed
- This is an OS-level limitation, not an architecture flaw

## Rationale
Stdio pipes are kernel objects. When a process dies, its pipe endpoints close.
There is no cross-platform way to transfer pipe ownership between processes.
This is the same reason nginx uses fork (shared memory) instead of exec for graceful restart.
We cannot fork on Windows, so rename-swap is the only viable zero-downtime approach.
