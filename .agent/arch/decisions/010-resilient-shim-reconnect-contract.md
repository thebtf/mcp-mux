# ADR 010 -- Resilient Shim Reconnect Contract

Status: Proposed

Date: 2026-05-19

## Context

Global goal: keep the current `consumer -> shim -> daemon -> real MCP`
topology if it can preserve a client-visible MCP session across daemon or owner
restart.

Global claim: the stdio shim must keep the consumer connection alive across a
daemon restart and must provide a valid JSON-RPC outcome for requests around the
restart boundary.

Why this decision is needed: the standalone current-topology PoC proves that a
minimal topology can survive same-stdio restart, initialize replay, one
interrupted request, one stdin-buffered next request, concurrent out-of-order
responses, and refresh-token reconnect. Production
`muxcore/owner/resilient_client.go` already has a richer state machine, but its
contract intentionally differs for already-sent in-flight requests.

System boundary under test: `RunResilientClient` and its reconnect path:
stdin reader, IPC writer/reader, token refresh or fallback spawn, initialize
replay, buffered stdin flush, and in-flight request handling.

## Decision

We will keep the current topology and harden the resilient shim contract in
production instead of replacing the architecture.

We will define two restart-boundary classes:

| Class | Contract |
| --- | --- |
| Buffered but not yet written to owner IPC | Preserve in `msgFromCC` and flush to the successor owner after reconnect. |
| Already written to owner IPC and still in-flight when IPC dies | Return a JSON-RPC error response by original ID unless a future explicit retry policy proves the method is safe to replay. |

We will not blindly retry every already-sent in-flight request by default. A
request may have reached the old upstream and produced side effects even if the
shim never received the response. Default retry would trade transport reliability
for duplicate execution risk.

## Proof Evidence

| Phase | Verdict | Evidence | Production implication |
| --- | --- | --- | --- |
| Phase 4 | PASS | `scripts/run-current-topology-poc.ps1 -WatchSeconds 1`; same stdio shim returned `break_observed:false` after daemon generation changed | Reconnect, initialize replay, and same-stdio continuity are viable topology invariants. |
| Phase 5 | PASS | Same runner; slow request plus stdin-buffered next request both returned from successor generation | The topology can preserve a serial reconnect window, but the PoC does not model non-idempotent upstream side effects. |
| Phase 6 | PASS | Same runner; `concurrent_demux` returned `active_calls:2`, `max_concurrent_calls:2`, `response_order:[4,3]`, both responses from successor generation | The experimental topology can support concurrent owner dispatch and out-of-order response demux by JSON-RPC ID across restart. |
| Phase 7 | PASS | Same runner; `refresh_reconnect` returned `refresh_used:true`, `fallback_spawn_used:false`, `refresh_successes:1`, `fallback_spawns:0`, and a different refreshed token | A restored owner can be rejoined through consumed-token history without falling back to fresh spawn. |

## First Break

Phase: Phase 4.

Observed failure: the simple shim exited when the owner connection closed.

Confirmed guard: successor daemon PID/generation changed before the
post-restart call.

Failing boundary: shim state machine, not the abstract
consumer-shim-daemon topology.

Minimal no-break mechanism: cache initialize, reconnect through daemon spawn,
replay initialize, and resume the same stdio process.

## Production Projection

