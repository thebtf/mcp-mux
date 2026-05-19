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

- The pending binary `mcp-mux.exe~` is consumed by `upgrade --restart`.
- `status` responds without handshake failure.
- Existing consumers reconnect through the shim/daemon path rather than
  requiring manual config edits.

Broken signals:

- The build cannot write `mcp-mux.exe~`.
- `upgrade --restart` reports socket handoff failure without fallback recovery.
- `status` cannot contact the daemon after restart.

## Verdict Template

Create a run report under `.agent/reports/emulation-playbook-run-YYYYMMDD-HHMM.md`
with this table:

| # | Scenario | Expected | Observed | Verdict |
| --- | --- | --- | --- | --- |
| 1 | Isolated Build | Candidate binary builds |  | PASS/FAIL |
| 2 | Real Time Upstream Reconnect | Smoke verdict PASS |  | PASS/FAIL |
| 3 | Current Topology Oracle | PoC verdict PASS |  | PASS/FAIL |
| 4 | Local Deployment Upgrade | Production binary upgraded and status works |  | PASS/FAIL/SKIPPED |

Overall verdict:

- `PRODUCT_WORKS`: all required scenarios PASS.
- `PARTIALLY_WORKS`: all required scenarios PASS but surprises need a release
  note or operator decision.
- `BROKEN`: any required scenario FAIL.
