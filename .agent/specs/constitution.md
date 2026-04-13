# Project Constitution

**Project:** mcp-mux
**Version:** 1.0.0
**Ratified:** 2026-03-17
**Last Amended:** 2026-03-17

## Principles

### 1. Transparent Proxy
mcp-mux MUST be invisible to both CC (downstream) and upstream MCP servers. No modification of JSON-RPC messages beyond ID remapping and `_meta` injection for session-aware servers. A server running through mcp-mux MUST behave identically to running without it.

**Rationale:** Users adopt mcp-mux by changing one line in config. If it changes behavior, trust is destroyed.

### 2. Zero-Configuration Default
mcp-mux MUST work without any configuration beyond wrapping the command. Auto-classification, daemon auto-start, caching, and env forwarding all happen automatically. Config exists for overrides, not for basic operation.

**Rationale:** Every config knob is a barrier to adoption. The default must be correct for 90% of cases.

### 3. Upstream Authority
The upstream MCP server is the source of truth. x-mux capability declarations override tool-name heuristics. Cached responses are invalidated on upstream notifications. mcp-mux MUST NOT invent capabilities, modify tool lists, or alter server behavior.

**Rationale:** mcp-mux is infrastructure, not middleware. It multiplexes, it doesn't transform.

### 4. Session Isolation Correctness
When sharing one upstream across N sessions, mcp-mux MUST guarantee: (a) responses route to the correct session, (b) no cross-session data leakage, (c) session disconnect does not affect other sessions.

**Rationale:** Incorrect routing = data corruption. This is the core correctness invariant.

### 5. Graceful Degradation
If daemon is unavailable, fall back to legacy per-session owner. If env forwarding fails, start upstream with daemon env. If classification is ambiguous, default to shared (safe for stateless servers). Never fail hard when a degraded mode exists.

**Rationale:** mcp-mux wraps 16+ servers. One failure mode must not take down all MCP connectivity.

### 6. Cross-Platform
All code MUST build and pass tests on Windows and Unix. Platform-specific code isolated behind build tags. Unix domain sockets (not named pipes) for IPC.

**Rationale:** Primary development on Windows, production may include Linux/macOS. No platform lock-in.

### 7. MCP Spec Compliance
Protocol version, capability negotiation, and message formats MUST follow the official MCP specification. Verify versions against GitHub releases (not training data or Context7 which may lag).

**Rationale:** Non-compliant proxy breaks unknown future clients. Spec is the contract.

### 8. Test Before Ship
No release without: (a) all tests green, (b) coverage >= 70% per package with 0% packages addressed, (c) manual verification of 16-server startup. No untested code paths in core routing.

**Rationale:** mcp-mux sits between CC and every MCP server. A bug here breaks all agent tooling.

### 9. Atomic Upgrades
Binary updates MUST be atomic (rename-swap) with rollback on failure. Running servers MUST NOT crash during upgrade. Daemon survives CC session restarts.

**Rationale:** Users run 4+ parallel CC sessions. Upgrade that kills sessions is unacceptable.

### 10. No Stubs, No Workarounds
Complete implementations only. InMemoryStore-style fakes, TODO comments, and "quick fix" approaches are prohibited. If a feature can't be implemented properly, defer it to TECHNICAL_DEBT.md.

**Rationale:** This is infrastructure software. Half-working features are worse than missing features.

## Governance

### Amendment Process
1. Propose change with rationale
2. Run /speckit-constitution with changes
3. Propagation check: verify all specs/plans/tasks align
4. Version bump (MAJOR for principle removal, MINOR for addition, PATCH for clarification)

### Compliance
- /speckit-analyze checks constitution alignment automatically
- Constitution MUST violations are always CRITICAL severity
- Principles can ONLY be changed via /speckit-constitution (not by editing specs)

### Version History

| Version | Date | Change |
|---------|------|--------|
| 1.0.0 | 2026-03-17 | Initial ratification — 10 principles |
