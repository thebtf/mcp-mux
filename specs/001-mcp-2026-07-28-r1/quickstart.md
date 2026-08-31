# R1 Customer Validation Guide

> This is a **future built-deliverable** validation guide. It is not evidence that the unimplemented plan currently works, and no command in this guide was run during the Plan stage. Run it from the exact implementation candidate after the public selector and customer fixture exist.

## What this proves

The guide proves the R1 customer contract rather than only helper behavior:

1. a known modern host can open a native same-era isolated route;
2. existing legacy behavior remains unchanged;
3. unsafe modern admission refuses before owner election;
4. modern traffic does not receive legacy bootstrap/cache/replay traffic;
5. loss/lifecycle boundaries quarantine rather than downgrade or replay; and
6. status tells an operator the truth without leaking private material.

See [`public-policy.md`](contracts/public-policy.md), [`daemon-control.md`](contracts/daemon-control.md), [`owner-status.md`](contracts/owner-status.md), [`lifecycle-quarantine.md`](contracts/lifecycle-quarantine.md), and [`data-model.md`](data-model.md) for the contract definitions.

## Scenario-to-success-criterion map

| Success criterion | Customer validation scenario |
| --- | --- |
| SC-001 | Scenario 1. Scenario 2 confirms the alternate host-sent discovery opener. |
| SC-002 | Scenario 3. |
| SC-003 | Scenario 7. |
| SC-004 | Scenario 5. |
| SC-005 | Scenarios 1, 2, and 4. |
| SC-006 | Scenario 6. |
| SC-007 | Scenarios 1 and 7 as the built-deliverable modern and legacy pair. |

Scenario 8 records the required rollback evidence. It does not add a separate success criterion.


## Prerequisites

- An exact built R1 candidate `mcp-mux` executable, not an older installed daemon or a different worktree build.
- A known MCP `2026-07-28` stdio upstream that accepts an ordinary native request and, for the byte-preservation scenario, a capture fixture that records each raw line received on upstream stdin.
- A known legacy upstream/reference fixture. The existing `testdata/mock_server.go` is the candidate’s adjacent legacy fixture; retain a before-R1 transcript for comparison.
- A clean temporary base directory so owner IPC and snapshot state cannot be inherited from another run.
- Windows or Unix tooling appropriate for the target. Run the lifecycle matrix on both platform families before release because IPC/process retirement is platform-sensitive.

The examples use the recommended explicit selector spelling `--mcp-protocol=2026-07-28`. If the final additive CLI API chooses a different spelling, substitute only that documented spelling; preserve the semantic policy and every expected outcome below.

## Test setup

PowerShell example from the candidate root after a build:

```powershell
$Mux = "$PWD/mcp-mux.exe"                 # Use ./mcp-mux on Unix
$BaseDir = Join-Path $env:TEMP "mcp-r1-$([guid]::NewGuid())"
New-Item -ItemType Directory -Path $BaseDir | Out-Null

# The current CLI derives owner/control paths from Go os.TempDir(). Isolate this
# invocation by setting the standard temporary-directory variables before launch.
$env:TEMP = $BaseDir
$env:TMP = $BaseDir
$env:TMPDIR = $BaseDir
$ModernPolicy = "--mcp-protocol=2026-07-28"

# Set these to the implementation-phase same-era upstream/capture fixture.
$ModernUpstream = "<known-modern-upstream>"
$ModernArgs = @("<upstream-args>")
$CaptureFile = Join-Path $BaseDir "modern-upstream.ndjson"

# Use a separate standard-temp directory for the preserved legacy transcript.
$LegacyBaseDir = Join-Path $env:TEMP "mcp-r1-legacy-$([guid]::NewGuid())"
New-Item -ItemType Directory -Path $LegacyBaseDir | Out-Null
```

Do not use an existing daemon process or a process started with a legacy-only binary. Before each isolated scenario, point `TEMP`, `TMP`, and `TMPDIR` at a freshly created directory; restore the shell values after the guide. Stop/clear only directories created for this guide after the run; do not remove unrelated mcp-mux state.

## Scenario 1 — Direct ordinary modern opener, no `clientInfo`

Send an ordinary valid request as the first message; do not call discovery first and intentionally omit optional `clientInfo`.

