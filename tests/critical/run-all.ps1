# @critical
# @category: smoke
# @features: [MCP-MUX-CLI, MCP-MUX-RECONNECT, MCP-MUX-CURRENT-TOPOLOGY, MUXCORE-NATIVE-UPDATE, NVMD-142]
# @dev_stand: optional
param(
    [int]$WatchSeconds = 1,
    [int]$TimeoutSeconds = 60,
    [string]$Launcher = $env:MCP_LAUNCHER
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$Stamp = Get-Date -Format "yyyyMMdd-HHmmss"
$RunDir = Join-Path $RepoRoot ".agent\tmp\critical-suite-$Stamp"
$ReportsDir = Join-Path $RepoRoot ".agent\reports"
$Binary = Join-Path $RunDir "mcp-mux.exe"
$SmokeEvidence = Join-Path $ReportsDir "critical-smoke-time-upstream-$Stamp.json"
$NativeUpdateEvidence = Join-Path $ReportsDir "critical-native-sessionhandler-update-$Stamp.json"
$LifecycleEvidence = Join-Path $ReportsDir "critical-process-lifecycle-$Stamp.json"
$JsonReport = Join-Path $ReportsDir "critical-suite-$Stamp.json"
$MdReport = Join-Path $ReportsDir "critical-suite-$Stamp.md"
$HistoryReport = Join-Path $ReportsDir "critical-suite-history.jsonl"

if ([string]::IsNullOrWhiteSpace($Launcher)) {
    $launcherCommand = Get-Command mcp-launcher -ErrorAction SilentlyContinue
    if ($launcherCommand) {
        $Launcher = $launcherCommand.Source
    }
}

New-Item -ItemType Directory -Force -Path $RunDir | Out-Null
New-Item -ItemType Directory -Force -Path $ReportsDir | Out-Null

$results = [System.Collections.Generic.List[object]]::new()
$started = Get-Date
$runId = [guid]::NewGuid().ToString()
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
        if (Test-Path -LiteralPath $Binary) {
            Remove-Item -LiteralPath $Binary -Force
        }
        go build -trimpath -o $Binary .\cmd\mcp-mux
        if ($LASTEXITCODE -ne 0) {
            throw "go build exited with code $LASTEXITCODE"
        }
        if (-not (Test-Path -LiteralPath $Binary -PathType Leaf)) {
            throw "go build did not produce candidate binary: $Binary"
        }
    }

    Invoke-CriticalStep "process lifecycle convergence smoke" {
        & .\scripts\smoke-process-lifecycle.ps1 `
            -CandidateBinary $Binary `
            -RunDir (Join-Path $RunDir "process-lifecycle") `
            -EvidencePath $LifecycleEvidence `
            -TimeoutSeconds $TimeoutSeconds
        if ($LASTEXITCODE -ne 0) {
            throw "smoke-process-lifecycle exited with code $LASTEXITCODE"
        }
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
        & .\scripts\run-current-topology-poc.ps1 -Launcher $Launcher -WatchSeconds $WatchSeconds
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
    run_id = $runId
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
    failed_tests = @($failed | ForEach-Object {
        [pscustomobject]@{
            file = "tests/critical/run-all.ps1"
            name = $_.name
            category = "smoke"
            features = @("MCP-MUX-CLI", "MCP-MUX-RECONNECT", "MCP-MUX-CURRENT-TOPOLOGY", "MUXCORE-NATIVE-UPDATE", "NVMD-142")
            message = $_.error
        }
    })
    categories_missing_coverage = @()
    error = $runError
    binary = $Binary
    launcher = $Launcher
    smoke_evidence = $SmokeEvidence
    native_update_evidence = $NativeUpdateEvidence
    lifecycle_evidence = $LifecycleEvidence
    results = $results
}

$report | ConvertTo-Json -Depth 32 | Set-Content -LiteralPath $JsonReport -Encoding UTF8
$report | ConvertTo-Json -Depth 32 -Compress | Add-Content -LiteralPath $HistoryReport -Encoding UTF8

$md = @(
    "# Critical Suite Run - $Stamp",
    "",
    "**Verdict:** $verdict",
    "**Runtime seconds:** $($report.runtime_seconds)",
    "**Binary:** $Binary",
    "**Smoke evidence:** $SmokeEvidence",
    "**Native update evidence:** $NativeUpdateEvidence",
    "**Process lifecycle evidence:** $LifecycleEvidence",
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
