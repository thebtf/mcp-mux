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

## Prerequisites

- Windows PowerShell from the repository root.
- Go toolchain matching CI (`go version` should be compatible with `go 1.25`).
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

Commands:

```powershell
go build -trimpath -o .\mcp-mux.exe~ .\cmd\mcp-mux
.\mcp-mux.exe upgrade --restart
.\mcp-mux.exe status
```

Expected:

- The stable launcher `mcp-mux.exe` remains in place; upgrade does not attempt
  to rename the configured binary while live shim/daemon processes may hold it.
- The pending binary `mcp-mux.exe~` is installed under
  `mcp-mux.versions/<hash>/mcp-mux-engine.exe`, and
  `mcp-mux.versions/active.txt` points at that engine.
- `status` responds without handshake failure.
- Existing consumers reconnect through the shim/daemon path rather than
  requiring manual config edits.

Broken signals:

- The build cannot write `mcp-mux.exe~`.
- `upgrade --restart` reports `rename current to old: Access is denied`.
- `upgrade --restart` reports socket handoff failure without fallback recovery.
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

# 1. Trigger ONE invocation of the isolated server from a host, then close
#    the host session (or stop using that server in that session).
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

Commands:

```powershell
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
  Where-Object { $_.command -match 'github' -or ($_.args -join ' ') -match 'github' } |
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
| 6 | Isolated Short Idle Cleanup (v0.25.0) | Target isolated owner reaped within ~70s of zero sessions (60s idle + 10s sweep) |  | PASS/FAIL |
| 7 | Credential Boundary (v0.25.0) | 2 owners under different credential, 2 under presence asymmetry |  | PASS/FAIL |

Overall verdict:

- `PRODUCT_WORKS`: all required scenarios PASS.
- `PARTIALLY_WORKS`: all required scenarios PASS but surprises need a release
  note or operator decision.
- `BROKEN`: any required scenario FAIL.
