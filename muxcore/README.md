# muxcore

Go library implementing the mcp-mux daemon engine, IPC transport, and handoff protocol
for zero-downtime daemon upgrades.

## Installation

```bash
go get github.com/thebtf/mcp-mux/muxcore@latest
```

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
