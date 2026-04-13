# Implementation Plan: Synthetic Progress Reporter

**Spec:** .agent/specs/synthetic-progress-reporter/spec.md
**Created:** 2026-04-13
**Status:** Draft

> **Provenance:** Planned by claude-opus-4-6 on 2026-04-13.
> Evidence from: spec.md, owner.go (inflight tracker + progressOwners), classify.go
> (ParseIdleTimeout/ParseToolTimeout patterns), research report.
> Key decisions by: AI agent (architecture follows existing codebase patterns).
> Confidence: VERIFIED — all referenced code structures confirmed via grep.

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.23+ | Project language |
| Timer | `time.Ticker` | Standard library, fits periodic tick pattern |
| JSON | `encoding/json` | Already used throughout owner.go for wire messages |
| Concurrency | goroutine + `sync.Map` range | Matches existing inflightTracker scan pattern |
| Capability parsing | `internal/classify` | Follows ParseIdleTimeout/ParseToolTimeout pattern |

No external libraries needed. All infrastructure exists in the codebase.

## Architecture

```
Owner.Serve()
  ├── acceptLoop (existing)
  ├── upstream reader (existing)
  └── progressReporter (NEW) ← goroutine, stopped via ctx cancel
        │
        every N seconds:
        ├── scan inflightTracker (sync.Map.Range)
        ├── for each entry older than threshold:
        │   ├── lookup tokens via requestToTokens
        │   ├── check lastRealProgress[token] timestamp
        │   ├── if silent → build synthetic notification JSON
        │   └── send via session.WriteRaw()
        └── no inflight entries → no-op
```

**REVERSIBLE** — new goroutine in Owner, no data model changes, no API changes. Removing the goroutine restores previous behavior.

## Data Model

### New Fields on Owner

| Field | Type | Purpose |
|-------|------|---------|
| `progressInterval` | `time.Duration` | Tick interval (default 5s, from x-mux.progressInterval) |
| `lastRealProgress` | `map[string]time.Time` | Token → last real upstream progress timestamp |

`lastRealProgress` is protected by existing `progressMu` (same lock as `progressOwners`).
`progressInterval` is set once during init, read-only after — no lock needed.

**REVERSIBLE** — two new fields, zero schema/protocol changes.

### Synthetic Notification Wire Format

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/progress",
  "params": {
    "progressToken": "<from requestToTokens>",
    "progress": 15,
    "message": "tavily_search: 15s elapsed"
  }
}
```

No `total` field (indeterminate). `progress` = seconds elapsed, rounded to interval.

**REVERSIBLE** — standard MCP notification, no new protocol.

## File Structure

```
internal/
  mux/
    progress_reporter.go      <- NEW: reporter goroutine + tick logic
    progress_reporter_test.go  <- NEW: unit tests
  classify/
    classify.go               <- EDIT: add ParseProgressInterval()
    progress_interval_test.go  <- NEW: tests for ParseProgressInterval
```

**4 files total: 2 new, 1 edit, 1 new test.**

## API Contracts

### ParseProgressInterval (internal/classify)

```go
// ParseProgressInterval extracts x-mux.progressInterval from a cached
// initialize response. Returns interval in seconds (range 1-60), or 0 if
// not declared. Follows same pattern as ParseIdleTimeout/ParseToolTimeout.
func ParseProgressInterval(initJSON []byte) int
```

**REVERSIBLE** — new function, no callers modified.

### progressReporter (internal/mux)

```go
// runProgressReporter scans inflightTracker every interval and sends
// synthetic notifications/progress for long-running requests without
// recent real progress. Runs as goroutine, cancelled via ctx.
func (o *Owner) runProgressReporter(ctx context.Context)

// recordRealProgress marks a token as having received real upstream progress.
// Called from routeProgressNotification when a real notification arrives.
func (o *Owner) recordRealProgress(token string)

// buildSyntheticProgress builds the JSON wire bytes for a synthetic
// notifications/progress message.
func buildSyntheticProgress(token string, toolOrMethod string, elapsedSeconds int) []byte
```

**REVERSIBLE** — new methods, integrated via single call in Serve().

## Phases

### Phase 1: Core Reporter + Capability Parsing
**Goal:** Reporter goroutine emits synthetic progress, configurable via capability.

- `ParseProgressInterval()` in classify.go (follows ParseIdleTimeout pattern)
- `progress_reporter.go`: `runProgressReporter`, `buildSyntheticProgress`
- Wire into `Owner.Serve()`: start goroutine with ctx cancel
- Set `progressInterval` from cached initResp after classification
- Tests: ParseProgressInterval (6 cases), buildSyntheticProgress (3 cases)

### Phase 2: Dedup + Real Progress Tracking
**Goal:** Suppress synthetic when upstream sends real progress.

- Add `lastRealProgress` map to Owner
- `recordRealProgress()` called from `routeProgressNotification()`
- Reporter checks `lastRealProgress[token]` before emitting
- Clean up `lastRealProgress` entries in `clearProgressTokensForRequest()`
- Tests: dedup logic (3 cases), cleanup on request completion (1 case)

### Phase 3: Integration + Edge Cases
**Goal:** End-to-end verification with full test suite.

- Integration: reporter tick with real inflight data (mock session + WriteRaw)
- Edge case: request completes mid-tick (inflightTracker already deleted)
- Edge case: multiple tokens per request
- Edge case: zero inflight requests (no-op tick)
- Full regression: `go test ./... -count 1`
- Build + deploy verification

## Library Decisions

| Component | Library | Version | Rationale |
|-----------|---------|---------|-----------|
| Timer | `time.Ticker` (stdlib) | — | Standard, no external dependency |
| JSON | `encoding/json` (stdlib) | — | Already used for all wire protocol |
| Sync | `sync.Map` (stdlib) | — | Already used for inflightTracker |

No external libraries. Everything uses existing stdlib + project patterns.

## Unknowns and Risks

| Unknown | Impact | Resolution Strategy |
|---------|--------|-------------------|
| CC dedup of multiple tokens for same toolUseId | LOW | Test with real CC session — if CC shows duplicates, emit on first token only. Can be patched post-ship. |
| Lock contention on progressMu during tick | LOW | Tick holds lock briefly (<1ms per scan). Monitor via benchmark if needed. |

## Integration Points

### Owner.Serve() (owner.go ~line 1428)
Add `go o.runProgressReporter(ctx)` alongside existing goroutines.

### routeProgressNotification() (owner.go ~line 1006)
After successful routing of real upstream progress, call `o.recordRealProgress(token)`.

### clearProgressTokensForRequest() (owner.go ~line 1068)
Also clean up `lastRealProgress` entries for cleared tokens.

### cacheResponse "initialize" path (owner.go)
After caching initResp, parse progressInterval:
```go
if sec := classify.ParseProgressInterval(raw); sec > 0 {
    o.progressInterval = time.Duration(sec) * time.Second
}
```
