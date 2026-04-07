# Technical Debt

### 2026-04-07: CC probe-connect causes false "failed" status for some MCP servers
**What:** CC makes a short-lived probe connection (~3ms) before the real connection. If the probe fails or disconnects quickly, CC marks the server as "failed" without retrying. The real connection (session 48) succeeds but CC already decided.
**Why:** CC behavior — not controllable from mux side. Servers are alive in daemon (verified via mux_list), but CC shows "✘ failed".
**Impact:** playwright, socraticode, netcoredbg intermittently show as failed. Manual `/mcp` reconnect fixes them.
**Context:** Observed pattern: session N connects → cached init replayed → disconnect in 3ms → session N+1 connects → works fine. CC marks failed based on session N.
**Workaround:** `/mcp` reconnect. No fix possible without CC changes (retry logic for MCP server startup).
