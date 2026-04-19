#Requires -Version 5.1
<#
.SYNOPSIS
    Post-deploy verification for upstream-survives-daemon-restart (T036).

.DESCRIPTION
    Asserts that `mcp-mux upgrade --restart` preserves every upstream PID
    across the daemon restart (v0.21.0+ handoff path) OR documents the
    legacy fallback path when the old daemon predates the handoff protocol.

.PARAMETER Binary
    Path to the mcp-mux binary. Defaults to 'mcp-mux' on PATH.

.PARAMETER TimeoutSeconds
    Seconds to wait for the successor daemon to become ready after restart.
    Defaults to 15.

.OUTPUTS
    Exit code:
      0 = PASS (all upstream PIDs survived OR FR-8 fallback path observed)
      1 = FAIL (PIDs diverged without a fallback signal — regression)
      2 = SETUP ERROR (binary missing, daemon not running, etc.)

.EXAMPLE
    ./scripts/verify-handoff.ps1
    ./scripts/verify-handoff.ps1 -Binary 'C:\tools\mcp-mux.exe' -TimeoutSeconds 30
#>

[CmdletBinding()]
param(
    [string] $Binary = $(if ($env:MCP_MUX_BINARY) { $env:MCP_MUX_BINARY } else { 'mcp-mux' }),
    [int]    $TimeoutSeconds = $(if ($env:VERIFY_TIMEOUT) { [int]$env:VERIFY_TIMEOUT } else { 15 })
)

$ErrorActionPreference = 'Stop'

function Write-Step { param($Msg) Write-Host "[verify-handoff] $Msg" }
function Invoke-Die  { param($Msg) Write-Host "[verify-handoff] FATAL: $Msg"; exit 2 }
function Invoke-Fail { param($Msg) Write-Host "[verify-handoff] FAIL: $Msg";  exit 1 }

if (-not (Get-Command $Binary -ErrorAction SilentlyContinue)) {
    Invoke-Die "mcp-mux binary not found: $Binary"
}

function Get-Status {
    try {
        $raw = & $Binary status 2>$null
        if ($LASTEXITCODE -ne 0 -or -not $raw) {
            throw "mcp-mux status exited $LASTEXITCODE"
        }
        return ($raw | ConvertFrom-Json -Depth 10)
    }
    catch {
        Invoke-Die "mcp-mux status failed — is the daemon running? ($_)"
    }
}

function Get-PidsSnapshot {
    param($Status)
    $rows = @()
    foreach ($server in ($Status.servers | Where-Object { $_.pid })) {
        $rows += [pscustomobject]@{
            Sid     = $server.server_id
            Pid     = [int]$server.pid
            Command = $server.command
        }
    }
    return $rows | Sort-Object Sid
}

function Get-HandoffCounter {
    param($Status, [string]$Key)
    if ($Status.handoff -and $Status.handoff.PSObject.Properties[$Key]) {
        return [int64]$Status.handoff.$Key
    }
    return [int64]0
}

Write-Step "step 1/5: capturing pre-restart state"
$statusBefore = Get-Status
$before       = Get-PidsSnapshot $statusBefore
if ($before.Count -eq 0) {
    Invoke-Die "no upstream PIDs reported before restart — start at least one upstream before verifying"
}
$beforeAttempted   = Get-HandoffCounter $statusBefore 'attempted'
$beforeTransferred = Get-HandoffCounter $statusBefore 'transferred'
$beforeFallback    = Get-HandoffCounter $statusBefore 'fallback'
Write-Step ("  observed {0} upstream(s); pre counters: attempted={1} transferred={2} fallback={3}" -f `
    $before.Count, $beforeAttempted, $beforeTransferred, $beforeFallback)

Write-Step "step 2/5: invoking mcp-mux upgrade --restart"
& $Binary upgrade --restart
if ($LASTEXITCODE -ne 0) {
    Invoke-Fail "mcp-mux upgrade --restart exited $LASTEXITCODE"
}

Write-Step "step 3/5: waiting up to ${TimeoutSeconds}s for successor daemon ready"
$elapsed = 0
$ready   = $false
while ($elapsed -lt $TimeoutSeconds) {
    try {
        $null = & $Binary status 2>$null
        if ($LASTEXITCODE -eq 0) {
            $ready = $true
            Write-Step "  daemon ready after ${elapsed}s"
            break
        }
    }
    catch { }
    Start-Sleep -Seconds 1
    $elapsed++
}
if (-not $ready) {
    Invoke-Fail "successor daemon did not come up within ${TimeoutSeconds}s"
}

Write-Step "step 4/5: capturing post-restart state"
$statusAfter      = Get-Status
$after            = Get-PidsSnapshot $statusAfter
$afterAttempted   = Get-HandoffCounter $statusAfter 'attempted'
$afterTransferred = Get-HandoffCounter $statusAfter 'transferred'
$afterFallback    = Get-HandoffCounter $statusAfter 'fallback'
Write-Step ("  observed {0} upstream(s); post counters: attempted={1} transferred={2} fallback={3}" -f `
    $after.Count, $afterAttempted, $afterTransferred, $afterFallback)

Write-Step "step 5/5: comparing PID sets"
$diff = Compare-Object -ReferenceObject $before -DifferenceObject $after -Property Sid,Pid -PassThru
if (-not $diff) {
    Write-Step ("PASS: all {0} upstream PIDs survived the restart" -f $after.Count)
    Write-Step ("       handoff counters delta: attempted+{0} transferred+{1} fallback+{2}" -f `
        ($afterAttempted - $beforeAttempted),
        ($afterTransferred - $beforeTransferred),
        ($afterFallback - $beforeFallback))
    exit 0
}

# PIDs diverged. If fallback counter incremented, that's FR-8 — expected for legacy.
if ($afterFallback -gt $beforeFallback) {
    Write-Step ("PASS (fallback): handoff_fallback incremented by {0}" -f ($afterFallback - $beforeFallback))
    Write-Step "       this is the FR-8 zero-deployment-impact path (old daemon had no handoff code,"
    Write-Step "       or protocol versions mismatched). Upstreams were respawned — next restart"
    Write-Step "       will use the handoff path."
    Write-Step "PID changes:"
    $diff | Format-Table -AutoSize | Out-String | Write-Host
    exit 0
}

Write-Step "PID diff (unexpected — no fallback counter bump):"
$diff | Format-Table -AutoSize | Out-String | Write-Host
Invoke-Fail "upstream PIDs diverged without a fallback signal — handoff regression suspected"