```powershell
$open = '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
$open | & $Mux $ModernPolicy $ModernUpstream @ModernArgs 2> (Join-Path $BaseDir "modern.stderr") |
  Tee-Object -FilePath (Join-Path $BaseDir "modern.stdout.ndjson")
```

**Expected outcome**:

- a native response for ID `1` arrives from the same-era upstream;
- one owner is created and direct R1 readback reports `protocol_era=2026-07-28`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine`; and
- the capture fixture’s first upstream stdin line is byte-for-byte `$open` (ignoring only its transport newline); and
- the capture contains no mux-generated `initialize`, `notifications/initialized`, `tools/list` before the supplied request, roots/list, list-change, cache/template response, or replay frame.

**SC mapping**: SC-001 and SC-005.

## Scenario 2 — Host-sent `server/discover` is forwarded, not manufactured

Use a new temporary base directory and make `server/discover` the actual host opener:

```powershell
$DiscoverBaseDir = Join-Path $env:TEMP "mcp-r1-discover-$([guid]::NewGuid())"
New-Item -ItemType Directory -Path $DiscoverBaseDir | Out-Null
$env:TEMP = $DiscoverBaseDir
$env:TMP = $DiscoverBaseDir
$env:TMPDIR = $DiscoverBaseDir
$discover = '{"jsonrpc":"2.0","id":2,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
$discover | & $Mux $ModernPolicy $ModernUpstream @ModernArgs |
  Tee-Object -FilePath (Join-Path $DiscoverBaseDir "discover.stdout.ndjson")
