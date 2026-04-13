# Feature: Synthetic Progress Reporter

**Slug:** synthetic-progress-reporter
**Created:** 2026-04-13
**Status:** Draft
**Author:** AI Agent (reviewed by user)

> **Provenance:** Specified by claude-opus-4-6 on 2026-04-13.
> Evidence from: MCP spec (Context7), CC source analysis (client.ts, MCPTool/UI.tsx),
> mcp-mux codebase (owner.go inflight tracker + progressOwners), research report
> (.agent/reports/research-synthetic-progress-2026-04-13.md).
> Confidence: ALL findings VERIFIED (G12).

## Overview

mcp-mux generates synthetic `notifications/progress` messages for long-running MCP tool calls,
providing elapsed time and tool name to Claude Code's UI instead of a generic "Running..." spinner.
This leverages existing inflight tracker data and the progress routing infrastructure already
present in Owner.

## Context

Most MCP servers (engram, tavily, serena, etc.) do not implement `notifications/progress`.
When a tool call takes >5 seconds, CC shows only "Running..." with no indication of what's
happening or how long it has been waiting. CC's MCP SDK automatically injects `progressToken`
into every interactive tool call request, and CC renders any received progress notifications
in the UI (text for indeterminate, progress bar for determinate). mcp-mux already captures
these tokens via `trackProgressToken()` and routes real upstream progress via
`routeProgressNotification()`. The missing piece: generating synthetic notifications when
upstream stays silent.

## Functional Requirements

### FR-1: Synthetic Progress Emission
The system must send `notifications/progress` to the correct downstream session for any
`tools/call` request that has been inflight longer than a configurable threshold, when
the upstream server has not sent any real progress notification for that request.

### FR-2: Progress Message Content
Each synthetic notification must include a human-readable `message` field containing
the tool name (if available) or method name, plus the elapsed time in seconds.
Format: `"{tool_or_method}: {elapsed}s elapsed"`.

### FR-3: Monotonically Increasing Progress Counter
The `progress` field must use seconds elapsed since request start as the counter value
(e.g., 5, 10, 15...), increasing by the interval amount on each tick. No `total` field
(indeterminate progress).

### FR-4: Real Progress Deduplication
The system must suppress synthetic notifications for any request that has received a real
`notifications/progress` from upstream within the current reporting interval. If upstream
stops sending real progress, synthetic notifications resume after one full interval of silence.

### FR-5: Configurable Interval
The reporting interval must be configurable via `x-mux.progressInterval` capability
in the server's `initialize` response (range: 1-60 seconds). Default: 5 seconds when
capability is not present.

### FR-6: Lifecycle Management
The reporter goroutine must start when Owner begins serving and stop when Owner shuts down.
It must not leak goroutines, timers, or references after Owner shutdown.

## Non-Functional Requirements

### NFR-1: Performance
The reporter tick must complete in <1ms for typical workloads (up to 50 concurrent inflight
requests). No allocations on tick when no inflight requests exceed the threshold.

### NFR-2: Thread Safety
All access to inflight tracker, progressOwners, and requestToTokens must be safe for
concurrent use. The reporter goroutine must not hold locks longer than necessary.

### NFR-3: Zero Regression
Existing progress routing (real upstream notifications) must work identically. Existing
test suite (10 packages) must pass without modification.

## User Stories

### US1: Visible Tool Call Progress (P1)
**As a** Claude Code user, **I want** to see which MCP tool is running and how long it has
been waiting, **so that** I know my request is being processed and can estimate when it
will complete.

**Acceptance Criteria:**
- [ ] After 5 seconds of a `tools/call` being inflight, CC UI shows "{tool_name}: 5s elapsed"
- [ ] The message updates every 5 seconds (10s, 15s, 20s...)
- [ ] When the tool call completes, synthetic notifications stop immediately
- [ ] When upstream sends real progress, synthetic notifications are suppressed

### US2: Server-Specific Interval (P2)
**As an** MCP server author who supports x-mux, **I want** to configure the progress
reporting interval, **so that** I can set a longer interval for naturally slow operations
or a shorter one for responsive feedback.

**Acceptance Criteria:**
- [ ] Server advertises `x-mux.progressInterval: 10` in initialize response
- [ ] mcp-mux reads the value and uses 10s instead of default 5s
- [ ] Values outside 1-60 range are clamped with a warning log

### US3: No False Progress (P2)
**As a** Claude Code user, **I want** synthetic progress to stop when real progress arrives,
**so that** I don't see duplicate or conflicting progress information.

**Acceptance Criteria:**
- [ ] If upstream sends `notifications/progress` for a token, synthetic stops for that token
- [ ] If upstream stops sending for one full interval, synthetic resumes
- [ ] Synthetic and real notifications never overlap within the same interval

## Edge Cases

- Tool call completes during the same tick as synthetic emission: race resolved by checking
  inflightTracker before sending (request already deleted = skip).
- Multiple tokens for the same request (rare but possible per `requestToTokens`): emit
  synthetic on the first token only, or all tokens. Decision: emit on all tokens — CC
  deduplicates by toolUseId internally.
- Owner has zero inflight requests: reporter tick is a no-op (scan inflightTracker, find nothing, return).
- Upstream sends progress with `total` field (determinate): once real progress is seen, synthetic
  must NOT override it even after silence, to avoid replacing a percentage bar with text.
- progressToken not present (non-CC client): no entry in requestToTokens, nothing to emit. Safe no-op.
- lastRealProgress entries are cleaned up in `clearProgressTokensForRequest()` alongside
  progressOwners and requestToTokens — no stale state accumulation.
- session.WriteRaw() fails during synthetic emission (session disconnected): log at debug level
  and skip — consistent with existing routeProgressNotification() drop-on-failure behavior.

## Out of Scope

- Generating deterministic progress (progress bar with total) — we don't know upstream's completion state
- Modifying CC's UI rendering — we only emit standard MCP notifications
- Supporting `notifications/progress` for non-tools/call methods (resources/read, prompts/get) — tools/call only
- Injecting synthetic `progressToken` into requests that lack one — CC SDK already provides tokens

## Dependencies

- Existing `inflightTracker` (sync.Map on Owner) — provides method, tool, session, start time
- Existing `progressOwners` map — provides progressToken → sessionID routing
- Existing `requestToTokens` map — provides requestID → []progressToken lookup
- Existing `session.WriteRaw()` — sends raw JSON to downstream
- `internal/classify` — for parsing `x-mux.progressInterval` from initialize response

## Success Criteria

- [ ] CC displays tool name + elapsed time for any MCP tool call >5s
- [ ] No synthetic notifications when upstream sends real progress
- [ ] Configurable interval via x-mux.progressInterval capability
- [ ] Zero goroutine leaks after Owner shutdown
- [ ] All existing tests pass (10 packages)
- [ ] Dedicated unit tests for reporter goroutine (tick logic, dedup, lifecycle)

## Clarifications

### Session 2026-04-13

| # | Category | Question | Resolution | Date |
|---|----------|----------|------------|------|
| C1 | Data Lifecycle | How are lastRealProgress entries cleaned up? | In clearProgressTokensForRequest(), same path as progressOwners | 2026-04-13 |
| C2 | Reliability | What happens if session.WriteRaw() fails during synthetic emit? | Log debug + skip, consistent with routeProgressNotification() | 2026-04-13 |

## Open Questions

None — all research questions resolved (see research report). All findings VERIFIED.
