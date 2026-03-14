# Implementation Plan: mcp-mux

## Phase 1: MVP — Shared Mode (core value)

### 1.1 Project Setup
- `package.json` with TypeScript config
- `tsconfig.json` targeting ES2022, Node20
- Entry point: `src/index.ts` (daemon), `src/client.ts` (wrapper)
- Zero npm deps — use Node.js stdlib only

### 1.2 Config Loader
- Parse TOML or JSON config file
- Validate server definitions (command, args, env, mode)
- Generate default config from existing `.mcp.json`

### 1.3 Upstream Manager
- Spawn MCP server as child process (stdio pipe)
- Send `initialize`, cache `InitializeResult`
- Send `tools/list`, build tool→server routing table
- Handle process crash → log, attempt restart
- Lazy spawn — only start upstream when first request arrives

### 1.4 ID Remapper
- Parse incoming JSON-RPC message
- For requests (has `id`): prefix with session id → `"s{N}:{id}"`
- For responses: strip session prefix, restore original id + type
- For notifications (no `id`): pass through / broadcast

### 1.5 Downstream Listener (Named Pipe)
- Create named pipe server (Windows: `net.createServer` on `\\.\pipe\mcp-mux`)
- Accept connections, assign session id (incrementing counter)
- Parse newline-delimited JSON-RPC from each client
- Route `initialize` → return cached result
- Route `tools/list` → return merged tool list
- Route `tools/call` → lookup tool→upstream, remap id, forward
- Route response back to correct session

### 1.6 Wrapper Client (mcp-mux-client)
- Bridge CC's stdio (stdin/stdout) to named pipe connection
- Auto-start daemon if not running (check pipe exists)
- Transparent — CC doesn't know it's talking to a proxy

### 1.7 Testing
- Unit: id remapper, config parser, tool routing
- Integration: 2 mock upstreams, 2 mock clients, concurrent requests
- Verify: correct response routing, no cross-session leaks

## Phase 2: Isolated Mode

- Per-session upstream spawning for `mode = "isolated"` servers
- Session lifecycle management (spawn on connect, kill on disconnect)
- No id remapping needed — 1:1 pipe

## Phase 3: Reliability

- Upstream health monitoring (process alive check)
- Auto-restart on upstream crash (configurable max retries)
- Status endpoint (IPC command: `mcp-mux-client --status`)
- Graceful shutdown (drain pending requests, then terminate)

## Phase 4: DX & Integration

- Config generator: read existing `.mcp.json` → generate mux config
- `.mcp.json` generator: output single mux entry for CC
- Documentation: README, setup guide, troubleshooting
- npm package publication (optional)

## Success Criteria

- [ ] 4 CC sessions share 6 upstream servers with correct isolation
- [ ] Memory usage < 2 GB (vs 4.8 GB current)
- [ ] Latency overhead < 1ms per request
- [ ] Zero cross-session response leaks
- [ ] Works on Windows 11 (primary) and Linux (secondary)
