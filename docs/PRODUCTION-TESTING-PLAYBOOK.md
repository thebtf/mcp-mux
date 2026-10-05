# Production Testing Playbook

Product: `mcp-mux`

This playbook is the customer-mode release walkthrough. Use it before every
release after code tests pass. The operator should act as a user of the CLI:
build an isolated candidate binary, run documented commands, and judge only
observable behavior.

## Scope

This playbook covers the local CLI/runtime surfaces that users depend on:

- `mcp-mux` shim startup against a real stdio MCP upstream.
- Daemon/owner reconnect after `stop --force`.
- Current-topology lifecycle behaviors proven through the local PoC oracle.
- Release deployment to the workstation binary through the documented upgrade
  path, when deployment is part of the release contract.

This is the `mcp-mux` product playbook. Muxcore library consumers need their
own customer-mode playbooks that exercise the consumer's public MCP/CLI entrypoint,
health/status surface, and chosen update topology.

## Prerequisites

- PowerShell 7+ (`pwsh`) from the repository root. For Scenario 5b, run from a
  normal operator shell or an explicitly approved unsandboxed agent command;
  sandboxed agent hosts can produce a host-specific Windows named-pipe
  `Access is denied` false failure. The script self-reexecs under `pwsh` when
  launched from Windows PowerShell 5.1.
- Go 1.25.12 matching CI (`go version` should report Go 1.25.12).
- `uvx` available for `mcp-server-time`.
- `D:\Dev\mcp-launcher\mcp-launcher.exe` available for the topology PoC.
- Do not run smoke tests directly against the production binary path
  `D:\Dev\mcp-mux\mcp-mux.exe`; the smoke script refuses this by design.

## Scenario 1: Isolated Build

Objective: prove a user can build a candidate binary without touching the
production binary.

Commands:

```powershell
New-Item -ItemType Directory -Force .agent\tmp\playbook | Out-Null
go build -trimpath -o .agent\tmp\playbook\mcp-mux.exe .\cmd\mcp-mux
Get-FileHash .agent\tmp\playbook\mcp-mux.exe -Algorithm SHA256
```

Expected:

- `go build` exits 0.
- The hash command prints a SHA256 for `.agent\tmp\playbook\mcp-mux.exe`.
- No write occurs to `D:\Dev\mcp-mux\mcp-mux.exe`.

## Scenario 2: Real Time Upstream Reconnect

Objective: prove a real MCP upstream remains usable after the shim reconnects
through `stop --force`.

Command:

```powershell
.\scripts\smoke-time-upstream.ps1 `
  -Binary .agent\tmp\playbook\mcp-mux.exe `
  -EvidencePath .agent\reports\playbook-smoke-time-upstream.json
```

Expected:

- The JSON verdict is `PASS`.
- `initialize.ok` is `true`.
- `tools_list.tool_names` includes `get_current_time` and `convert_time`.
- `reconnect_probe.post_reconnect_tools_list.ok` is `true`.
- `failure_string_present` is `false`.

Broken signals:

- `connection closed: initialize response`.
- `upstream restarted, request lost during reconnect`.
- A post-reconnect `tools/list` response for id 3 is missing or is an error.

## Scenario 3: Current Topology Oracle

Objective: prove the hardened topology behaviors still match the proofing
oracle after release changes.

Command:

```powershell
.\scripts\run-current-topology-poc.ps1 -WatchSeconds 1
```

Expected:

- The script exits 0.
- It reaches the final PASS line for the current-topology PoC.
- No daemon/process remains wedged after the script's cleanup steps.

## Scenario 4: Local Deployment Upgrade

Run this scenario only when the release contract includes deploying to this
workstation.

This scenario is specific to the `mcp-mux` product updater. Native muxcore
consumers should not copy the `mcp-mux.exe~` / `mcp-mux.versions` layout unless
their product deliberately chose the same topology.

Commands:

```powershell
go build -trimpath -o .\mcp-mux.exe~ .\cmd\mcp-mux
.\mcp-mux.exe upgrade --restart
Get-Content .\mcp-mux.versions\active.txt
.\mcp-mux.exe status
```

Expected:

- The stable launcher `mcp-mux.exe` remains in place; upgrade does not attempt
  to rename the configured binary while live shim/daemon processes may hold it.
- The pending binary `mcp-mux.exe~` is installed under
  `mcp-mux.versions/<hash>/mcp-mux-engine.exe`, and
  `mcp-mux.versions/active.txt` points at that engine.
