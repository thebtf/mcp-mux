# Playbook: Integrating muxcore into aimux

## Prerequisites

- Go 1.25+
- `go get github.com/thebtf/mcp-mux/internal/muxcore/engine`

## Step 1: Add engine dependency

```go
import "github.com/thebtf/mcp-mux/internal/muxcore/engine"
```

## Step 2: Create engine config

aimux maintains session state, so use `Persistent: true` — the daemon stays alive
even when all CC sessions disconnect.

```go
eng, err := engine.New(engine.Config{
    Name:        "aimux",
    Handler:     serveMCP,   // your existing MCP handler func
    Persistent:  true,       // don't exit on zero sessions
    IdleTimeout: 30 * time.Minute,
})
if err != nil {
    log.Fatalf("engine: %v", err)
}
```

`Handler` signature: `func(ctx context.Context, stdin io.Reader, stdout io.Writer) error`

## Step 3: Replace main() entry point

Before:
```go
func main() {
    // ... server setup ...
    runMCPServer(os.Stdin, os.Stdout)
}
```

After:
```go
func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
    defer cancel()

    eng, err := engine.New(engine.Config{
        Name:       "aimux",
        Handler:    serveMCP,
        Persistent: true,
    })
    if err != nil {
        log.Fatalf("engine: %v", err)
    }
    if err := eng.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
        log.Fatalf("aimux: %v", err)
    }
}
```

`eng.Run` auto-detects mode:
- First invocation → starts background daemon, becomes owner
- Subsequent invocations → connect as clients via IPC

## Step 4: Remove old process management

Delete calls to `SharedPM.Kill()` and any manual restart logic.
The engine handles lifecycle: daemon survives CC session disconnect; clients reconnect automatically.

## Step 5: Test

```bash
aimux   # first CC session — starts daemon
aimux   # second CC session — connects as client
ps aux | grep aimux   # expect: 1 daemon + N thin shims
```

## Before / After comparison

| Aspect | Before | After |
|---|---|---|
| Processes per CC session | 1 aimux per session | 1 daemon + thin shim per session |
| Memory | N × ~150 MB | 1 × ~150 MB |
| Reconnect on crash | manual restart | automatic (engine.ResilientClient) |
| Zero-session behaviour | process exits | daemon stays alive (Persistent) |

## Configuration options

| Field | Default | Notes |
|---|---|---|
| `Name` | — | Required. Used for socket file naming and log prefix. |
| `Handler` | — | Required. Your MCP handler func. |
| `Persistent` | `false` | Set `true` for aimux — preserves state across session gaps. |
| `IdleTimeout` | 5 min | Ignored when `Persistent: true`. |
| `DaemonFlag` | `--muxcore-daemon` | CLI flag that triggers daemon mode on re-exec. |
| `BaseDir` | `os.TempDir()` | Socket file directory. Override for custom temp path. |
