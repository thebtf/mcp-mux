# Technical Debt

All items from the 2026-04-08 debt batch have been resolved:

- ✅ **Progress notifications forwarding** — implemented earlier via
  `Owner.progressOwners`, `trackProgressToken`, and `routeProgressNotification`
  in `internal/mux/owner.go`. `notifications/progress` from upstream is now
  routed to the originating session using the `_meta.progressToken` mapping,
  with broadcast fallback if routing fails.

- ✅ **Tool call timeout** — implemented in v0.10.1 as `x-mux.toolTimeout`
  capability. Upstream servers declare their acceptable tool-call wait time
  (in seconds); mux starts a watchdog goroutine per `tools/call` request.
  If upstream doesn't respond within the window, the watchdog synthesizes
  a JSON-RPC error response (`code -32000`) and delivers it to the session.
  Uses atomic `sync.Map.LoadAndDelete` to race cleanly with the natural
  response path — whichever wins the claim delivers the response.

- ✅ **Inflight progress injection (research)** — moved to engram as a
  research task. The debt entry was explicitly marked "if feasible" and
  requires CC-side investigation of MCP progress-display support. Not
  suitable for implementation without upstream protocol research.

---

## In progress

(none)

## Open items

### 2026-04-18: FLAKE-UBUNTU-CI-TestOwnerMultipleSessions

**What:** `TestOwnerMultipleSessions` in `muxcore/owner/mux_test.go` times out at 30s consistently on ubuntu-latest in CI. Passes locally on Windows in 0.4s and on macOS CI every run.

**Why deferred:** Cannot reproduce locally on Windows; fixing requires investigation on a Linux environment. Not tied to any recent change — the flake was visible on PR #65/#66/#67 master-push CI (same test, same 30s timeout). Does not affect production correctness; it only blocks green-CI-on-push.

**Impact:** Every merge-to-master CI run is red on ubuntu-latest. Masks any real Ubuntu-specific regression that might land next. Accumulates noise in the run history.

**Context:** Test uses `go run ../../testdata/mock_server.go` which spawns a subprocess. Suspect: subprocess cold-start timing on Linux CI runners under concurrent test load + the PR #67 drain-goroutine changes interacting with `go run` compile cache timing. First concrete failure: CI run 24592642184 (v0.19.4-era). Most recent: 24606721806 (PR #68).

**Suggested fix path:** (1) Reproduce in a Linux Docker container locally, (2) add explicit sync in the test to wait for upstream readiness before sending requests, (3) or split the test into Windows/Linux variants with longer timeout on Linux.


<!--
Resolved items are not tracked in this file; see git history + GH releases for audit trail.
- `upstream.Start` Wait-vs-ReadLine race → PR #67 (squash 30d6314), shipped in muxcore/v0.20.1 + v0.9.7.
-->

