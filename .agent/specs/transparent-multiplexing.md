# Specification: Transparent Multiplexing

## Overview

Make mcp-mux fully transparent — users write `mcp-mux <command>` without flags, and mcp-mux automatically determines the optimal sharing strategy per server. Three implementation horizons.

## Functional Requirements

### Horizon 1: Convention-based auto-detection (near-term)
- FR1: After upstream `initialize` + `tools/list`, mcp-mux analyzes tool names/descriptions to classify server as shared or isolated
- FR2: Known patterns (browser_*, start_process, etc.) trigger isolated mode automatically
- FR3: Override via env var `MCP_MUX_ISOLATED=1` / `MCP_MUX_SHARED=1` for edge cases
- FR4: `--isolated` and `--stateless` flags remain for backward compatibility

### Horizon 2: Transport Translation (near-term, parallel with H1)
- FR5: For servers that support HTTP/SSE transport, mcp-mux launches upstream in HTTP mode instead of STDIO
- FR6: mcp-mux proxies JSON-RPC from client STDIO → HTTP POST with per-client `Mcp-Session-Id`
- FR7: Per-session isolation is achieved natively by the server's HTTP factory (e.g. Playwright `--port`)
- FR8: Detection: check if upstream binary accepts `--port` flag or has known HTTP mode
- FR9: Fallback: if HTTP mode unavailable or fails, use STDIO (shared or isolated per H1 rules)

### Horizon 3: MCP Session Identity adoption (future, when spec lands)
- FR10: Monitor MCP spec evolution for session identity in data layer (cookie-like mechanism)
- FR11: When available, use native session identity for per-client isolation over STDIO
- FR12: Deprecate transport translation for servers that adopt native session identity
- FR13: Maintain backward compatibility with servers that don't adopt the new spec

## Non-Functional Requirements
- NFR1: Zero user configuration for 90%+ of servers (only env override for edge cases)
- NFR2: Latency overhead for HTTP proxy < 5ms per request
- NFR3: Clean fallback chain: native session ID → HTTP transport → shared STDIO → isolated STDIO

## Acceptance Criteria
- [ ] AC1: `mcp-mux npx @playwright/mcp` auto-detects as needing isolation, uses HTTP mode if available
- [ ] AC2: `mcp-mux npx tavily-mcp` auto-detects as stateless, uses shared STDIO
- [ ] AC3: `mcp-mux uvx serena` auto-detects as stateless, uses shared STDIO
- [ ] AC4: No `--isolated` or `--stateless` flags needed in any .mcp.json config
- [ ] AC5: `mcp-mux status` shows transport mode per server (stdio-shared, stdio-isolated, http)
- [ ] AC6: MCP session identity spec changes tracked in .agent/arch/decisions/

## Out of Scope
- Modifying upstream MCP server code
- Supporting MCP servers that only work over network (remote HTTP servers)
- Multi-machine multiplexing (mcp-mux is local-only)

## Dependencies
- MCP protocol spec (current: 2025-06-18, watching: session identity SEP)
- Playwright MCP `--port` mode (verified: exists in v1.0.7+)
- Go net/http for HTTP proxy implementation
