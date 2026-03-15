# Phase 1 PoC — Implementation Plan

## Goal

Working mcp-mux binary that can wrap any MCP server command, multiplex multiple CC sessions through a single upstream instance via IPC (named pipes / Unix sockets).

## Architecture (PoC scope)

```
CLIENT mode (dumb pipe):
  own stdin ──> IPC connection (write)
  IPC connection (read) ──> own stdout

OWNER mode (multiplexer):
  IPC listener + own stdio ──> sessions[]
  sessions[] ──parse JSON-RPC──> remap ID ──> upstream stdin
  upstream stdout ──parse JSON-RPC──> de-remap ID ──> route to correct session
  notifications (no id) ──> broadcast to all sessions
```

Key insight: client is a raw byte bridge. Owner does all JSON-RPC parsing and multiplexing. IPC uses same framing as stdio (newline-delimited JSON).

## Project Structure

```
mcp-mux/
├── cmd/mcp-mux/main.go         # Entry point, arg parsing, mode selection
├── internal/
│   ├── jsonrpc/
│   │   ├── message.go           # JSON-RPC 2.0 message types
│   │   ├── scanner.go           # Newline-delimited JSON reader
│   │   └── message_test.go      # Unit tests
│   ├── remap/
│   │   ├── remap.go             # ID remapping logic
│   │   └── remap_test.go        # Unit tests
│   ├── upstream/
│   │   ├── process.go           # Spawn and manage upstream process
│   │   └── process_test.go      # Unit tests
│   ├── ipc/
│   │   ├── server.go            # Named pipe / Unix socket listener
│   │   ├── client.go            # Connect to existing owner
│   │   ├── transport.go         # Platform abstraction
│   │   ├── transport_windows.go # Windows named pipe impl
│   │   ├── transport_unix.go    # Unix domain socket impl
│   │   └── ipc_test.go          # Unit tests
│   ├── mux/
│   │   ├── owner.go             # Owner mode: mux core + IPC server
│   │   ├── client.go            # Client mode: stdio ↔ IPC bridge
│   │   ├── session.go           # Session abstraction (one per downstream)
│   │   └── mux_test.go          # Unit tests
│   └── serverid/
│       ├── serverid.go          # Server identity hash computation
│       └── serverid_test.go     # Unit tests
├── testdata/
│   └── mock_server.go           # Simple echo MCP server for testing
├── go.mod
├── Makefile
└── .gitignore
```

## Tasks (dependency order)

### Phase 1a: Project scaffold + JSON-RPC parser [no deps]
- [x] Initialize Go module (`go mod init github.com/bitswan-space/mcp-mux`)
- [ ] JSON-RPC 2.0 message types (Request, Response, Notification)
- [ ] Newline-delimited JSON scanner (reads from io.Reader, emits messages)
- [ ] Tests: parse request, response, notification, malformed input, batch

### Phase 1b: ID remapper [depends on 1a]
- [ ] Remap function: (sessionID, originalID) → prefixed string "s{N}:{id}"
- [ ] Deremap function: prefixed string → (sessionID, originalID, originalType)
- [ ] Handle all JSON-RPC id types: number, string, null
- [ ] Tests: remap/deremap roundtrip, type preservation, null handling

### Phase 1c: Server identity [no deps]
- [ ] Hash computation: SHA256(command + args + sorted env keys)
- [ ] IPC path derivation: \\.\pipe\mcp-mux-{hash[:16]} (Windows) or /tmp/mcp-mux-{hash[:16]}.sock (Unix)
- [ ] Tests: deterministic hash, different commands → different IDs

### Phase 1d: IPC transport [no deps]
- [ ] Platform abstraction: Listener interface, Dial function
- [ ] Windows: named pipe (net.Pipe or winio)
- [ ] Unix: domain socket (net.Listen("unix", path))
- [ ] Tests: listen + connect + send/receive

### Phase 1e: Upstream process manager [depends on 1a]
- [ ] Spawn child process with command + args + env
- [ ] Bridge: write to child stdin, read from child stdout
- [ ] Handle child stderr (log or discard)
- [ ] Detect child exit, propagate error
- [ ] Tests: spawn echo process, send/receive, handle crash

### Phase 1f: Mux core — Owner mode [depends on 1a, 1b, 1d, 1e]
- [ ] Owner struct: holds upstream, sessions map, IPC listener
- [ ] Accept IPC connections → create session
- [ ] Own stdio → first session
- [ ] Per-session goroutine: read JSON-RPC → remap → upstream
- [ ] Upstream goroutine: read JSON-RPC → deremap → route to session
- [ ] Notification broadcast to all sessions
- [ ] Session cleanup on disconnect
- [ ] Graceful shutdown

### Phase 1g: Mux core — Client mode [depends on 1d]
- [ ] Connect to owner IPC endpoint
- [ ] Bridge: stdin → IPC write; IPC read → stdout
- [ ] Detect IPC disconnect → exit with error

### Phase 1h: Main entry point [depends on 1c, 1f, 1g]
- [ ] Parse argv: mcp-mux [flags] <command> [args...]
- [ ] Compute server_id from command + args
- [ ] Try connect to IPC → client mode
- [ ] Failed → create IPC listener → owner mode
- [ ] `mcp-mux status` subcommand (basic: list active pipes)
- [ ] `--isolated` flag (skip IPC, always owner mode)

### Phase 1i: Integration test [depends on all]
- [ ] Mock MCP server (echo server with tools/list support)
- [ ] Test: 1 owner + 1 client, concurrent requests, correct routing
- [ ] Test: client disconnect doesn't crash owner
- [ ] Test: same JSON-RPC id from different sessions → correct routing

## Success Criteria

1. `mcp-mux node mock_server.js` starts and proxies MCP protocol correctly
2. Second `mcp-mux node mock_server.js` connects to first instance via IPC
3. Both instances can send concurrent requests with same id=1, get correct responses
4. Disconnecting one instance doesn't affect the other
5. `mcp-mux status` shows active connections
6. Works on Windows (primary) and Linux

## Non-goals (PoC)

- Isolated mode (just `--isolated` flag skeleton)
- `mcp-mux setup` command
- cwd sandbox (security — Phase 2)
- Health monitoring / restart
- Transport adapters
