# Implementation Plan: Inflight Request Observability

**Spec:** .agent/specs/inflight-observability/spec.md
**Created:** 2026-04-07
**Status:** Draft

> **Provenance:** Planned by Claude Opus 4.6 on 2026-04-07.
> Evidence from: spec.md, codebase (session_manager.go, owner.go handleDownstreamMessage).
> Confidence: VERIFIED (data flow traced through code).

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Tracking map | sync.Map | Already used for methodTags, zero alloc when empty |
| Time | time.Now() | stdlib, no external deps |
| JSON parsing | encoding/json | Extract params.name from tools/call |

## Architecture

```
Session → handleDownstreamMessage → [TRACK: method, tool, session, time]
                                          ↓
                                    forward to upstream
                                          ↓
handleUpstreamMessage → response → [UNTRACK: remove by remapped ID]
                                          ↓
Owner.Status() → reads tracking map → inflight[]
                                          ↓
mux_list --verbose → includes inflight array
```

## Data Model

### InflightRequest
| Field | Type | Notes |
|-------|------|-------|
| Method | string | JSON-RPC method (e.g. "tools/call", "initialize") |
| Tool | string | params.name for tools/call, empty otherwise |
| SessionID | int | Originating session, 0 for proactive init |
| StartTime | time.Time | When forwarded to upstream |

## File Structure

```
internal/mux/
  owner.go          # Add: inflightTracker sync.Map, track/untrack in handleDownstream/Upstream
  owner.go          # Modify: Status() to include inflight array
  snapshot.go       # No change (inflight is transient, not persisted)
```

## Phases

### Phase 1: Inflight Tracker + Status Output
- Add `inflightTracker sync.Map` to Owner (remapped ID → InflightRequest)
- Track in handleDownstreamMessage when forwarding request to upstream
- Extract tool name from tools/call params
- Untrack in handleUpstreamMessage when response arrives
- Also untrack in drainInflightRequests (upstream death)
- Expose in Status() as `inflight` array

### Phase 2: Tests
- Test: track request → Status shows inflight → untrack → Status empty
- Test: tools/call extracts tool name
- Test: malformed params → empty tool name
- Test: proactive init shown as session 0
- Full regression

## Library Decisions

All stdlib. No external dependencies.

## Constitution Compliance

| Principle | Compliance |
|-----------|-----------|
| 1. Transparent Proxy | Read-only parsing, no message modification |
| 2. Zero-Configuration | Automatic, no config needed |
| 6. Cross-Platform | stdlib only |
