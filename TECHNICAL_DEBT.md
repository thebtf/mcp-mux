# Technical Debt

### 2026-04-08: Progress notifications forwarding
**What:** MCP protocol supports `notifications/progress` with `progressToken`. mux should forward these from upstream to the originating session (not broadcast). Servers like netcoredbg could report build progress if mux reliably delivers progress notifications.
**Why:** Currently CC shows only "Running…" with no indication of what's happening inside a long tool call.
**Impact:** Users cannot distinguish "working slowly" from "deadlocked".
**Context:** netcoredbg `start_debug` with `pre_build` sends build output via `ctx.info()` — these are MCP notifications that mux broadcasts synchronously (the broadcast deadlock we're fixing now).

### 2026-04-08: Tool call timeout (large, anti-deadlock)
**What:** Add configurable tool call timeout in mux. If upstream doesn't respond to a `tools/call` within N seconds, mux generates a JSON-RPC error response and returns it to the session. Default: 300s (5 minutes). Configurable via `x-mux.timeout` capability or env var.
**Why:** Prevents eternal hangs when upstream deadlocks or crashes silently.
**Impact:** CC stops showing "Running…" forever, user gets actionable error.
**Context:** netcoredbg `start_debug` hung for 7+ minutes due to broadcast deadlock. Even after deadlock fix, upstream bugs can cause infinite hangs.

### 2026-04-08: Inflight progress in CC (if feasible)
**What:** Explore whether mux can inject `notifications/progress` to CC based on inflight tracking data (elapsed time, method, tool name). CC would show "start_debug: 45s elapsed" instead of just "Running…".
**Why:** mux already tracks inflight requests with timestamps. Sending periodic progress notifications to CC would give real-time visibility.
**Impact:** User sees what's happening without needing `mux_list --verbose`.
**Context:** Requires CC to support progress display for MCP tool calls. May need `_meta.progressToken` injection.