| Proof mechanism | Production owner | Current production gap | Port action | Verification |
| --- | --- | --- | --- | --- |
| Same-stdio reconnect | `RunResilientClient`, `runProxy`, `reconnect` | Covered in production behavior but needs restart-boundary parity test tied to daemon generation or owner replacement. | Add a production test that closes old owner IPC, reconnects to a successor, and proves the shim process remains alive. | From `muxcore`: `go test ./owner -run TestResilientClient_.*Reconnect -count=1` plus a new generation-aware test. |
| Initialize replay | `runStdinReader`, `finishReconnect`, `replayInit` | Existing tests cover replay ordering, but not against the PoC Phase 4/5 acceptance shape. | Keep replay behavior; bind it into the parity test. | Verify successor receives replayed `initialize` before post-restart traffic. |
| Stdin-buffered next request survives reconnect | `msgFromCC`, `flushBuffer` | Existing buffer test verifies forwarding, but `flushBuffer` writes directly and does not currently re-register flushed requests in `inflight`. | Track request IDs for flushed requests with the same semantics as `runIPCWriter`, or route flush through one shared writer helper. | New test: buffered request flushed to successor remains tracked until successor response, and is drained on a second disconnect. |
| Already-sent in-flight request | `inflight`, `drainOrphanedInflight` | Production intentionally returns JSON-RPC error instead of retrying. This is safer than the PoC retry behavior for side-effecting calls. | Preserve default error response behavior; document as the production contract. Optional future policy can add method-scoped safe retry. | Test that a request already written to old IPC receives one JSON-RPC error by ID and does not get replayed to the successor by default. |
| Token continuity | `RefreshToken`, `Reconnect`, `finishReconnect`, session token history | PoC proves the invariant with consumed-token history, but production parity still needs integration evidence across daemon restart and restored owners. | Compare production refresh-token history and fallback counters against Phase 7; keep fallback spawn as fallback-only, not the normal reconnect path. | Production test: consumed original token is refreshed to a different token, reconnect succeeds, initialize is replayed, fallback spawn counter stays zero. |
| Concurrent response demux | owner dispatch loop, resilient client IPC reader/writer | PoC proves the invariant; production parity still needs explicit tests before changing owner concurrency. | Compare production dispatch/response tracking to Phase 6 before porting concurrency behavior. | New production test with two outstanding requests, out-of-order responses, and restart boundary. |

## Consequences

Positive: the architecture remains viable, and production can harden the exact
failure boundary without collapsing into a one-binary or no-daemon redesign.

Negative: a client may receive an error for an already-sent request during
restart even when a toy PoC could retry it successfully.

Neutral: a future safe-retry policy remains possible, but it must be explicit
and method-aware.

## Risks and Open Questions

| Risk | Evidence gap | Mitigation |
| --- | --- | --- |
| Buffered requests flushed after reconnect may become untracked | `flushBuffer` bypasses `runIPCWriter` request-ID tracking | Refactor request forwarding into a shared helper and add second-disconnect coverage. |
| PoC stronger success behavior may be misread as production retry mandate | Phase 5 does not model side effects | Keep this ADR as the contract and encode it in tests. |
| Production true concurrent/out-of-order dispatch remains unproven | Phase 6 is proven only in the standalone PoC | Add production parity tests before changing owner dispatch concurrency. |
| Production refresh-token reconnect parity remains unproven | Phase 7 is proven only in the standalone PoC | Add production parity test with refresh success and fallback-spawn absence before relying on runtime smoke alone. |
| Runtime startup flakiness may also involve daemon/socket/process lifecycle outside `resilient_client.go` | This ADR focuses on shim reconnect contract | Use production parity tests plus fresh-session attach smoke after code changes. |

## Verification Plan

Commands:

```powershell
Push-Location muxcore
go test ./owner -run "TestResilientClient|TestReconnect" -count=1
go test ./... -count=1
Pop-Location
go test ./... -count=1
.\scripts\run-current-topology-poc.ps1 -WatchSeconds 1
```

Runtime smoke: after production changes, run a controlled local
`mcp-mux upgrade --restart` or equivalent test harness and verify a fresh
Claude/Codex MCP session can attach through the shim without startup handshake
failures.

Regression tests:

- same-stdio reconnect remains alive;
- initialize replay happens before post-restart traffic;
- buffered-but-unsent request is flushed and tracked;
- already-sent in-flight request receives one JSON-RPC error by ID and is not
  replayed by default;
- concurrent out-of-order responses are demuxed by JSON-RPC ID;
- refresh-token reconnect uses a different token and avoids fallback spawn when
  the restored owner is alive;
- no `mux-reconnect` synthetic progress notification is emitted.

## Reversibility

Rollback path: keep current `drainOrphanedInflight` behavior and remove only
new tests/refactors if the shared-forwarding refactor introduces regression.

Compatibility notes: default behavior remains compatible with existing
consumers because already-sent in-flight requests already receive JSON-RPC
errors today; the planned hardening mainly adds coverage and fixes tracking for
flushed buffered requests.
