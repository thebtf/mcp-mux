# Prove live upstream executable replacement

This guide is an implementation/release oracle. No commands below were run in the SpecKit assignment. `scripts/smoke-upstream-maintenance.ps1` is a planned deliverable in tasks.md, not an existing runnable script at the source base. Complete its implementation before running the commands.

## Prerequisites

Use the exact integrated candidate after core and adapters land, Go 1.25.12 for the current production-playbook gate, PowerShell 7 on Windows and Unix, and the existing critical-suite launcher prerequisite `MCP_LAUNCHER` or `-Launcher`. Use a dedicated current-user test namespace and private config/IPC storage. Scratch and evidence belong under the primary checkout's `.agent/tmp`, never a linked checkout's inner `.agent`, `.t`, or OS temp. Root owns allocation, exact process cleanup, and acceptance.

The smoke must reuse `scripts/lifecycle-smoke-upstream/main.go` and `testdata/mock_modern_server.go`. Extend only the lifecycle fixture with a link-time version string, opt-in shared mode while retaining its current isolated default, a bounded in-flight operation, and request/frame capture needed to prove no replay. No new supervisor or test framework. The runner is cross-platform pwsh with platform-specific exact PID/tree observations; it must not retain the Windows-only restriction in `smoke-process-lifecycle.ps1`.

## Run the actual candidate and replacement proof

From the integrated candidate, resolve the primary checkout through Git's common directory and use the root-owned scratch resource returned by the workspace allocator. The examples below show the layout; a run uses its exact allocated path rather than reusing another run's directory.

```powershell
$PrimaryRoot = Split-Path -Parent (git rev-parse --path-format=absolute --git-common-dir)
$ScratchRoot = Join-Path $PrimaryRoot '.agent/tmp/maintenance-proof'
$BuildDir = Join-Path $ScratchRoot 'build'
New-Item -ItemType Directory -Force $BuildDir | Out-Null
# Windows build. On Unix use the same command with output filename mcp-mux.
$CandidateBinary = Join-Path $BuildDir 'mcp-mux.exe'
go build -o $CandidateBinary ./cmd/mcp-mux
if ($LASTEXITCODE -ne 0) { throw 'candidate build failed' }
$OutputDir = Join-Path $ScratchRoot 'windows'
$EvidencePath = Join-Path $OutputDir 'summary.json'
pwsh -NoProfile -File scripts/smoke-upstream-maintenance.ps1 -SourceRoot . -CandidateBinary $CandidateBinary -ScratchRoot $ScratchRoot -OutputDir $OutputDir -EvidencePath $EvidencePath -TimeoutSeconds 180
if ($LASTEXITCODE -ne 0) { throw 'maintenance proof failed' }
```

On Unix, build the Unix binary from the same exact source head and select a fresh `unix` OutputDir beneath its allocated primary ScratchRoot. Do not reuse the Windows executable.

The runner's canonical parameters are `SourceRoot`, `CandidateBinary`, `ScratchRoot`, `OutputDir`, `EvidencePath`, and `TimeoutSeconds`. ScratchRoot must be root-owned primary `.agent` storage; OutputDir must be fresh and contained beneath it. The runner builds two distinguishable fixture versions, runs actual CLI hold/resume/renew commands, and exits zero only when the applicable scenarios pass. It records source SHA, binary/fixture hashes, unchanged host pipes, scoped PID/tree observations, control responses, timestamped frames, file hashes, platform and exit statuses. Missing proof is a failure, not a skipped success.

### Required live sequence

1. Start two aware legacy hosts through the candidate wrapper against one opt-in shared v1 lifecycle fixture. Keep the original host input/output pipes open throughout. Verify one managed leader plus descendant, initialized hosts, and distinct numeric/string demand.
2. Start one bounded in-flight request. Obtain the exact server ID from local daemon status/list. Run `mcp-mux hold <id> --ttl 30s --drain-timeout 2s --json`; the runner substitutes the observed ID, not a substring or PID. Observe the in-flight request's allowed completion before the one drain deadline, or its terminal retirement error afterward. New held requests must never reach the upstream or receive cached success.
3. Require HELD, TreesRetired=true, future expiry, and zero selected leader/descendant survivors. Confirm no process-start gap around acquisition. Copy v2 bytes over the exact path formerly executed by v1 while the hold stays active. Require changed SHA-256 and successful write. Do not use rename-only replacement as proof of dead Unix processes; check actual tree death and overwrite the executed path.
4. Send numeric and string IDs, a notification, and controlled reconnect/queued traffic while held. Require exactly one -32005 maintenance error for each request ID, no invented notification response, no replay, and no scoped starts. Exercise a queue/reconnect transition race with focused tests, not a timing-only live claim.
5. Run `renew <observed-hold-id> --ttl 30s --json` and verify expiry changes from renewal acceptance. Attempt a competing acquisition and a stale lease operation; require typed conflict/not-found without mutation. Attempt controlled restart/handoff/shutdown through CLI and library update paths; require terminal refusal with no fallback or successor.
6. Run `resume <observed-hold-id> --json`. Fresh requests on the same two host pipes must report v2 from exactly one fresh shared generation. No held or terminated request may appear in the fixture capture. Repeat with a short proven-held TTL and no resume to prove safe expiry.
7. Use a dedicated aware daemon-loss scenario to prove durable HELD is loaded before fresh admission. Incomplete/corrupt authority and blocked retirement are proved by deterministic focused regressions; never corrupt an operator ledger. Capture that blocked state does not expire into a start.
8. Run one known native modern host through `--mcp-protocol=2026-07-28` and the existing same-era fixture. Hold its exact isolated target, require original-ID maintenance errors and no injected legacy bootstrap/cache/replay, then require fresh exact-era admission or the documented explicit new-launch-required result. Do not interpret that refusal as transparent restoration.
9. Exercise the control endpoint with no optional maintenance handler and require unsupported with no stop/kill/exec fallback. Separately document that old shims under an aware daemon get only physical start fencing, not immediate-error/non-replay guarantees. Arbitrary old binaries are not an oracle the feature can control.
10. Compare safe readbacks against private fixture credentials and frame content without printing their values. Require zero disclosure and unchanged unrelated-context operation. Cleanup only exact fixture-owned processes/resources through existing lifecycle authority, preserving failed evidence and blocked leases.

