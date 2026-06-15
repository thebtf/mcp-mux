# @critical
# @category: smoke
# @features: [MCP-MUX-CLI, MCP-MUX-RECONNECT, MCP-MUX-CURRENT-TOPOLOGY, MUXCORE-NATIVE-UPDATE]
# @dev_stand: optional
param(
    [int]$WatchSeconds = 1,
    [int]$TimeoutSeconds = 60
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$Stamp = Get-Date -Format "yyyyMMdd-HHmmss"
$RunDir = Join-Path $RepoRoot ".agent\tmp\critical-suite-$Stamp"
$ReportsDir = Join-Path $RepoRoot ".agent\reports"
$Binary = Join-Path $RunDir "mcp-mux.exe"
$SmokeEvidence = Join-Path $ReportsDir "critical-smoke-time-upstream-$Stamp.json"
$NativeUpdateEvidence = Join-Path $ReportsDir "critical-native-sessionhandler-update-$Stamp.json"
$JsonReport = Join-Path $ReportsDir "critical-suite-$Stamp.json"
$MdReport = Join-Path $ReportsDir "critical-suite-$Stamp.md"

New-Item -ItemType Directory -Force -Path $RunDir | Out-Null
New-Item -ItemType Directory -Force -Path $ReportsDir | Out-Null

$results = [System.Collections.Generic.List[object]]::new()
$started = Get-Date
$runError = ""

function Invoke-CriticalStep {
    param(
        [string]$Name,
        [scriptblock]$Body
    )

    $stepStarted = Get-Date
    try {
        & $Body
        $results.Add([pscustomobject]@{
            name = $Name
            verdict = "PASS"
            duration_ms = [int]((Get-Date) - $stepStarted).TotalMilliseconds
            error = ""
        })
    } catch {
        $results.Add([pscustomobject]@{
            name = $Name
            verdict = "FAIL"
            duration_ms = [int]((Get-Date) - $stepStarted).TotalMilliseconds
            error = $_.Exception.Message
        })
        throw
    }
}

Push-Location $RepoRoot
try {
    Invoke-CriticalStep "build isolated mcp-mux binary" {
        go build -trimpath -o $Binary .\cmd\mcp-mux
    }

    Invoke-CriticalStep "real time upstream reconnect smoke" {
        & .\scripts\smoke-time-upstream.ps1 `
            -Binary $Binary `
            -EvidencePath $SmokeEvidence `
            -TimeoutSeconds $TimeoutSeconds
        if ($LASTEXITCODE -ne 0) {
            throw "smoke-time-upstream exited with code $LASTEXITCODE"
        }
    }

    Invoke-CriticalStep "current topology proofing oracle" {
        & .\scripts\run-current-topology-poc.ps1 -WatchSeconds $WatchSeconds
        if ($LASTEXITCODE -ne 0) {
            throw "run-current-topology-poc exited with code $LASTEXITCODE"
        }
    }

    Invoke-CriticalStep "native SessionHandler update smoke" {
        & .\scripts\smoke-native-sessionhandler-update.ps1 `
            -RunDir (Join-Path $RunDir "native-sessionhandler-update") `
            -EvidencePath $NativeUpdateEvidence `
            -TimeoutSeconds $TimeoutSeconds
        if ($LASTEXITCODE -ne 0) {
            throw "smoke-native-sessionhandler-update exited with code $LASTEXITCODE"
        }
    }
} catch {
    $runError = $_.Exception.Message
} finally {
    Pop-Location
}

$failed = @($results | Where-Object { $_.verdict -ne "PASS" })
$verdict = if ($failed.Count -eq 0) { "PASS" } else { "FAIL" }
$finished = Get-Date

$report = [pscustomobject]@{
    wrapper_version = "1.0.0"
    timestamp = $started.ToUniversalTime().ToString("o")
    finished = $finished.ToUniversalTime().ToString("o")
    verdict = $verdict
    counts = [pscustomobject]@{
        total = $results.Count
        passed = @($results | Where-Object { $_.verdict -eq "PASS" }).Count
        failed = $failed.Count
        skipped = 0
        errored = 0
    }
    dev_stand = [pscustomobject]@{
        brought_up = $false
        teardown_success = $true
        shape = "local-isolated-processes"
    }
    runtime_seconds = [math]::Round(($finished - $started).TotalSeconds, 3)
    error = $runError
    binary = $Binary
    smoke_evidence = $SmokeEvidence
    native_update_evidence = $NativeUpdateEvidence
    results = $results
}

$report | ConvertTo-Json -Depth 32 | Set-Content -LiteralPath $JsonReport -Encoding UTF8

$md = @(
    "# Critical Suite Run - $Stamp",
    "",
    "**Verdict:** $verdict",
    "**Runtime seconds:** $($report.runtime_seconds)",
    "**Binary:** $Binary",
    "**Smoke evidence:** $SmokeEvidence",
    "**Native update evidence:** $NativeUpdateEvidence",
    "",
    "| Step | Verdict | Duration ms | Error |",
    "| --- | --- | ---: | --- |"
)
foreach ($result in $results) {
    $errorText = if ([string]::IsNullOrWhiteSpace($result.error)) { "" } else { $result.error.Replace("|", "\|") }
    $md += "| $($result.name) | $($result.verdict) | $($result.duration_ms) | $errorText |"
}
$md | Set-Content -LiteralPath $MdReport -Encoding UTF8

Write-Output ($report | ConvertTo-Json -Depth 32)

if ($verdict -ne "PASS") {
    exit 1
}
