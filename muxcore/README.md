# muxcore

`muxcore` is the Go library behind `mcp-mux`. It provides the daemon, shim,
IPC, token handshake, reconnect, session routing, snapshot, and handoff layers
needed to run one MCP server implementation behind many stdio clients.

This README is for developers embedding muxcore into another binary such as
`aimux` or `engram`. If you only want to wrap an existing MCP server from a
Claude/Codex config, use the top-level `mcp-mux` CLI instead.

## Install

Pin the tagged muxcore module. Do not depend on `latest` for production
consumers; muxcore is a runtime layer and downstream behavior changes matter.

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.25.0
```

## Golden Path

Use `muxcore/engine`. Treat lower-level packages (`daemon`, `owner`, `session`,
`control`, `ipc`) as expert APIs for muxcore maintainers unless you have a
specific reason to bypass the engine.

Minimal shape:

```go
package main

import (
    "context"
    "io"
    "log"
    "os"
    "os/signal"

    "github.com/thebtf/mcp-mux/muxcore/engine"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
    defer stop()

    eng, err := engine.New(engine.Config{
        Name:    "my-mcp-server",
        Handler: serveStdio,
    })
    if err != nil {
        log.Fatal(err)
    }
    if err := eng.Run(ctx); err != nil {
        log.Fatal(err)
    }
}

func serveStdio(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
    // Run your normal MCP stdio server loop here.
    return myServer.Serve(ctx, stdin, stdout)
}
```

`engine.Run` selects one of three roles from the same binary:

| Runtime role | Trigger | What muxcore does |
| --- | --- | --- |
| Client/shim | normal launch from the MCP host | Starts or reuses the daemon, asks it for an owner, connects with a one-time token, and runs the resilient stdio proxy. |
| Daemon | `engine.Config.DaemonFlag` is present in `os.Args` | Owns all upstream/session owners for this engine namespace, serves the control socket, reaps idle owners, snapshots state, and handles graceful restart. |
| Proxy | `MCP_MUX_SESSION_ID` is set | Runs the raw `Handler` directly on stdio because an outer mux is already managing multiplexing. |

## Minimum Correct Integration Contract

Every muxcore consumer should satisfy this checklist before shipping.

1. **Use `engine.New` and `engine.Run` as the main runtime path.**
   `engine.New` enforces the two most important setup errors: `Name` is
   required, and at least one serving shape (`Command`, `Handler`, or
   `SessionHandler`) is required. Direct `daemon.New` still has compatibility
   defaults and can be misconfigured more easily.

2. **Choose a unique, stable `engine.Config.Name`.**
   The name scopes daemon control sockets, owner sockets, locks, stale-socket
   cleanup, status, and reconnect behavior. Do not reuse `"mcp-mux"` for a
   different product. Use values like `"aimux"` or `"engram"`.

3. **Choose one serving shape deliberately.**

   | Shape | Use when | Required config |
   | --- | --- | --- |
   | Subprocess upstream | Your wrapper should spawn another executable as the real MCP server. | `Command` + `Args` |
   | Raw stdio in-process server | Your binary already has a stdio MCP server loop. | `Handler` |
   | Structured in-process server | You want per-request `ProjectContext`, session routing, direct notification handling, and optional `SessionMeta`. | `SessionHandler` |

   If you set both `Handler` and `SessionHandler`, daemon mode uses
   `SessionHandler`; proxy mode still needs `Handler`. This is intentional:
   a process wrapped by an outer `mcp-mux` has only raw stdio and cannot serve a
   structured `SessionHandler` without an owner.

4. **Keep proxy-mode compatibility when using `SessionHandler`.**
   If your program may be launched behind an external `mcp-mux` shim, keep a
   `Handler` fallback too. With `SessionHandler` alone, proxy mode returns an
   error because there is no owner/session dispatcher in that process.

5. **Make your CLI parser accept daemon mode.**
   The default daemon flag is `--muxcore-daemon`. Strict CLI frameworks must not
   reject that flag before `engine.Run` sees it. If your product uses a
   subcommand such as `daemon`, set `DaemonFlag: "daemon"` and make sure the
   same argument starts the engine in daemon mode. Graceful restart re-execs the
   binary with this exact flag.

6. **Do not bypass the daemon spawn/token path.**
   A daemon-managed owner requires a one-time session token minted by the
   daemon. Connecting directly to an owner IPC path skips `PreRegister`/`Bind`
   and is correctly rejected as an invalid or missing token. Let the engine call
   `spawn` and run the resilient client.

7. **Set `Persistent` for long-lived state.**
   If your server owns background jobs, caches, indexes, active conversations,
   or other state that should survive all clients disconnecting, set
   `Persistent: true` or declare `x-mux.persistent: true` in the upstream
   initialize response. Otherwise idle owners are eligible for reaping.

8. **Use `StdinEOFWaitForDisconnect` only when EOF is not client shutdown.**
   The default policy treats stdin EOF as the MCP host ending the session: drain
   in-flight work, then exit. Use `owner.StdinEOFWaitForDisconnect` only for
   internal pipes where EOF does not mean the client intentionally closed.

9. **Use `Ready()` and `Daemon()` for in-process status.**
   If you need health/status from inside the same process, wait for
   `eng.Ready()` and call `eng.Daemon()` in daemon mode. Do not round-trip to
   your own control socket unless you are in client/proxy mode or an external
   operator process.

10. **Provide a real update strategy.**
    Muxcore can restart daemons and reattach owners, but your product must
    choose how new executable bytes become the next daemon. On Windows the
    configured executable is exactly the file most likely to be held by live
    shim/daemon processes, so do not assume that a self-overwrite or
    launcher-path swap is a safe update protocol.

## SessionHandler Contract

`SessionHandler` is the preferred API for stateful in-process MCP servers.

```go
import muxcore "github.com/thebtf/mcp-mux/muxcore"

