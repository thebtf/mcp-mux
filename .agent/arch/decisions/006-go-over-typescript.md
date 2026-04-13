# ADR-006: Go Over TypeScript

## Status

Accepted

## Date

2026-03-15

## Context

The original spec chose TypeScript/Node.js for familiarity with the MCP ecosystem (most MCP servers are Node). The 2026-03-15 brainstorm reconsidered this given mcp-mux's core responsibilities: process management, stream multiplexing, IPC coordination, and cross-platform binary distribution.

### Options Considered

| Language | Pros | Cons |
|----------|------|------|
| TypeScript/Node | MCP ecosystem familiarity, fast JSON | Runtime dependency, single event loop, weak process mgmt |
| Go | Single binary, goroutines, excellent os/exec and net, fast cross-compile | Less ergonomic JSON handling |
| Rust | Best performance, zero-cost async, memory safety | Slower development, heavier cross-compile |

## Decision

**Go**.

## Rationale

- **Single static binary**: no Node.js runtime required on target machine — critical for distribution
- **Goroutines**: natural fit for per-connection concurrency (one goroutine per downstream client, one per upstream)
- **Process management**: Go's `os/exec`, `io.Pipe`, `io.Copy` are purpose-built for what mcp-mux does
- **Named pipes/sockets**: `net` package handles both uniformly
- **Cross-compilation**: `GOOS=windows GOARCH=amd64 go build` — trivial
- **Startup time**: Go binary starts in <10ms vs Node.js ~200ms
- **Memory**: Go binary ~10-15 MB RSS vs Node.js ~50-80 MB

Rust was considered but deemed overkill — mcp-mux is a proxy, not a high-frequency trading system. Go's performance is more than sufficient and development velocity is significantly higher.

## Consequences

- JSON-RPC parsing less ergonomic than JS — mitigated by `encoding/json` with raw messages
- No native MCP SDK in Go — but we only need JSON-RPC 2.0, which is trivial to implement
- Team needs Go proficiency (existing: engram backend is Go)
- Build toolchain: Go 1.22+ with `go build` — no webpack, no bundler, no node_modules