- If live sessions are attached, `upgrade --restart` reports daemon restart
  deferred and does not send `graceful-restart`; existing MCP host stdio
  transports remain open.
- If no live sessions are attached, `upgrade --restart` may restart the daemon
  and `status` responds without handshake failure.
- `--force-daemon-restart` is maintenance-only. Do not use it as the release
  smoke for transparent updates with live sessions.

Broken signals:

- The build cannot write `mcp-mux.exe~`.
- `upgrade --restart` reports `rename current to old: Access is denied`.
- `upgrade --restart` sends `graceful-restart` while status shows live
  `session_count > 0`.
- `upgrade --restart` reports socket handoff failure without fallback recovery
  when no live sessions were attached.
- `status` cannot contact the daemon after restart.

## Scenario 5: Global-First Owner Dedup Across Cwds (v0.25.0)

Objective: prove that two host sessions wrapping the same MCP server command
from different working directories share ONE upstream owner (not one per cwd)
after `mcp-mux upgrade --restart` to v0.25.0.

Setup:

- v0.25.0 binary installed and `mcp-mux status` responds (covered by Scenario 4).
- A shareable MCP server in `mcp-mux` host config (the test uses `time`
  server: `uvx mcp-server-time` — known shared-classifiable by tools/list).

Commands (run as a user, not a developer):

1. Open **two** new shells, each in a **different cwd**, and pretend each
   is a different host session. Example layout:

   ```powershell
   # Shell A
   cd $env:USERPROFILE

   # Shell B
   cd D:\Dev\mcp-mux
   ```

2. Trigger each shell's host to actually USE the time MCP server at least
   once (so the shim spawns the upstream and classification completes).
   Easiest: in each host session, ask the agent to run a `get_current_time`
   tool call.

3. Open a **third** shell — a regular PowerShell window, NOT a host session —
   for inspection. From that third shell:

   ```powershell
   .\mcp-mux.exe status |
     ConvertFrom-Json |
     Select-Object -ExpandProperty servers |
     Where-Object { $_.command -eq 'uvx' -and ($_.args -join ' ') -match 'mcp-server-time' } |
     Format-Table server_id, cwd, cwd_set, auto_classification, session_count, persistent -AutoSize
   ```

Expected:

- After both shells have invoked the time tool, the table shows **exactly ONE**
  row matching `uvx ... mcp-server-time`. Verify with:

  ```powershell
  (.\mcp-mux.exe status |
    ConvertFrom-Json |
    Select-Object -ExpandProperty servers |
    Where-Object { $_.command -eq 'uvx' -and ($_.args -join ' ') -match 'mcp-server-time' }).Count
  # Expected: 1
  ```

- The owner's `cwd_set` array length is **2**, with both shells' working
  directories listed (e.g. `["C:\\Users\\<you>", "D:\\Dev\\mcp-mux"]`). The
  single `cwd` field shows whichever shell spawned first.
- `auto_classification` reads `shared` (or `session-aware`); NOT `isolated`.
- Subsequent invocations from either shell reuse the same `server_id` —
  re-running the count query stays at 1.

Broken signals:

- Two rows for the same `(command, args)` tuple appear — global-first dedup
  did not collapse them.
- `cwd_set` contains only one of the two directories — second-shell spawn
  did not bind to the existing owner.
- `auto_classification` reports `isolated` for a server that legitimately
  advertises shared tools/list. Check the upstream's actual capability
  before concluding v0.25.0 regression — `mcp-server-time` is normally
  shared-classifiable; an isolated verdict suggests classification
  bypassed (look at `classification_source` and `classification_reason`).

## Scenario 5b: Native Muxcore Consumer Hot Update

Run this scenario when the release changes muxcore library contracts, update
helpers, or docs that native consumers depend on.

Objective: prove a product that embeds muxcore can update through its own
public entrypoint without agents reaching for `mcp-mux mux_list` as the source
of truth.

Required consumer fixture:

- A small SessionHandler-based test binary or a real downstream product in a
  read-only fixture mode.
- A stable consumer status or health command/tool that reports at least:
  product version, `engine_name`, `daemon_generation`, `owner_generation`,
  `restore_source`, and shim reconnect counters.