## Focused regression commands

Planned maintenance tests use the `TestMaintenance` prefix. Run only after those tests exist; an empty matching denominator is not proof.

```powershell
Push-Location muxcore
go test ./control ./daemon ./owner ./engine ./upstream -run '^TestMaintenance' -count=1
go test -race ./control ./daemon ./owner ./engine ./upstream -run '^TestMaintenance' -count=1
Pop-Location
go test ./cmd/mcp-mux ./internal/mcpserver -run '^TestMaintenance' -count=1
```

Capture named cases and denominator, including failing-before/passing-after respawn suppression, start-through-install race, placeholder settlement, actual drain, all-tree death versus handoff, finite CWD/era/security/namespace context sets, stale generations/leases, durable commit failures, startup order, renewal/expiry/blocked retirement, terminal launcher/update refusal, unsupported standalone paths, and held queue/reconnect disposition.

## Scenario-to-success-criterion map

| Criterion | Required proof |
| --- | --- |
| SC-001 | Live steps 1-3 on Windows and Unix: two hosts, descendant, no scoped survivors/starts, actual executable overwrite. |
| SC-002 | Step 4 plus deterministic queue/reconnect races: exactly-one original numeric/string ID errors, zero upstream/replay capture. |
| SC-003 | Step 6 explicit resume and safe expiry: same host pipes, one new generation, v2 response. |
| SC-004 | Step 5 renewal/conflict and deterministic stale/expiry/blocked-retirement tests. |
| SC-005 | Steps 5/7 plus exact-context/start/install/persistence/lifecycle races; no held starts or fallback. |
| SC-006 | Steps 8/9 native modern and incapable control endpoint; explicit old-shim/old-binary limits. |
| SC-007 | Step 10 redaction and unrelated-target continuity. |

## Root integration and release gates

Root runs the full integrated suites once per admitted exact head, after all maker changes land. These commands supplement the live replacement proof, not substitute for it.

```powershell
go test ./... -count=1
go vet ./...
Push-Location muxcore
go test ./... -count=1
go vet ./...
go test -race ./control ./daemon ./owner ./engine ./upstream -count=1
Pop-Location
pwsh -NoProfile -File tests/critical/run-all.ps1 -TimeoutSeconds 120
pwsh -NoProfile -File scripts/smoke-native-sessionhandler-update.ps1 -RunDir (Join-Path $ScratchRoot 'native-update') -EvidencePath (Join-Path $ScratchRoot 'native-update.json') -TimeoutSeconds 120
```

The last command is production Scenario 5b. Run applicable Scenario 8 lifecycle/tree tests from `docs/PRODUCTION-TESTING-PLAYBOOK.md`, including Windows and Unix process-group/Job tests. Retain R1 native parity proof using the existing runners:

```powershell
pwsh -NoProfile -File scripts/verify-r1-native-isolation.ps1 -SourceRoot . -OutputDir (Join-Path $ScratchRoot 'r1-windows')
```

```sh
bash scripts/verify-r1-native-isolation.sh --source-root . --output-dir "$SCRATCH_ROOT/r1-unix"
```

On Unix, set `SCRATCH_ROOT` to that host's exact allocated primary scratch path. The critical runner already resolves Git's common directory to primary `.agent`; pass its supported `-ArtifactRoot` explicitly when selecting another root-owned evidence directory. Controlled activation additionally uses the existing namespace file lock and the shared read-only ledger check from ADR-015's implementation amendment; a status preflight by itself is not race proof.

Follow `docs/RELEASE-PROTOCOL.md` after exact-head review, root release authorization, and accepted version selection. Bump binary/library versions, update CHANGELOG/RELEASE_NOTES and consumer docs, merge exact SHA, then tag last. Verify remote tag parity and module resolution using the actual root-selected `$ReleaseVersion`:

```powershell
git ls-remote --tags origin
go list -m -json "github.com/thebtf/mcp-mux/muxcore@$ReleaseVersion"
```

Root owns publishing and fresh-clone binary/module canary, same-host hold/replace/resume proof from the delivered artifact, and fresh-session consumer proof. Prepare and deliver current-version handoffs to aimux, engram, and other affected consumers, read them back, and record `PROJECT_RELEASE_PROTOCOL_PASS` and `CONSUMER_HANDOFF_PASS` or the exact blocked boundary. No external tracker/publication effects are delegated by this guide.

## Rollback

Use the hold-aware current binary to inspect and resume every proven retired lease before controlled rollback. Preserve blocked/incomplete records and report the exact unsupported recovery boundary; do not delete authority to manufacture success. Active-lease transfer, hold-unaware downgrade, foreign-engine control, manual active-pointer replacement, and arbitrary old-binary starts are unsupported.
