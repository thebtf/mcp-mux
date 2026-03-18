# Contributing to mcp-mux

## Development Setup

```bash
git clone https://github.com/thebtf/mcp-mux.git
cd mcp-mux
go build ./...
go test ./...
```

Requirements: Go 1.25+

## Running Tests

```bash
make test          # all tests with -race
make test-v        # verbose output
make cover         # generate coverage report (coverage.html)
make vet           # static analysis
```

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Immutable patterns preferred — return new objects, don't mutate
- Files under 800 lines, functions under 50 lines
- Error messages: lowercase, no punctuation, wrap with context
- Test files colocated with source (`*_test.go`)

## Project Structure

```
cmd/mcp-mux/          CLI entry point, subcommands, daemon bootstrap
internal/mux/         Core multiplexer (Owner, Session, Client)
internal/daemon/      Global daemon, reaper/GC
internal/control/     Control plane protocol (NDJSON over Unix sockets)
internal/mcpserver/   MCP server for control plane (mux_list/stop/restart)
internal/classify/    Auto-classification (x-mux capability, tool heuristics)
internal/jsonrpc/     JSON-RPC message parsing, ID replacement, meta injection
internal/remap/       Request ID remapping (session-scoped)
internal/serverid/    Deterministic server identity hashing
internal/ipc/         Unix domain socket helpers
internal/upstream/    Upstream process lifecycle
testdata/             Mock MCP server for integration tests
docs/                 Public protocol specifications
```

## Pull Request Process

1. Fork and create a feature branch
2. Write tests first — target 70%+ coverage for new code
3. Run `make test` and `make vet` — all must pass
4. Commit with conventional format: `feat:`, `fix:`, `test:`, `docs:`, `chore:`
5. Open PR against `master`

## Architecture Principles

See `.agent/specs/constitution.md` for the 10 project principles. Key ones:

- **Transparent proxy** — invisible to both CC and upstream servers
- **Zero-configuration** — works without any config beyond wrapping the command
- **Upstream authority** — server decides sharing mode via x-mux capability
- **Session isolation** — no cross-session data leakage
- **No stubs** — complete implementations only

## Reporting Issues

Open an issue with:
- mcp-mux version (`mux_version` from `mcp-mux status`)
- OS and Go version
- Steps to reproduce
- Expected vs actual behavior
- Relevant log output (stderr from mcp-mux)
