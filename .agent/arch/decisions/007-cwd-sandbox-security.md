# ADR-007: cwd Sandbox Security Model

## Status

Accepted

## Date

2026-03-15

## Context

MCP servers often need filesystem access (reading project files, writing outputs). When multiplexing requests from multiple CC sessions (potentially different projects), we need to ensure one session's requests cannot access another session's files.

## Decision

**Strict cwd sandbox per downstream client:**

1. Each downstream client declares its cwd when connecting
2. mcp-mux passes this cwd to the upstream server as the working directory context
3. All file path arguments in MCP requests are validated:
   - Resolve to absolute path
   - Check that resolved path starts with the client's cwd
   - Resolve symlinks and re-check (prevent symlink escape)
   - Block `../` traversal beyond cwd boundary
4. Violations return a JSON-RPC error to the client

## Scope

This applies at the mux layer, before requests reach the upstream server. The upstream server may have its own security model (e.g., Serena's project activation), but mcp-mux provides a baseline guarantee.

## Rationale

- Multiple CC sessions may be working in different project directories
- Without sandbox, a shared MCP server could be tricked into accessing files outside the intended project
- Symlink checking prevents the classic symlink-escape attack
- Defense in depth — even if the upstream server has no path checking, mux provides it

## Consequences

- Adds small overhead per request with file path arguments (resolve + check)
- Must identify which MCP request parameters contain file paths — needs per-method knowledge or generic path detection
- Some MCP servers may legitimately need access outside cwd (e.g., global config) — need escape hatch (configurable allowed paths)
- Isolated mode servers skip sandbox (they are 1:1 with the client, so the client controls the context)