type server struct{}

func (s *server) HandleRequest(
    ctx context.Context,
    project muxcore.ProjectContext,
    request []byte,
) ([]byte, error) {
    // project.ID is stable for the worktree.
    // project.Cwd is the client session cwd.
    // project.Env contains per-session environment values forwarded by muxcore.
    return handleJSONRPC(ctx, project, request)
}
```

Important rules:

- `HandleRequest` is called concurrently. Protect shared state.
- `ProjectContext.ID` is the stable project key; `ProjectContext.Cwd` is the
  raw session cwd and may differ across sessions in the same worktree.
- `ProjectContext.Env` is a merged session environment: shim-provided values
  win, daemon environment fills gaps. This prevents trimmed client environments
  from losing credentials.
- Implement `NotificationHandler` if you need client-to-server notifications
  such as `notifications/cancelled`.
- Implement `SessionHandlerWithSessionMeta` and
  `NotificationHandlerWithSessionMeta` when you need OS peer identity or
  `AuthorizeSession` metadata. If both legacy and `WithSessionMeta` methods are
  implemented, muxcore calls only the `WithSessionMeta` path for that frame.

`ConnInfo` zero values are meaningful: `PeerPid == 0` and `PeerUid == 0` mean
"unavailable", not PID 0 or root.

## Authorization and Frame Hooks

Muxcore provides two optional policy hooks on `engine.Config`.

### `AuthorizeSession`

`AuthorizeSession` runs once per session after IPC token handshake and peer
credential extraction, before the session is added or any frame is dispatched.

```go
AuthorizeSession: func(ctx context.Context, conn muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth {
    tenant, ok := lookupTenant(conn.PeerPid, project.ID)
    if !ok {
        return muxcore.SessionAuth{
            Decision: muxcore.AuthDeny,
            Reason:   "tenant_not_enrolled",
        }
    }
    return muxcore.SessionAuth{
        Decision: muxcore.AuthAllow,
        TenantID: tenant,
    }
}
```

Semantics:

- `AuthAllow` stamps `SessionMeta.TenantID` and `AuthorizedAt`.
- Empty `TenantID` is valid for `AuthAllow`; use `SessionMeta.IsAuthorized()`
  to distinguish "authorized with no tenant" from "callback not configured".
- `AuthDeny` sends JSON-RPC `-32000` with the reason and closes the session.
- Panics are recovered and treated as deny with reason `"authorize panic"`.
- Denial does not necessarily stop an already-created owner/upstream, because
  upstreams are owner resources, not per-session resources. If your policy must
  gate upstream creation itself, gate before registering/starting the owner.

### `OnFrameReceived`

`OnFrameReceived` runs for every inbound client-to-server frame after parsing
and before dispatch.

Rules:

- It runs synchronously on the reader goroutine.
- The muxcore budget is 1 ms. Timeout is fail-open (`FramePass`) and logged.
- Panics are recovered and treated as `FramePass`.
- Scope is inbound only. Outbound responses and muxcore synthetic
  notifications are not intercepted.

Use it for cheap admission decisions such as local rate limiting. Do not put
network calls, database writes, or heavy policy evaluation in this hook.

## Upgrade and Restart Contract

Muxcore has two related but separate responsibilities:

1. **Runtime handoff:** the daemon can snapshot state, start a successor, and
   transfer or restore owners so shims reconnect.
2. **Binary selection:** your product must decide which executable the successor
   daemon should run.

By default, the handoff successor uses `os.Executable()` with
`engine.Config.DaemonFlag`. That is correct only when the current executable is
the intended successor. For versioned or launcher-based deployments, set one of
these environment variables before invoking graceful restart:

| Variable | Meaning |
| --- | --- |
| `MCPMUX_SUCCESSOR_EXE` | Absolute path to the successor executable. Highest priority. |
| `MCPMUX_ACTIVE_ENGINE_FILE` | Path to a text file containing the active engine path. Relative paths are resolved from that file's directory. |

The `mcp-mux` CLI implements the recommended Windows-safe pattern:

```text
consumer config -> stable launcher -> versions/<hash>/engine executable
                               \-> active.txt
```

For muxcore consumers, the important lesson is the invariant, not the exact
directory name: do not make the configured executable the thing you must rename
while it is running. Keep the configured launcher stable and move runtime code
behind a versioned engine pointer.

`muxcore/upgrade.Swap` is still available as a low-level rename-swap helper,
but it is not a full production update strategy for binaries that may be held
open by many live shim processes.

### Choose One Update Topology

Pick exactly one product topology and document it in the consumer's own
operator docs.

| Topology | Use when | Consumer action |
| --- | --- | --- |
| Stable launcher + versioned engine store | The executable configured in the MCP host may be held open by live shims or daemons. This is the safest Windows topology. | Keep the configured launcher stable. Install each build as a new engine path such as `versions/<hash>/<product>-engine.exe`, update an `active.txt` pointer, and restart with `MCPMUX_ACTIVE_ENGINE_FILE` or `MCPMUX_SUCCESSOR_EXE` pointing at the intended successor. Do not call `ApplyUpdateAndRestart` against the launcher path. |
| Fixed replaceable engine path | The product has a current engine executable path that can be renamed while old processes continue from `*.old.<pid>`. | Build or copy the candidate to an explicit `StagedExe`, commonly `CurrentExe + "~"`, then call `ApplyUpdateAndRestart` with `CurrentExe` set to that replaceable engine path. |
| Offline / custom supervisor | The product owns daemon lifecycle outside the standard engine helper. | Use `upgrade.Swap` only as the rename primitive. Your updater must still handle daemon namespace locking, graceful restart, shutdown fallback, daemon start, ready wait, and status reporting. |

For the **stable launcher + versioned engine store** topology, do not swap the
launcher. Update the active engine pointer first, then ask the currently-running
daemon to restart with the intended successor executable:

```go
result, err := eng.RestartWithSuccessor(ctx, engine.RestartWithSuccessorOptions{
    SuccessorExe:    nextEngineExe,
    DrainTimeout:    30 * time.Second,
    RestartTimeout:  60 * time.Second,
    ShutdownTimeout: 10 * time.Second,
    ReadyTimeout:    30 * time.Second,
})
```

Existing shims wait briefly for the planned successor daemon before self-starting
their own executable. This prevents a live predecessor shim from resurrecting
the old binary during the successor's control-socket bind window.

### Apply Update and Restart

Consumers using the **fixed replaceable engine path** topology should prefer
`MuxEngine.ApplyUpdateAndRestart` over hand-rolled upgrade code. In this helper,
`CurrentExe` is the replaceable engine executable that should exist after the
swap, and `StagedExe` is the already-built replacement binary. `CurrentExe` is
not the configured stable launcher path unless your product deliberately makes
that path replaceable while live processes are running.

```go
result, err := eng.ApplyUpdateAndRestart(ctx, engine.UpdateAndRestartOptions{
    CurrentExe:      currentExe,
    StagedExe:       currentExe + "~",
    DrainTimeout:    30 * time.Second,
    RestartTimeout:  60 * time.Second,
    ShutdownTimeout: 10 * time.Second,
    ReadyTimeout:    10 * time.Second,
    CleanStale:      true,
})
if err != nil {
    var updateErr *engine.UpdateAndRestartError
    if errors.As(err, &updateErr) {
        // updateErr.Phase tells whether the failure happened during swap,
        // daemon lock, graceful restart, fallback shutdown, replacement
        // start, or ready wait. updateErr.Result reports partial success.
    }
    return err
}
_ = result
```

The helper:

1. Calls `upgrade.Swap(CurrentExe, StagedExe)`.
2. Acquires the daemon namespace lock for `engine.Config.Name` and `BaseDir`.
3. Sends `graceful-restart` with `successor_exe` set to `CurrentExe`.
4. Falls back to `shutdown` if graceful restart is unavailable or rejected.
5. Starts the replacement daemon from `CurrentExe` when a successor is not
   already ready.
6. Waits for the replacement daemon to answer `ping` before releasing the lock.

SessionHandler consumers such as aimux can use this in their own
`upgrade(action=apply)` path only when their product topology has a replaceable
engine path. If they use a stable launcher and versioned engine store, their
updater should update the engine pointer and invoke graceful restart with the
intended successor executable instead.

The shim process must survive for transparent reconnect to work. Muxcore keeps
the shim transport alive and reconnects it to the replacement daemon when
possible; it does not promise request replay. Requests active during reconnect
may receive explicit JSON-RPC errors by original ID.

For `SessionHandler` consumers, this is **transport and owner continuity**, not
a heap-state migration of the old daemon process. Muxcore preserves the
snapshot-restored owner registry, cached initialize/tools/prompts/resources
responses, classification, cwd/env metadata, reconnect-token history, and shim
reconnection. Product-private in-memory state inside the handler survives only
if the consumer persists it outside the daemon process or can reconstruct it in
the successor.

For a healthy SessionHandler hot update, the same open shim should reconnect by
token refresh under the successor daemon: `shim_reconnect_refreshed` increments,
`shim_reconnect_fallback_spawned` remains `0`, `shim_reconnect_gave_up` remains
`0`, and `handoff.restored_owner_count` is non-zero when owners were present.

## What muxcore Enforces

These guardrails exist in current muxcore:

| Guardrail | Where |
| --- | --- |
| Empty `engine.Config.Name` is rejected. | `engine.New` |
| Missing serving shape is rejected. | `engine.New` |
| `SessionHandler` takes priority over `Handler` in daemon mode; proxy mode requires `Handler`. | `engine.Run` |
| Daemon-managed owner connections require one-time tokens. | daemon spawn + owner accept loop |
| Reconnect first tries token refresh, then falls back to daemon spawn. | resilient client |
| Spawn is rejected while daemon shutdown/restart is in progress. | daemon spawn path |
| Live updates use one engine helper for swap, daemon lock, restart, fallback, start, and ready wait. | `engine.ApplyUpdateAndRestart` |
| Owner sockets are scoped by engine name. | serverid/daemon/owner paths |
| Stale-socket cleanup is name-scoped. | daemon startup |
| `AuthorizeSession` panics cannot crash the daemon. | owner accept loop |
| `OnFrameReceived` timeout/panic is fail-open. | owner frame dispatch |

## What muxcore Cannot Guess

The consumer must still decide these product-level facts:

- Whether the server is stateless, isolated, session-aware, or persistent.
- Which tenant/session policy should allow or deny a connection.
- Whether stdin EOF means client shutdown or only an internal pipe closing.
- How to package and update the product binary.
- Whether a structured `SessionHandler` also needs a raw `Handler` fallback for
  proxy mode.
- Which status fields are part of the consumer's own operator API.

When in doubt, prefer explicit configuration and a smoke test over relying on
auto-classification.

## Design Rule for Consumer-Facing APIs

Muxcore APIs should make the correct integration path the easiest path.

When adding or changing a consumer-facing feature:

1. If muxcore can infer a safe behavior from its own runtime facts, it should do
   so inside `engine` instead of requiring every consumer to copy the logic.
2. If muxcore cannot infer the answer safely, `engine.New` or `engine.Run`
   should reject the ambiguous configuration with an actionable error.
3. If a lower-level escape hatch is still needed, its documentation must name
   which engine guarantees the caller is taking over: daemon lifecycle, token
   minting, reconnect, snapshot restore, proxy mode, upgrade handoff, or status
   reporting.
4. Every new guardrail should ship with a regression test and an integration
   smoke that exercises the public entrypoint, not only the internal helper.

The preferred failure mode is "consumer cannot start with a bad muxcore wiring",
not "consumer starts and later flakes during reconnect, restart, or session
isolation".

## Anti-Patterns

Avoid these integration patterns:

- Calling `daemon.New` directly for ordinary consumers. Use `engine.New` unless
  you are implementing muxcore internals or a custom control plane.
- Reusing `Name: "mcp-mux"` in another product.
- Letting a CLI parser reject `--muxcore-daemon` before `engine.Run` can see it.
- Implementing only `SessionHandler` and then wrapping the binary with an
  external `mcp-mux`; proxy mode needs `Handler`.
- Connecting directly to owner IPC paths from a shim. That bypasses token
  minting and will be rejected.
- Treating `PeerPid == 0` or `PeerUid == 0` as a real identity.
- Doing slow work inside `OnFrameReceived`.
- Assuming `AuthorizeSession` prevents owner/upstream creation. It gates
  sessions, not necessarily owner allocation.
- Using `upgrade.Swap` as the only live-update mechanism on Windows for a
  heavily shared configured executable.
- Calling `ApplyUpdateAndRestart` with a stable launcher path when the real
  successor is selected by an active-engine pointer.
- Reporting update success without checking `UpdateAndRestartResult` or the
  `UpdateAndRestartError.Phase` and partial `Result`.

## Integration Verification

A muxcore consumer release should include at least these checks:

```bash
go test ./...
go vet ./...
```

Then run a customer-mode smoke through the same entrypoint your MCP host uses:

1. Start the binary normally and confirm the MCP host receives a valid
   `initialize` response.
2. Open two sessions and confirm shared/session-aware/isolated behavior matches
   your intended mode.
3. Restart the daemon and confirm the shim reconnects.
4. If you ship an updater, run it while a session is active and confirm the
   next daemon uses the intended engine or successor path.
5. Inspect `HandleStatus` / control-plane status for `daemon_generation`,
   `owner_generation`, `restore_source`, `handoff.restored_owner_count`, and
   reconnect counters. SessionHandler hot updates should show refresh-based
   reconnect (`shim_reconnect_refreshed > 0`) without fallback spawn or give-up.
6. Verify native muxcore products through their own MCP/CLI health and update
   surfaces. `mcp-mux serve` / `mux_list` observes the `mcp-mux` daemon
   namespace; it is not a generic registry for `aimux`, `engram`, or other
   native muxcore daemons unless those products explicitly opt into such a
   registry.

## Stable Operator Status Contract

`daemon.HandleStatus()` is the stable operator status API used by control
clients such as `mcp-mux serve` and `mux_list`. Fields may be added
compatibly, but released field names and meanings must not be removed, renamed,
or repurposed without release notes and a compatibility window.

Current lifecycle evidence fields:

| Field | Meaning |
| --- | --- |
| `engine_name` | Daemon namespace name such as `mcp-mux`, `aimux`, or `engram`; use it to avoid confusing product-native daemons with the `mcp-mux` product daemon. |
| `daemon_generation` | Process-lifetime generation string for distinguishing predecessor and successor daemons. |
| `reaped_owner_count` | Count of owners removed by idle lifecycle reaping. |
| `owner_removal.total` | Count of owner-removal helper executions in this daemon process. |
| `owner_removal.by_reason` | Removal counts keyed by reason, including `operator_hard`, `operator_soft`, `idle`, `zombie`, `handoff`, `restore_failed`, and `upstream_exit`. |
| `owner_removal.pending_tokens_removed` | Total owner-scoped pending session tokens removed during owner cleanup. |
| `owner_removal.bound_history_removed` | Total owner-scoped reconnect history entries removed during owner cleanup. |
| `handoff.predecessor_pid` | PID recorded by the daemon that wrote the snapshot, when known. |
| `handoff.predecessor_daemon_generation` | Generation recorded by the daemon that wrote the snapshot, when known. |
| `handoff.successor_daemon_generation` | Current daemon generation reported in the handoff status block. |
| `handoff.restored_owner_count` | Number of owners restored from snapshot or handoff into this daemon process. |
| `handoff.old_owner_socket_retired_count` | Number of predecessor owner socket pairs invalidated while restoring in handoff mode. |
| `servers[].owner_generation` | Per-owner generation string; changes when a fresh owner replaces a prior owner. |
| `servers[].restored_from_owner_generation` | Prior owner generation when an owner was restored from snapshot or handoff. |
| `servers[].restore_source` | Owner source classification: `fresh`, `snapshot_handoff`, or `snapshot_fallback`. |
| `shim_reconnect_refreshed` | Count of reconnects that recovered by refreshing a bound session token. |
| `shim_reconnect_fallback_spawned` | Count of reconnects that had to ask the daemon for a replacement owner spawn. |
| `shim_reconnect_gave_up` | Count of reconnect loops that exhausted recovery and gave up. |

## Handoff API

The `daemon` package exposes public functions for coordinating a live handoff
between an old daemon process and its successor during a zero-downtime upgrade.
Most consumers should use `engine.Run` and the daemon control command instead
of calling this directly. The direct API is useful when implementing a custom
daemon supervisor.

### Quick start

```go
import (
    "context"
    "net"

    "github.com/thebtf/mcp-mux/muxcore/daemon"
)

// Predecessor (old daemon): called after accepting a connection from the
// successor. conn must be a *net.UnixConn on Unix or a winio named-pipe
// connection on Windows.
func predecessor(conn net.Conn, token string, upstreams []daemon.HandoffUpstream) {
    result, err := daemon.PerformHandoff(context.Background(), conn, token, upstreams)
    if err != nil {
        // ErrTokenMismatch, ErrProtocolVersionMismatch, or transport error.
        return
    }
    _ = result.Transferred
    _ = result.Aborted
}

// Successor (new daemon): called after dialing the predecessor's handoff socket.
func successor(conn net.Conn, token string) {
    upstreams, err := daemon.ReceiveHandoff(context.Background(), conn, token)
    if err != nil {
        return
    }
    _ = upstreams
}
```

### Token lifecycle

```go
token, path, err := daemon.WriteHandoffToken(dir)
if err != nil {
    return err
}

token, err = daemon.ReadHandoffToken(path)
if err != nil {
    return err
}
defer daemon.DeleteHandoffToken(path)
```

### Error handling

| Error / Field | Meaning |
| --- | --- |
| `daemon.ErrTokenMismatch` | Successor presented the wrong pre-shared token; comparison is constant-time. |
| `daemon.ErrProtocolVersionMismatch` | Handoff protocol versions differ; fall back to shutdown/restore. |
| `result.Aborted` | Per-upstream list of server IDs not transferred. Other upstreams may still succeed. |

### Platform constraints

| Platform | Requirement |
| --- | --- |
| Unix (Linux, macOS) | `conn` must be `*net.UnixConn`; file descriptors transfer with `SCM_RIGHTS`. |
| Windows | `conn` must be a winio named-pipe connection; handles transfer with `DuplicateHandle`. |

On Windows the successor PID is supplied automatically through the handoff
protocol's `HelloMsg.SourcePID`; no additional caller configuration is needed.
