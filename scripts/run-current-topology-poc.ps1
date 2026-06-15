param(
    [string]$Launcher = $env:MCP_LAUNCHER,
    [int]$WatchSeconds = 2
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent $PSScriptRoot
$RuntimeDir = Join-Path $RepoRoot ".agent\current-topology-poc"
$Binary = Join-Path $RuntimeDir "current-topology-poc.exe"
$ControlSocket = Join-Path $RuntimeDir "control.sock"

New-Item -ItemType Directory -Force -Path $RuntimeDir | Out-Null

$env:CURRENT_TOPOLOGY_POC_HOME = $RuntimeDir
$env:CURRENT_TOPOLOGY_POC_CTL = $ControlSocket
$env:CURRENT_TOPOLOGY_POC_QUIET = "1"

function Invoke-NativeStep {
    param(
        [string]$Name,
        [scriptblock]$Body
    )

    Write-Host ""
    Write-Host "== $Name =="
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Name failed with exit code $LASTEXITCODE"
    }
}

function Stop-PocDaemon {
    if (Test-Path -LiteralPath $Binary) {
        try {
            $Output = & $Binary --poc-control shutdown 2>$null
            if ($LASTEXITCODE -eq 0 -and $Output) {
                $Output | Out-Host
            }
        } catch {
        }
    }
    Start-Sleep -Milliseconds 300
    if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Windows)) {
        $binaryPath = (Resolve-Path -LiteralPath $Binary -ErrorAction SilentlyContinue).Path
        if ($binaryPath) {
            Get-CimInstance Win32_Process -Filter "name = 'current-topology-poc.exe'" -ErrorAction SilentlyContinue |
                Where-Object { $_.CommandLine -like "*$binaryPath*" -and $_.CommandLine -like "*--muxcore-daemon*" } |
                ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
        }
    }
    Remove-Item -LiteralPath $ControlSocket -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath (Join-Path $RuntimeDir "owners.snapshot.json") -ErrorAction SilentlyContinue
    Get-ChildItem -LiteralPath $RuntimeDir -Filter "*.owner.sock" -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
    $global:LASTEXITCODE = 0
}

function Get-PocStatus {
    $raw = & $Binary --poc-control status 2>$null
    if ($LASTEXITCODE -ne 0) {
        throw "status failed with exit code $LASTEXITCODE"
    }
    return $raw | ConvertFrom-Json
}

function Wait-PocReady {
    $Deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $Deadline) {
        try {
            $Status = Get-PocStatus
            if ($Status.ready -eq $true) {
                return $Status
            }
        } catch {
        }
        Start-Sleep -Milliseconds 100
    }
    throw "PoC daemon did not become ready"
}

function Invoke-WindowsPersistEmulation {
    $Daemon = Start-Process -FilePath $Binary -ArgumentList "--muxcore-daemon" -WindowStyle Hidden -PassThru
    Write-Host "  daemon pid=$($Daemon.Id)"
    $StatusA = Wait-PocReady
    Write-Host "  status A pid=$($StatusA.pid) generation=$($StatusA.daemon_generation)"

    & $Launcher -binary $Binary -mode tool -tool topology_state -args "{}" -expect-tools 1 -timeout 10
    if ($LASTEXITCODE -ne 0) {
        throw "launcher tool session in persist emulation failed with exit code $LASTEXITCODE"
    }

    Start-Sleep -Seconds $WatchSeconds
    $StatusB = Get-PocStatus
    Write-Host "  status B pid=$($StatusB.pid) generation=$($StatusB.daemon_generation) ready=$($StatusB.ready)"

    if ($StatusA.pid -ne $StatusB.pid) {
        throw "daemon pid changed during persist emulation: A=$($StatusA.pid) B=$($StatusB.pid)"
    }
    if ($StatusB.ready -ne $true) {
        throw "daemon is not ready after persist emulation"
    }
}

function Wait-PocReplacementReady {
    param(
        [int]$PreviousPid,
        [string]$PreviousGeneration
    )

    $Deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $Deadline) {
        try {
            $Status = Get-PocStatus
            if ($Status.ready -eq $true -and ($Status.pid -ne $PreviousPid -or $Status.daemon_generation -ne $PreviousGeneration)) {
                return $Status
            }
        } catch {
        }
        Start-Sleep -Milliseconds 100
    }
    throw "PoC replacement daemon did not become ready"
}

function Invoke-WindowsKillReconnectEmulation {
    $Started = Get-Date
    Write-Host "  Session A: connect"
    & $Launcher -binary $Binary -mode tool -tool topology_state -args "{}" -expect-tools 1 -timeout 10
    if ($LASTEXITCODE -ne 0) {
        throw "launcher session A failed with exit code $LASTEXITCODE"
    }
    $StatusA = Get-PocStatus
    Write-Host "  daemon A pid=$($StatusA.pid) generation=$($StatusA.daemon_generation)"

    Stop-Process -Id $StatusA.pid -Force
    Start-Sleep -Milliseconds 300

    Write-Host "  Session B: reconnect"
    & $Launcher -binary $Binary -mode tool -tool topology_state -args "{}" -expect-tools 1 -timeout 10
    if ($LASTEXITCODE -ne 0) {
        throw "launcher session B failed with exit code $LASTEXITCODE"
    }
    $StatusB = Get-PocStatus
    Write-Host "  daemon B pid=$($StatusB.pid) generation=$($StatusB.daemon_generation) ready=$($StatusB.ready)"

    if ($StatusB.ready -ne $true) {
        throw "daemon is not ready after kill reconnect"
    }
    if ($StatusA.pid -eq $StatusB.pid) {
        throw "daemon pid did not change after hard kill: $($StatusA.pid)"
    }
    $Elapsed = (Get-Date) - $Started
    if ($Elapsed.TotalSeconds -gt 30) {
        throw "kill reconnect exceeded 30s stdio timeout: $($Elapsed.TotalMilliseconds)ms"
    }
    Write-Host ("  total_recovery={0:n1}ms daemon_pid_A={1} daemon_pid_B={2}" -f $Elapsed.TotalMilliseconds, $StatusA.pid, $StatusB.pid)
}

