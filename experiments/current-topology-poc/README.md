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

The runner uses `mcp-launcher` for MCP startup, tool calls, crash reconnect, and
restart smoke. On Windows it uses a status-backed persist emulation because
`mcp-launcher -mode persist` currently treats live processes as dead when
`Process.Signal(0)` returns Windows `EWINDOWS`.

See `PHASES.md` for the current experiment ladder. Each phase adds one
production-like mechanism and must keep the same runner green.