```

**Expected outcome**:

- the upstream receives exactly `$discover` as its first line;
- mcp-mux does not send discovery on its own before/after it;
- the host gets the upstream discovery result unchanged except for ordinary transport framing; and
- the owner is still forced isolated and cache/template/replay off.

**SC mapping**: SC-001 and SC-005. This also proves discovery is supported when sent, not required as a handshake.

## Scenario 3 — Strict modern admission and precise errors

For each input below, use a fresh base directory and a capture upstream. Confirm zero upstream process start/attach and zero legacy fallback. When a request ID is present, inspect the returned error frame.

| Case | Opening mutation | Expected result |
| --- | --- | --- |
| Missing `_meta` | Remove `params._meta` | `-32602` and no owner election. |
| Null `_meta` | `"_meta":null` | `-32602` and no owner election. |
| Non-object capabilities | Set `clientCapabilities` to a string/array/null | `-32602` and no owner election. |
| Missing version | Omit `protocolVersion` | `-32602` and no owner election. |
| Malformed optional identity | Include `clientInfo` without valid string `name`/`version` | `-32602` and no owner election. |
| Unsupported valid version | Set version to a different valid string | `-32022` with `data.supported` and `data.requested`; no fallback. |
| Contradictory opener | Explicit modern policy but legacy `initialize`/conflicting era claim | Explicit local refusal; do not assert `-32022` unless an unsupported declared version actually caused it. |
| Daemon echo mismatch | Run against an old/mismatched control implementation or fault fixture | Explicit local control-era failure before IPC attach; not automatically `-32022`. |

For every refusal, query the temporary daemon/status surface if available. It must not show a modern owner attached to a legacy owner, a newly started upstream, or a fallback legacy route.
**SC mapping**: SC-002.


## Scenario 4 — Native MRTR, directionality, and logging

Use a same-era fixture that can return `input_required`, emit a standard request-scoped log, and deliberately attempt a server-originated JSON-RPC request.

1. Send a valid `tools/call` with required metadata.
2. Confirm the fixture’s `input_required` result, `inputRequests`, and opaque `requestState` reach the host unchanged.
3. Have the host send its own new-ID retry with the required input and echoed opaque state. Confirm mcp-mux treats it as ordinary fresh traffic; it does not create a retry lineage or cache it.
4. Opt into a log level on a request if the fixture uses standard logging. Confirm one valid log reaches the sole route at most once; mcp-mux does not synthesize a log or transform stderr.
5. Have the fixture write a JSON-RPC request toward the host. Confirm it is contained: no downstream host frame, no last-active callback, no broadcast, and only an existing redacted diagnostic if the implementation provides one.

**SC mapping**: SC-005.

## Scenario 5 — Lifecycle quarantine matrix

Use the implementation-phase customer/lifecycle fixture to create an isolated modern owner with an identifiable current generation and existing ephemeral request, progress, and subscription state. For each boundary, observe the allowed R1 result and the prohibited result.

| Boundary exercise | Expected R1 result | Must never occur |
| --- | --- | --- |
| Snapshot export/restore | Modern owner excluded; a later explicit modern launch cold-starts or the operation fails closed. | Cache/token/route hydration or legacy restore. |
| Graceful live handoff | Modern owner is refused/omitted before era-less transfer; cold start/failure follows existing authority proof rules. | Modern owner attached through legacy handoff payload. |
| Reaper and zero-session cleanup | Existing activity/CAS/finalization gates apply; then modern owner drains/removes. | Implicit legacy successor or premature deletion before retirement proof. |
| Retry/respawn | Exact modern era and modern policy continue only if explicitly carried; otherwise cold start/fail closed. | Legacy bootstrap/cache/replay on the replacement. |
| Upstream/daemon loss | Sent in-flight request gets one terminal outcome; current routes clear. | Late old-generation response/cancel/progress/subscription delivery or replay. |
| Downstream reconnect | Exact-era fresh route or explicit new-launch failure. Host sends a new listen request. | `replayInit`, synthesized list change, old subscription route, legacy attachment. |
| Blocked finalization | Current owner remains authoritative/blocked until existing proof/retry resolves. | Competing owner or legacy fallback while retirement is unproven. |

For the loss/reconnect row, explicitly send a new `subscriptions/listen` after reconnection. Its acknowledgement must precede its notifications, and no notification from the former route may arrive.

**SC mapping**: SC-004.


## Scenario 6 — Minimal R1 readback and redaction

While a modern owner is active, inspect the direct R1 readbacks that carry `OwnerInfo`:

```powershell
& $Mux status
# Also inspect the product's direct daemon control status/list.
# Inspect mux_list only when it serializes OwnerInfo.
```

Limit R1 proof to the direct projections listed above.

**Expected common facts**:

```text
protocol_era      = 2026-07-28
sharing_policy    = forced-isolated
cache_policy      = off
lifecycle_policy  = r1-quarantine
```

If a direct projection already has a readiness field, confirm that it retains its existing meaning. Do not require a new logging field, lifecycle-state word, or counter. Search the direct outputs and error logs for a deliberately injected sentinel representing a request payload, opaque state, credential-like string, token-like string, environment value, progress token, subscription ID, and compatibility key. None may appear. Do not use a real secret for this check.

**SC mapping**: SC-006.

## Scenario 7 — Released legacy parity

Use `$LegacyBaseDir`, omit `$ModernPolicy`, and run the recorded legacy reference fixture before and after the R1 candidate:

```powershell
$env:TEMP = $LegacyBaseDir
$env:TMP = $LegacyBaseDir
$env:TMPDIR = $LegacyBaseDir
# Replace arguments only with the existing legacy fixture invocation used by the release baseline.
Get-Content -Raw -Path (Join-Path $LegacyBaseDir "legacy-input.ndjson") |
  & $Mux "go" "run" "./testdata/mock_server.go" |
  Tee-Object -FilePath (Join-Path $LegacyBaseDir "legacy.stdout.ndjson")
```

Compare the before/after outputs and owner identity bytes using the project’s existing legacy parity fixture. **Expected outcome**: identical legacy bootstrap, request sequence, result sequence, cache/replay behavior, and identity; R1 admission failures do not alter the legacy route.

**SC mapping**: SC-003 and, together with Scenario 1, SC-007.


## Scenario 8 — Operator rollback

1. Stop new invocations that select the explicit modern policy.
2. Read status and identify only modern forced-isolated owners.
3. Drain/remove them through the existing safe owner lifecycle command/path.
4. Confirm the direct R1 readbacks retain the four policy facts while an owner remains active, then use existing completion/readiness evidence to confirm its removal where available. Do not require a new lifecycle-state word or counter.
5. Retain the customer transcript and status/redaction evidence with the exact candidate SHA.

Rollback is complete only when no new modern admission is possible and no live modern work has been converted to legacy or replayed.

## Release evidence record

Record the exact binary/source SHA, operating system, selected public policy spelling, upstream fixture revision, captured opening bytes, control echo, status snapshots, all scenario outcomes, and the independent checker identity. The record must clearly distinguish the successful modern proof, legacy parity proof, and any allowed lifecycle cold-start/refusal from a failure.