- A consumer update command/tool that exercises the chosen topology from
  `muxcore/README.md`: stable launcher + versioned engine store with
  `RestartWithSuccessor`, fixed replaceable engine path with
  `ApplyUpdateAndRestart`, or custom supervisor.

Commands:

```powershell
# Repo fixture:
pwsh -NoProfile -File .\scripts\smoke-native-sessionhandler-update.ps1 `
  -RunDir .t\native-sessionhandler-update `
  -TimeoutSeconds 120

# If the fixture log repeats:
# control: accept error: open \\.\pipe\mcp-mux-...: Access is denied
# rerun the same command from a normal operator pwsh window or an unsandboxed
# agent shell before treating the result as muxcore evidence.

# Consumer/product equivalent:
# 1. Start the old consumer through the same entrypoint an MCP host uses.
# 2. Call the consumer's own health/status surface and save the old version
#    plus daemon_generation/owner_generation.
# 3. Keep that MCP session open.
# 4. Stage the new consumer binary using the consumer's documented topology.
# 5. Invoke the consumer's own update/apply command.
# 6. Call the same consumer health/status surface through the still-open
#    session and then through a fresh session.
```

Expected:

- The update result reports whether it used graceful restart or fallback
  shutdown/start, and reports partial failure phase when it cannot complete.
- The still-open session can make a successful post-update MCP request.
- The still-open session reports the new product version/executable after
  reconnect.
- A fresh session reports the new product version.
- `daemon_generation` changes, `engine_name` stays the consumer's engine name,
  and reconnect counters show `shim_reconnect_refreshed > 0`,
  `shim_reconnect_fallback_spawned = 0`, and `shim_reconnect_gave_up = 0`.
- `handoff.restored_owner_count` is non-zero when owners existed before update.
- For `SessionHandler` products, product-private in-memory state is either
  durably restored by the consumer or explicitly documented as not preserved.

Forbidden signals:

- `connection closed: initialize response`.
- Inspecting or managing a native consumer solely through `mcp-mux mux_list`.
- Reporting success from `upgrade.Swap` alone without daemon restart/readiness
  evidence.
- Calling `ApplyUpdateAndRestart` against a stable launcher path when the real
  successor should come from a versioned engine pointer.
- `shim_reconnect_fallback_spawned > 0` in the hot-update fixture unless the
  consumer explicitly documents fallback spawn as its accepted recovery mode.

Release handoff evidence:

- When the release contains a critical or consumer-impacting `muxcore` change,
  Scenario 5b is paired with the consumer handoff gate in
  `docs/RELEASE-PROTOCOL.md`.
- The release report must list the Engram issues or comments created for
  `aimux`, `engram`, and any other impacted muxcore consumer. A local fixture
  PASS proves provider behavior; it does not replace consumer adoption
  instructions.
- If Engram cannot be updated, record `CONSUMER_HANDOFF_BLOCKED` and do not
  report the full critical muxcore release scope as shipped.

## Scenario 6: Isolated Owner Short Idle Cleanup (v0.25.0)

Objective: prove a stateful (isolated-classified) upstream tears down within
**~70 seconds** of its last session disconnect — the 60s `IsolatedIdleTimeout`
plus up to one 10s reaper sweep — NOT the general 10-minute owner idle
timeout. Strict-greater comparison + sweep cadence is why ~70s, not 60s
exactly.

Setup:

- v0.25.0 binary installed.
- A stateful MCP server in host config that classifies as isolated. The
  `playwright` MCP and `serena` MCP are good candidates — both classify
  isolated under tools/list inspection.
- **Choose ONE target server before starting** — record its command string
  (e.g. `npx -y @playwright/mcp@latest` or `uvx --from git+...serena ...`).
  Step 2 below filters by that exact command so pre-existing isolated
  owners on the workstation cannot pollute the result.

Commands:

