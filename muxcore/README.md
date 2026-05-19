# muxcore

Go library implementing the mcp-mux daemon engine, IPC transport, and handoff protocol
for zero-downtime daemon upgrades.

## Installation

```bash
go get github.com/thebtf/mcp-mux/muxcore@latest
```

## Stable Operator Status Contract

`daemon.HandleStatus()` is the stable operator status API used by control
clients such as `mcp-mux serve` and `mux_list`. Fields may be added
compatibly, but released field names and meanings must not be removed, renamed,
or repurposed without release notes and a compatibility window.

Current lifecycle evidence fields:

| Field | Meaning |
| --- | --- |
| `daemon_generation` | Process-lifetime generation string for distinguishing predecessor and successor daemons. |
| `reaped_owner_count` | Count of owners removed by idle lifecycle reaping. |
| `owner_removal.total` | Count of owner-removal helper executions in this daemon process. |
| `owner_removal.by_reason` | Removal counts keyed by reason, including `operator_hard`, `operator_soft`, `idle`, `zombie`, `handoff`, `restore_failed`, and `upstream_exit`. |
| `owner_removal.pending_tokens_removed` | Total owner-scoped pending session tokens removed during owner cleanup. |
| `owner_removal.bound_history_removed` | Total owner-scoped reconnect history entries removed during owner cleanup. |
| `handoff.successor_daemon_generation` | Current daemon generation reported in the handoff status block. |
| `handoff.restored_owner_count` | Number of owners restored from snapshot or handoff into this daemon process. |
| `servers[].owner_generation` | Per-owner generation string; changes when a fresh owner replaces a prior owner. |
| `servers[].restored_from_owner_generation` | Prior owner generation when an owner was restored from snapshot or handoff. |
| `servers[].restore_source` | Owner source classification: `fresh`, `snapshot_handoff`, or `snapshot_fallback`. |

## Handoff API (external daemon upgrade)

The `daemon` package exposes five public functions for coordinating a live handoff between
an old daemon process and its successor during a zero-downtime upgrade. Upstream processes
(MCP servers) survive the upgrade without restarting — their file descriptors are transferred
directly to the new daemon.

### Quick start

```go
import (
    "context"
    "net"

    "github.com/thebtf/mcp-mux/muxcore/daemon"
)

// Predecessor (old daemon) — called after accepting a connection from the successor.
// `conn` must be a *net.UnixConn on Unix or a *winio named-pipe conn on Windows.
func predecessor(conn net.Conn, token string, upstreams []daemon.HandoffUpstream) {
    result, err := daemon.PerformHandoff(context.Background(), conn, token, upstreams)
    if err != nil {
        // ErrTokenMismatch, ErrProtocolVersionMismatch, or transport error
        return
    }
    // result.Transferred — server IDs successfully handed off
    // result.Aborted    — server IDs that fell back to clean shutdown
    _ = result
}

// Successor (new daemon) — called after dialing the predecessor's handoff socket.
// `conn` must be a *net.UnixConn on Unix or a *winio named-pipe conn on Windows.
func successor(conn net.Conn, token string) {
    upstreams, err := daemon.ReceiveHandoff(context.Background(), conn, token)
    if err != nil {
        return
    }
    // upstreams contains transferred HandoffUpstream entries with StdinFD/StdoutFD
    // ready to attach to new Owner instances.
    _ = upstreams
}
```

### Token lifecycle

```go
// 1. Predecessor generates and writes the token.
token, path, err := daemon.WriteHandoffToken(dir)
if err != nil { /* handle */ }

// 2. Pass token + socket path to the successor via env var or config.
// os.Setenv("HANDOFF_TOKEN", token)
// os.Setenv("HANDOFF_SOCKET", socketPath)

// 3. Successor reads the token before dialing.
token, err = daemon.ReadHandoffToken(path)
if err != nil { /* handle */ }

// 4. Clean up — idempotent, safe from defer.
defer daemon.DeleteHandoffToken(path)
```

### Error handling

| Error / Field | Meaning |
|---------------|---------|
| `daemon.ErrTokenMismatch` | Successor presented wrong pre-shared token (constant-time comparison) |
| `daemon.ErrProtocolVersionMismatch` | Incompatible protocol version; fall back to shutdown |
| `result.Aborted` | Per-upstream list of server IDs NOT transferred (individual failure; other upstreams may succeed) |

### Platform constraints

| Platform | Requirement |
|----------|-------------|
| Unix (Linux, macOS) | `conn` must be `*net.UnixConn`; any other type returns an error |
| Windows | `conn` must be a winio named-pipe connection (`winio.DialPipeContext` / `ListenPipe.Accept`) |

On Windows the successor PID is supplied automatically via the handoff protocol's
`HelloMsg.SourcePID` field — no additional configuration is required from the caller.
