# Research: MCP Working Directory & Roots Propagation for Multiplexer

Full report from Gemini Deep Research + multi-model dialog.
See accumulated_content in aimux session for raw data.

## Executive Summary

MCP spec supports dynamic roots via `roots/list` + `notifications/roots/list_changed`.
But roots are per-session advisory boundaries, NOT per-request context.
There is no mechanism in MCP to specify "which root" a tool call belongs to.

## Three Architectural Approaches

### Approach A: ServerID = hash(command + args + cwd) — "Isolated by Policy"
- Same cwd = share, different cwd = separate upstream
- Zero protocol introspection
- DOES NOT solve N×M for cross-project use (main use case)

### Approach B: Path Rewriting — "Virtualized Roots"
- Parse every tool call, identify path arguments, rewrite them
- IMPOSSIBLE without per-server JSON schema knowledge
- Fragile, maintenance nightmare

### Approach C: Roots Aggregation — "Union of Roots" (MCP-compliant)
- When upstream asks roots/list, respond with UNION of all client cwds
- When new client joins, send roots/list_changed to upstream
- Upstream re-fetches roots and sees expanded boundaries
- LLM (not mux) provides absolute paths in tool calls
- Works IF: server respects roots AND LLM sends absolute paths

### Approach C Protocol Sequence:
1. Upstream sends roots/list → Mux intercepts
2. Mux fans out to all connected clients, collects their roots
3. Mux responds with aggregated array
4. New client joins → Mux sends roots/list_changed to upstream
5. Upstream re-queries roots/list → gets updated union

## Server Classification

| Server | CWD-dependent? | Roots-aware? | Shareable cross-project? |
|--------|---------------|-------------|------------------------|
| time, tavily, nia | No | No | YES — trivially |
| openrouter, n8n, notion | No | No | YES — trivially |
| github, memory, context7 | No | No | YES (already HTTP) |
| serena | Yes (process cwd + LSP state) | Unclear | RISKY — LSP state bound to one project |
| cclsp | Yes (LSP workspace) | Unclear | RISKY — spawns per-project LSP |
| context-please | Yes (indexes cwd) | Unclear | RISKY — index bound to cwd |
| desktop-commander | Per-session state | No | NO — isolated |
| playwright, pencil, aimux | Per-session state | No | NO — isolated |

## Recommendation

Two-tier strategy:
1. **Stateless servers** (time, tavily, nia, openrouter, n8n, notion, wsl):
   ServerID = hash(command + args) WITHOUT cwd. One instance globally shared.

2. **cwd-dependent servers** (serena, cclsp, context-please):
   Implement Approach C (roots aggregation) as best-effort.
   Fallback: ServerID = hash(command + args + cwd) if server doesn't respect roots.

3. **Stateful servers** (aimux, playwright, desktop-commander, pencil):
   Always --isolated.