```powershell
# Replace this with the exact command string of YOUR target isolated server,
# verbatim from `mcp-mux status` output (`.command` + `.args` joined).
$targetMatch = 'playwright'   # e.g. matches any owner whose args contain 'playwright'

# 1. Trigger ONE invocation of the isolated server from a host, then FULLY
#    DISCONNECT — close the host session (Ctrl+D, /exit, or kill the host
#    process). Merely stopping to use the server keeps session_count >= 1
#    so the idle-reap clock never starts; the scenario would report a
#    false regression.
# 2. Note the server_id immediately after invocation. Use auto_classification
#    (not "classification" — there is no such field in the status payload).
#    Filter MUST include the target match — without it, an unrelated
#    pre-existing isolated owner could be picked up by step 4.
.\mcp-mux.exe status |
  ConvertFrom-Json |
  Select-Object -ExpandProperty servers |
  Where-Object {
    $_.auto_classification -eq 'isolated' -and
    $_.session_count -eq 0 -and
    (($_.args -join ' ') -match $targetMatch -or $_.command -match $targetMatch)
  } |
  Select-Object server_id, command, auto_classification, session_count, idle_timeout_s, last_session

# 3. Wait 75 seconds (60s idle threshold + up to 10s reaper sweep + 5s buffer).
Start-Sleep -Seconds 75

# 4. Re-check status by server_id captured in step 2:
.\mcp-mux.exe status |
  ConvertFrom-Json |
  Select-Object -ExpandProperty servers |
  Where-Object { $_.server_id -eq '<noted-sid-from-step-2>' }
```

Expected:

- Step 2 finds the isolated owner matching `$targetMatch` with
  `session_count: 0` and `auto_classification: "isolated"`. The selected
  fields (`idle_timeout_s`, `last_session`) verified to exist on every
  server entry in real `mcp-mux status` output.
- Step 4 returns no rows — the owner has been reaped after the sweep.
- A re-spawn of the same upstream from a new session works fine (no zombie
  state).

Broken signals:

- The owner still exists after 70 seconds. (Check whether
  `engine.Config.IsolatedIdleTimeout` was set to `&zero` to disable — that
  is a legitimate operator override, not a bug.)
- The owner exists but its process is gone (`ps`/`Get-Process` shows nothing).

## Scenario 7: Credential Boundary Across Sessions (v0.25.0)

Objective: prove two host sessions with different `GITHUB_TOKEN` env values
get separate owners for the same MCP command, so neither session sees the
other's token in its upstream.

Setup:

- An MCP server in host config that consumes `GITHUB_TOKEN` (the `github`
  MCP server, or any server documented to read GH credentials).
- **Record the exact target command before starting** (same discipline as
  Scenario 6). Example: `npx -y @modelcontextprotocol/server-github`. A
  substring uniquely identifying that command goes into `$targetMatch`
  below so pre-existing github-related owners on the workstation do not
  pollute the count.

Commands:

```powershell
# Substring that uniquely matches YOUR chosen github-credential-consuming
# MCP server's command/args. Adjust if you use a different github server
# package.
$targetMatch = 'server-github'

# Shell A — credential value "abc"
$env:GITHUB_TOKEN = 'abc'
# Start a host session in Shell A and invoke the github MCP server.

# Shell B — credential value "xyz"
$env:GITHUB_TOKEN = 'xyz'
# Start a host session in Shell B and invoke the same github MCP server.

# In a THIRD shell — a plain PowerShell window, NOT a host session — inspect:
.\mcp-mux.exe status |
  ConvertFrom-Json |
  Select-Object -ExpandProperty servers |
  Where-Object {
    ($_.args -join ' ') -match $targetMatch -or $_.command -match $targetMatch
  } |
  Format-Table server_id, session_count, cwd, auto_classification -AutoSize
```

Expected:

- Two distinct `server_id` rows for the same `(command, args)`. The second
  sid carries an `-env-<hash>` suffix (e.g. `<base>-env-9a1b2c3d`).
- Each session sees only its own `GITHUB_TOKEN` value reflected in the
  upstream's behavior (validate by invoking a github tool that echoes the
  authenticated user; the two shells should report different users if the
  tokens belong to different accounts).

Same scenario with the credential entirely ABSENT in one shell:

```powershell
# Shell A — set GITHUB_TOKEN
$env:GITHUB_TOKEN = 'abc'

# Shell B — REMOVE GITHUB_TOKEN
Remove-Item Env:GITHUB_TOKEN -ErrorAction SilentlyContinue
```

Expected: still two distinct owners (presence asymmetry must split, not
collapse). This is the codex PR #121 P1 guarantee — Shell B must not bind
to Shell A's token-bearing owner.

Broken signals:

- One shared owner serves both shells despite different credential values.
- One shared owner serves both shells when one has GITHUB_TOKEN and the
  other does not.
- A session sees the OTHER session's token in upstream behavior (e.g.
  authenticated as the wrong user).

## Scenario 8: Lifecycle Convergence and Tree Authority (v0.27.0)

