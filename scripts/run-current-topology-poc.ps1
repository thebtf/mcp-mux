param(
    [string]$Launcher = "D:\Dev\mcp-launcher\mcp-launcher.exe",
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
    Remove-Item -LiteralPath $ControlSocket -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath (Join-Path $RuntimeDir "owners.snapshot.json") -ErrorAction SilentlyContinue
    Get-ChildItem -LiteralPath $RuntimeDir -Filter "*.owner.sock" -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
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

    Invoke-NativeStep "mcp-launcher kill-reconnect" {
        & $Launcher -binary $Binary -mode kill-reconnect -ctl $ControlSocket -expect-tools 1 -timeout 10
    }

    Stop-PocDaemon

    Invoke-NativeStep "mcp-launcher phase2 restart smoke" {
        & $Launcher -binary $Binary -mode phase2 -ctl $ControlSocket -expect-tools 1 -timeout 10
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

    Write-Host ""
    Write-Host "PASS current-topology PoC"
} finally {
    Pop-Location
}
