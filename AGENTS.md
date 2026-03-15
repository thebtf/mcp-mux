# mcp-mux — MCP Stdio Multiplexer

## STACKS

```yaml
STACKS: [GO]
```

## PROJECT OVERVIEW

A local stdio multiplexer/proxy for MCP (Model Context Protocol) servers. Allows multiple Claude Code sessions to share a single instance of each MCP server, reducing process count and memory usage by ~3x.

### Problem

Each Claude Code session spawns its own copy of every configured MCP server (stdio transport). With 4 parallel sessions × 12 MCP servers = ~48 node processes consuming ~4.8 GB RAM. Most of these servers are stateless — they don't need per-session isolation.

### Solution

`mcp-mux` is a transparent command wrapper. User prefixes their MCP server command with `mcp-mux`:

```json
{ "command": "mcp-mux", "args": ["uvx", "--from", "...", "serena", ...] }
```

First mcp-mux instance for a given server becomes the "owner" (spawns upstream, listens on IPC). Subsequent instances connect as clients.

```
CC 1 ──stdio──> mcp-mux ──IPC──┐
CC 2 ──stdio──> mcp-mux ──IPC──┤──> mcp-mux (owner) ──stdio──> engram (1×)
CC 3 ──stdio──> mcp-mux ──IPC──┤
CC 4 ──stdio──> mcp-mux ──IPC──┘
```

### Key Concepts

- **Upstream**: A real MCP server process (e.g., engram, tavily)
- **Downstream**: A CC session connecting via mcp-mux wrapper
- **Owner**: First mcp-mux instance — spawns upstream, accepts IPC connections
- **Client**: Subsequent mcp-mux instances — connect to owner via IPC
- **Shared** (default): One upstream serves all clients
- **Isolated** (`--isolated`): Each client gets its own upstream

## CONVENTIONS

- Investigation reports: `.agent/reports/YYYY-MM-DD-topic.md`
- Diagnostic data: `.agent/data/topic.md`
- Action plans: `.agent/plans/topic.md`
- Specifications: `.agent/specs/topic.md`
- Architecture decisions: `.agent/arch/decisions/NNN-title.md`

## RULES

| Rule | Description |
|------|-------------|
| **No stubs** | Complete, working implementations only |
| **No guessing** | Verify with tools before using |
| **Reasoning first** | Document WHY before implementing |
| **Spec compliance** | MCP protocol spec is authoritative — verify all protocol behavior against it |

## INSTRUCTION HIERARCHY

```
System prompts > Task/delegation > Global rules > Project rules > Defaults
```