Run this scenario for v0.27.0 and later releases that touch shim suspension,
launcher dormancy, process containment, snapshot fallback, or live handoff.

Objective: prove disposable processes disappear without losing the host stdio
transport, request delivery stays exactly-once across wake/reconnect, and a
planned restart never leaves process-tree authority split between generations.

Focused automated proof:

```powershell
go test ./cmd/mcp-mux -run 'TestLauncherSupervisorDormant|TestLauncherSupervisorQuiescing|TestReplayLauncherSupervisorHandshake' -count=1
Push-Location muxcore
go test ./owner -run 'TestResilientClient_(Idle|Activity|WriterOwned|DemandBefore|ReplayInit|InitializeHints|ZeroIdle)' -count=1
go test ./daemon -run 'Test(PerformHandoff|HandoffReceipt|ProtocolVersion)' -count=1
go test ./procgroup -run 'Test(GracefulKill_KillsTree|Kill_AfterLeaderExitTerminatesDescendant)' -count=1
go test ./upstream -run 'TestClose_LeaderExitsOnEOFKillsRemainingDescendant' -count=1
Pop-Location
```

Run the tree tests on both Windows and Unix. A PASS on one platform does not
prove the other platform's Job Object/process-group path.

Runtime idle/dormant proof:

1. Build and run an isolated candidate through its stable launcher with a real
   non-persistent MCP upstream. Set short valid test values before starting the
   host so the proof does not wait for production defaults:

   ```powershell
   $env:MCPMUX_SHIM_IDLE_TIMEOUT = '5s'
   $env:MCPMUX_SHIM_DORMANT_GRACE = '3s'
   ```

2. Complete `initialize`, then leave the session with no requests, queued
   frames, progress tokens, or busy declarations. Verify the daemon IPC session
   closes after about five seconds and the supervised engine exits dormant
   after the additional three-second grace without closing host stdio.
3. Send one new tool request through the same host session. Verify the stable
   launcher starts the current active engine, the cached initialize handshake
   completes, and the triggering request reaches the upstream exactly once.
4. Repeat with a persistent fixture (`Persistent: true` or
   `x-mux.persistent: true`). It must remain connected and must not become
   dormant. Repeat with zero or a negative value to prove the selected stage is
   disabled. Remove both environment overrides after the run.

Planned-restart proof:

- v1-to-v2: start a pre-v0.27 candidate, stage v0.28.0, and request the normal
  safe restart with zero live sessions. Evidence must show protocol mismatch
  before owner detach, the predecessor still serving, and no snapshot fallback
  or successor spawn. In-flight work may receive explicit JSON-RPC errors by
  original id; it must not be replayed.
- v2-to-v2: restart between two v2 candidates with a subprocess tree. Evidence
  must show `snapshot_handoff`, a changed daemon generation, the same upstream
  PID/process tree, a complete accepted/aborted final partition, and no
  orphan descendant after final cleanup.

