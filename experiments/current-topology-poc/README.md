# Current Topology PoC

Standalone experiment for the topology:

```text
MCP consumer -> shim -> daemon -> real MCP
```

This program intentionally imports no existing `muxcore` code. It tests whether
the topology can work when the lifecycle rules are explicit:

- daemon `ready` is true only after the owner listener is bound
- daemon and owner generations are minted separately
- spawn tickets are one-shot and bound to both generations
- stale generation/token mismatch fails closed with a structured error

Run:

```powershell
.\scripts\run-current-topology-poc.ps1 -WatchSeconds 1
```

Expected result:

```text
PASS current-topology PoC
```

The runner uses `mcp-launcher` for MCP startup and tool-call sessions. On
Windows, lifecycle checks that need `-ctl` are emulated through the PoC's native
control path because muxcore maps IPC paths to named pipes there; external
helpers that dial the path as raw AF_UNIX cannot manage that control socket.
The persist check also stays status-backed because `mcp-launcher -mode persist`
currently treats live processes as dead when `Process.Signal(0)` returns
Windows `EWINDOWS`.

The idle-reaper probe owns a nested runtime and control namespace. Its cleanup
captures the private daemon before requesting shutdown, then waits for Windows
OS process exit (or control-listener retirement on other systems) within the
existing two-second cleanup deadline. Only successful cleanup removes that
runtime and emits the success JSON; failures retain it and exit nonzero.

See `PHASES.md` for the current experiment ladder. Each phase adds one
production-like mechanism and must keep the same runner green.
