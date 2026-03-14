# mcp-mux — MCP Stdio Multiplexer

## STACKS

```yaml
STACKS: [TYPESCRIPT]
```

## PROJECT OVERVIEW

A local stdio multiplexer/proxy for MCP (Model Context Protocol) servers. Allows multiple Claude Code sessions to share a single instance of each MCP server, reducing process count and memory usage by ~3x.

### Problem

Each Claude Code session spawns its own copy of every configured MCP server (stdio transport). With 4 parallel sessions × 12 MCP servers = ~48 node processes consuming ~4.8 GB RAM. Most of these servers are stateless — they don't need per-session isolation.

### Solution

A single `mcp-mux` process sits between CC sessions and upstream MCP servers. Each CC session connects to mcp-mux via stdio (standard MCP transport). mcp-mux maintains one shared upstream connection per MCP server and multiplexes requests via JSON-RPC id remapping.

```
CC Session 1 ──stdio──┐
CC Session 2 ──stdio──┤──> mcp-mux ──stdio──> 1× engram
CC Session 3 ──stdio──┤               ──stdio──> 1× tavily
CC Session 4 ──stdio──┘               ──stdio──> 1× context7
```

### Key Concepts

- **Upstream**: A real MCP server process (e.g., engram, tavily)
- **Downstream**: A CC session connecting to mcp-mux
- **Shareable**: Server whose requests are stateless (safe to multiplex)
- **Isolated**: Server that requires per-session state (SSH connections, browser sessions)

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