Serena note: its web dashboard is configured separately from mux lifecycle. To
prevent automatic dashboard opening during a Serena runtime proof, add
`--open-web-dashboard false` to `start-mcp-server` or set
`web_dashboard_open_on_launch: false` in `serena_config.yml`; these leave the
dashboard active. Set `web_dashboard: false` only when the dashboard itself
must be disabled. Do not treat dashboard configuration as evidence that muxcore
leaked an upstream descendant; see the [Serena dashboard
documentation](https://oraios.github.io/serena/02-usage/060_dashboard.html).

Broken signals:

- Dormant engine respawns before demand, the host stdio closes, or the wake
  request reaches the upstream more than once.
- An already-sent in-flight request is replayed after reconnect instead of
  receiving one explicit error with its original id.
- A persistent owner suspends or reaps.
- A v1 peer detaches, starts snapshot fallback, or shuts down the predecessor
  instead of failing negotiation with the predecessor still serving.
- A v2 predecessor releases tree authority before final adoption, both daemon
  generations retain authority, or any descendant survives final tree cleanup.

## Scenario 9: Demand-Driven Upstream Materialization (v0.28.0)

Run this scenario for v0.28.0 and later releases that change template-backed
owner discovery, demand-driven upstream creation, template compatibility, or
template revision publication.

Objective: prove template-backed MCP entries publish cached discovery without
starting an upstream, then materialize exactly one compatible owner only when
an uncached request demands it.

Focused automated proof:

```powershell
Push-Location muxcore
$env:GOTOOLCHAIN = 'go1.25.12'
go test ./daemon -run 'Test(TemplateBackedIsolatedStormMaterializesOnlyDemandedOwner|TemplateBackedSharedStormConvergesOneOwnerOneGeneration|CodexEightEntryHandshakeBurstSameTransportWake|CompatibleTemplateCachedBurstThenUncachedWakeSameIPCSession|TemplateCompatibilityUsesExactEnvAndIsolatedCwd|IncompatibleTemplateContextGetsNoCachedFramesAndOneColdProcess|TwoTemplateRevisionMismatchesThenOneColdBypassWithoutStaleFrames|PersistentTemplateMaterializesEagerly)' -count=1
Pop-Location
```

Customer-mode observed invariants:

1. Start eight template-backed Codex-style entries with compatible isolated
   contexts. Cached discovery completes with **8 owners, 0 upstream processes,
   and 0 starts**.
2. Send an uncached request through one existing open IPC transport. The
   request returns successfully on that same transport, then topology reports
   **8 owners, 1 upstream process, and 1 start**.
3. Repeat the burst for a shared template. The storm converges to **1 owner and
   0 processes** before demand, then produces exactly **1 generation** after
   demand.
4. Replay using an incompatible CWD or effective environment. It reuses zero
   cached discovery and follows the bounded cold path; it must not receive
   stale cached frames.
5. Repeat with a persistent/eager template owner. It still starts eagerly,
   rather than waiting for an uncached request.

Broken signals:

- Any template-backed discovery starts an upstream before an uncached request,
  or eight isolated entries do not remain eight owners with zero starts.
- The uncached request fails, uses a new IPC transport, creates more than one
  upstream generation, or leaves counts other than 8 owners / 1 process / 1
  start.
- A shared storm has more than one owner before demand or more than one
  generation after demand.
- An incompatible CWD or effective environment receives cached frames, retries
  without the bounded cold path, or sees stale frames after revision mismatch.
- A persistent/eager template owner waits for demand instead of starting
  eagerly.

## Scenario 11: Upstream maintenance replacement

Run this scenario for the upstream maintenance hold change and later changes to
its fence, drain, lease, shim, CLI, or recovery behavior. It is an explicit
cross-platform release gate, not an automatically enrolled critical-suite test.
This section defines expected observations; it does not record a runtime PASS.

Use the exact integrated candidate, Go compatible with `go.mod`, and PowerShell
7 on Windows and Unix. Root allocates fresh owned scratch beneath the PRIMARY
checkout's `.agent`, checks capacity, and binds the candidate binary hash to its
source SHA. Never use the linked worktree's inner `.agent`, OS temp, or the
operator's daemon/config namespace. Keep the primary path short enough for Unix
domain socket limits. The runner rejects a long control endpoint instead of
silently using another namespace.

The canonical inputs are `SourceRoot`, `CandidateBinary`, `OutputDir`, and
`TimeoutSeconds`, plus required explicit `ScratchRoot` and `EvidencePath`.
`ScratchRoot` must already exist under PRIMARY `.agent`, outside worktrees.
`OutputDir` and `EvidencePath` must be new paths beneath it. `CandidateBinary`
is input only; fixture builds, caches, runtime/config directories, executable
copies, and evidence stay in that owned scratch. No `Binary` alias or temp default
is provided.

After root has allocated `$Scratch` and built `$CandidateBinary` from
`$CandidateRoot`, run on Windows:

```powershell
pwsh -NoProfile -File "$CandidateRoot/scripts/smoke-upstream-maintenance.ps1" `
  -SourceRoot $CandidateRoot -CandidateBinary $CandidateBinary `
  -ScratchRoot $Scratch -OutputDir "$Scratch/w" `
  -EvidencePath "$Scratch/windows.json" -TimeoutSeconds 180
if ($LASTEXITCODE -ne 0) { throw 'Windows maintenance replacement proof failed' }
```

From a separate exact-head Unix checkout, use that checkout's PRIMARY `.agent`
scratch and Unix candidate binary:

```powershell
pwsh -NoProfile -File "$CandidateRoot/scripts/smoke-upstream-maintenance.ps1" `
  -SourceRoot $CandidateRoot -CandidateBinary $CandidateBinary `
  -ScratchRoot $Scratch -OutputDir "$Scratch/u" `
  -EvidencePath "$Scratch/unix.json" -TimeoutSeconds 180
if ($LASTEXITCODE -ne 0) { throw 'Unix maintenance replacement proof failed' }
```

Both platforms are required. The runner builds link-time v1/v2 versions of
`scripts/lifecycle-smoke-upstream/main.go` and the unchanged native
`testdata/mock_modern_server.go`. The lifecycle fixture defaults to isolated;
only this smoke opts into sharing and frame capture. The script invokes the
actual candidate CLI, keeps two original legacy host pipes open, and uses private
current-user config/IPC contexts. It does not substitute mocked hold results,
source-text checks, PID cleanup, or rename-only replacement.

Required live observations:

1. Two initialized legacy hosts share one real v1 leader and its managed
   descendant. OS executable/start-time and child-parent observations bind their
   identities; capture has a positive request denominator.
2. Actual `hold <observed-server-id> --ttl 30s --drain-timeout 2s --json`
   fences before new demand. Already-forwarded short work completes before the
   accepted single deadline. A second long operation is ended once at retirement,
   without replay. HELD requires future expiry and `trees_retired=true`.
3. Every recorded selected leader and descendant is dead. No new scoped fixture
   generation appears while held. The installer opens and overwrites the exact
   previously executed path with v2 bytes, flushes it, and observes a changed
   SHA-256. Dead-tree proof is required even when Unix permits rename or unlink.
4. Held numeric/string requests receive exactly one original-ID `-32005`
   `maintenance_held` error without ordinary reconnect grace. Normally cached
   discovery also errors. Notifications invent no response ID. Captures exclude
   held frames before and after release; original native pipe handles stay open.
5. Renew returns expiry from acceptance. Competing/stale IDs cannot mutate the
   lease. Managed `restart_owner`, graceful restart/handoff, shutdown, CLI stop,
   launcher forced restart, and staged upgrade refuse terminally without replacing
   the daemon or starting the held fixture. Held CLI refusal uses the fixed safe
   message, not merely a nonzero exit. Launcher, staged binary, layout, and active
   pointer file sets/hashes remain unchanged. An unrelated managed context stays usable.
6. Exact resume and a separate short safely proven TTL each admit one new v2
   shared generation serving both original legacy host pipes. No held or
   terminated request reaches that generation.
7. A dedicated HELD scenario loses only its observed owned daemon. The aware
   replacement reloads the same durable lease before admission. Buffered
   numeric/string demand receives maintenance errors; overwrite/resume then
   reaches v2 on the unchanged host pipes. No operator ledger is corrupted.
8. A known native MCP `2026-07-28` host uses required per-request metadata and
   the unchanged modern fixture. Held original IDs error. Fresh demand after
   resume succeeds through fresh same-era isolated admission on the same pipes.
   Exact upstream captures contain no injected legacy bootstrap or held replay.
   Generic `-32603`, EOF, and an unnamed new-launch refusal are failures for this
   selected proof, not modern restoration evidence.
9. Safe control/status readbacks contain no fixture credential value or request
   marker. Cleanup closes owned hosts, resumes only safely proven holds, and
   uses the exact private lifecycle endpoint. A blocked lease or failed cleanup
   keeps evidence/resources for root recovery and makes the exit nonzero.

`summary.json`, `transcript.ndjson`, fixture generation records/captures, and the
explicit `EvidencePath` retain observed commands, timestamps, exit codes,
SHA/binary/fixture hashes, original host/pipe identities, scoped PID/tree
observations, control replies, overwritten file hashes, and failures. PASS means
all live observations passed with a nonzero denominator and cleanup completed.
It is not full feature or release acceptance.

Root also runs named focused `TestMaintenance` cases for deterministic
queue/reconnect, start/install, and hold-versus-activation races, finite
CWD/era/security/namespace scopes, stale identities/generations, corrupt/incomplete
authority, failed persistence, blocked retirement despite expiry/resume,
unsupported old endpoints/optional handlers, whole-millisecond duration boundaries,
and library update-helper terminal refusal. Prove that status/pure startup checks
do not acquire/write the namespace lock or start a daemon, and that offline/old
activation requires locked persisted-clear proof. Record actual case names,
nonzero denominators, and RED/GREEN evidence. The live runner lists these as
`required_focused_proof`; it does not execute or mark them PASS. Timing-only live
traffic cannot prove those races, and missing cases cannot be marked PASS.

```powershell
Push-Location "$CandidateRoot/muxcore"
go test ./control ./daemon ./owner ./engine ./upstream -run '^TestMaintenance' -count=1
go test -race ./control ./daemon ./owner ./engine ./upstream -run '^TestMaintenance' -count=1
Pop-Location
Push-Location $CandidateRoot
go test ./cmd/mcp-mux ./internal/mcpserver -run '^TestMaintenance' -count=1
Pop-Location
```

Keep existing Scenario 5b, applicable Scenario 8 Windows/Unix tree gates, the
repository critical suite and its `MCP_LAUNCHER`/`-Launcher` prerequisite, and
R1 native parity runners. Scenario 11 supplements them. Root runs integrated
tests/vet once after makers land, then follows `docs/RELEASE-PROTOCOL.md` with
the actual selected release version and delivered-artifact/fresh-session proof.

Support limits are part of acceptance: the finite admitted context set is not
host-wide file-lock authority; old daemons are unsupported; old shims receive
physical start fencing only, not immediate-error/non-replay guarantees.
Uncoordinated standalone/direct-owner launch, foreign engines, arbitrary old
binaries, and manual active-pointer replacement are unsupported. Controlled
restart/handoff/shutdown/downgrade and idle exit refuse while any fence remains.
Before downgrade, clear safely proven holds using the current aware binary.
Blocked retirement never TTL-clears; retain its authority and do not delete
ledgers, sweep PIDs, or directly start the upstream to manufacture recovery.
Controlled update/install/swap, layout/bootstrap mutation, and active-pointer
updates share the existing namespace file lock with hold-ledger mutation. This
coordination does not extend the finite context set or add a host-wide file lock
or updater lease.

## Verdict Template

Create a run report under `.agent/reports/emulation-playbook-run-YYYYMMDD-HHMM.md`
with this table:

| # | Scenario | Expected | Observed | Verdict |
| --- | --- | --- | --- | --- |
| 1 | Isolated Build | Candidate binary builds |  | PASS/FAIL |
| 2 | Real Time Upstream Reconnect | Smoke verdict PASS |  | PASS/FAIL |
| 3 | Current Topology Oracle | PoC verdict PASS |  | PASS/FAIL |
| 4 | Local Deployment Upgrade | Production binary upgraded and status works |  | PASS/FAIL/SKIPPED |
| 5 | Global-First Owner Dedup (v0.25.0) | 1 owner per (cmd, args) across 2 cwds |  | PASS/FAIL |
| 5b | Native Muxcore Consumer Hot Update | Consumer-owned update path preserves MCP session and reports new version |  | PASS/FAIL/SKIPPED |
| 6 | Isolated Short Idle Cleanup (v0.25.0) | Target isolated owner reaped within ~70s of zero sessions (60s idle + 10s sweep) |  | PASS/FAIL |
| 7 | Credential Boundary (v0.25.0) | 2 owners under different credential, 2 under presence asymmetry |  | PASS/FAIL |
| 8 | Lifecycle Convergence and Tree Authority (v0.27.0+) | Dormant wake is exact-once; v1 skew aborts pre-detach; post-Hello fallback is single-shot; same-v2 handoff retains one full-tree authority |  | PASS/FAIL |
| 9 | Demand-Driven Upstream Materialization (v0.28.0) | 8 owners / 0 processes before demand; same transport response; 8 / 1 after one wake; one authority |  | PASS/FAIL |
| 10 | Critical muxcore consumer handoff | `CONSUMER_HANDOFF_PASS`, or `CONSUMER_HANDOFF_BLOCKED` with the full critical scope not called shipped |  | PASS/BLOCKED |
| 11 | Upstream maintenance replacement | Actual Windows and Unix overwrite; unchanged aware host pipes; original-ID errors/no replay; safe resume/TTL/aware recovery and fresh native modern demand, plus focused maintenance proof |  | PASS/FAIL |

Overall verdict:

- `PRODUCT_WORKS`: all required scenarios PASS.
- `PARTIALLY_WORKS`: all required scenarios PASS but surprises need a release
  note or operator decision.
- `BROKEN`: any required scenario FAIL.

The release closeout must name every affected consumer and record exactly one
of `CONSUMER_HANDOFF_PASS` or `CONSUMER_HANDOFF_BLOCKED`. The PASS marker is
valid only after the released muxcore tag resolves and every touched Engram
issue/comment has been re-read.