function Invoke-WindowsPhase2RestartEmulation {
    & $Launcher -binary $Binary -mode tool -tool topology_state -args "{}" -expect-tools 1 -timeout 10
    if ($LASTEXITCODE -ne 0) {
        throw "launcher pre-restart session failed with exit code $LASTEXITCODE"
    }
    $Before = Get-PocStatus
    if ($Before.owner_count -lt 1) {
        throw "pre-restart owner registry is empty"
    }

    $Restart = & $Binary --poc-control graceful-restart | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or $Restart.ok -ne $true) {
        throw "graceful-restart failed: $($Restart | ConvertTo-Json -Compress)"
    }
    $After = Wait-PocReplacementReady -PreviousPid $Before.pid -PreviousGeneration $Before.daemon_generation
    Write-Host "  replacement pid=$($After.pid) generation=$($After.daemon_generation) owner_count=$($After.owner_count) handoff=$($After.handoff)"

    if ($After.owner_count -lt 1) {
        throw "post-restart owner registry is empty"
    }
    & $Launcher -binary $Binary -mode tool -tool topology_state -args "{}" -expect-tools 1 -timeout 10
    if ($LASTEXITCODE -ne 0) {
        throw "launcher post-restart session failed with exit code $LASTEXITCODE"
    }
}

if ([string]::IsNullOrWhiteSpace($Launcher)) {
    $LocalLauncher = Join-Path $RepoRoot "mcp-launcher.exe"
    if (Test-Path -LiteralPath $LocalLauncher) {
        $Launcher = $LocalLauncher
    } else {
        throw "mcp-launcher path is required. Pass -Launcher or set MCP_LAUNCHER."
    }
}
if (-not (Test-Path -LiteralPath $Launcher)) {
    throw "mcp-launcher not found: $Launcher"
}

Push-Location $RepoRoot
try {
    Invoke-NativeStep "build dummy topology binary" {
        go build -o $Binary .\experiments\current-topology-poc
    }

    Stop-PocDaemon

    Invoke-NativeStep "mcp-launcher tool smoke" {
        & $Launcher -binary $Binary -mode tool -tool topology_state -args "{}" -expect-tools 1 -timeout 10
    }

    Stop-PocDaemon

    if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Windows)) {
        Invoke-NativeStep "windows persist emulation with mcp-launcher session" {
            Invoke-WindowsPersistEmulation
        }
    } else {
        Invoke-NativeStep "mcp-launcher persist" {
            & $Launcher -binary $Binary -mode persist -ctl $ControlSocket -watch $WatchSeconds -expect-tools 1 -timeout 10
        }
    }

    Stop-PocDaemon

    if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Windows)) {
        Invoke-NativeStep "windows kill-reconnect emulation with mcp-launcher sessions" {
            Invoke-WindowsKillReconnectEmulation
        }
    } else {
        Invoke-NativeStep "mcp-launcher kill-reconnect" {
            & $Launcher -binary $Binary -mode kill-reconnect -ctl $ControlSocket -expect-tools 1 -timeout 10
        }
    }

    Stop-PocDaemon

    if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Windows)) {
        Invoke-NativeStep "windows phase2 restart emulation with mcp-launcher sessions" {
            Invoke-WindowsPhase2RestartEmulation
        }
    } else {
        Invoke-NativeStep "mcp-launcher phase2 restart smoke" {
            & $Launcher -binary $Binary -mode phase2 -ctl $ControlSocket -expect-tools 1 -timeout 10
        }
    }

    Stop-PocDaemon

    Invoke-NativeStep "stale generation token rejection" {
        & $Binary --poc-probe-stale-token
    }

    Stop-PocDaemon

    Invoke-NativeStep "owner registry semantics" {
        & $Binary --poc-probe-owner-registry
    }

    Stop-PocDaemon

    Invoke-NativeStep "zombie owner replacement" {
        & $Binary --poc-probe-zombie-owner
    }

    Stop-PocDaemon

    Invoke-NativeStep "live same-stdio reconnect" {
        & $Binary --poc-probe-live-reconnect
    }

    Stop-PocDaemon

    Invoke-NativeStep "concurrent in-flight reconnect" {
        & $Binary --poc-probe-inflight-reconnect
    }

    Stop-PocDaemon

    Invoke-NativeStep "out-of-order concurrent demux reconnect" {
        & $Binary --poc-probe-concurrent-demux
    }

    Stop-PocDaemon

    Invoke-NativeStep "refresh-token reconnect" {
        & $Binary --poc-probe-refresh-reconnect
    }

    Stop-PocDaemon

    Invoke-NativeStep "generation-aware handoff" {
        & $Binary --poc-probe-generation-handoff
    }

    Stop-PocDaemon

    Invoke-NativeStep "persistent idle reaper" {
        & $Binary --poc-probe-idle-reaper
    }

    Stop-PocDaemon

    Write-Host ""
    Write-Host "PASS current-topology PoC"
    $global:LASTEXITCODE = 0
} finally {
    Pop-Location
}
