# Playbook: Integrating muxcore into engram

## Prerequisites

- Go 1.25+
- `go get github.com/thebtf/mcp-mux/muxcore/engine`

## Step 1: Add engine dependency

```go
import "github.com/thebtf/mcp-mux/muxcore/engine"
```

## Step 2: Create engine config

engram runs an in-process HTTP server. Run it in a goroutine inside the daemon and
expose the MCP stdio interface through the engine `Handler`.

```go
eng, err := engine.New(engine.Config{
    Name:        "engram",
    Handler:     serveMCPStdio,  // wraps your existing MCP handler
    Persistent:  true,           // memory state must survive session gaps
    IdleTimeout: 1 * time.Hour,
})
if err != nil {
    log.Fatalf("engine: %v", err)
}
```

## Step 3: Replace main() entry point

Before:
```go
func main() {
    srv := startHTTPServer()
    runMCPStdio(os.Stdin, os.Stdout, srv)
}
```

After:
```go
func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
    defer cancel()

    // HTTP server still starts — engram's API endpoint stays unchanged.
    srv := startHTTPServer()

    eng, err := engine.New(engine.Config{
        Name:       "engram",
        Persistent: true,
        Handler: func(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
            return runMCPStdio(ctx, stdin, stdout, srv)
        },
    })
    if err != nil {
        log.Fatalf("engine: %v", err)
    }
    if err := eng.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
        log.Fatalf("engram: %v", err)
    }
}
```

The HTTP server starts once in the daemon. All CC sessions share the same in-memory knowledge graph.

## Step 4: Remove old process management

Disable any external restart mechanism (systemd `Restart=on-success`, launcher scripts).
External restarts create a second daemon and break socket ownership — the engine owns the lifecycle.

## Step 5: Test

```bash
engram &   # first CC session — starts daemon
engram &   # second CC session — connects as client
ps aux | grep engram   # expect: 1 daemon + N thin shims
# Store a memory in session 1, recall it in session 2 to confirm sharing.
```

## Before / After comparison

| Aspect | Before | After |
|---|---|---|
| Processes per CC session | 1 engram per session | 1 daemon + thin shim per session |
| Knowledge graph | isolated per session | shared across all sessions |
| Memory | N × ~200 MB | 1 × ~200 MB |
| HTTP API port conflicts | possible with N instances | impossible (single daemon) |
| Reconnect on crash | session loses context | automatic reconnect, state preserved |

## Configuration options

| Field | Default | Notes |
|---|---|---|
| `Name` | — | Required. Used for socket file naming and log prefix. |
| `Handler` | — | Required. Wraps your existing `runMCPStdio` call. |
| `Persistent` | `false` | Set `true` — engram state must survive zero-session periods. |
| `IdleTimeout` | 5 min | Ignored when `Persistent: true`. |
| `DaemonFlag` | `--muxcore-daemon` | CLI flag appended on daemon re-exec. |
| `BaseDir` | `os.TempDir()` | Override if engram needs a specific temp directory. |
| `ProgressInterval` | 5 s | Synthetic progress ping interval (1-60 s range). |
