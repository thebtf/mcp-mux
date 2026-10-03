param(
    [string]$Launcher = $env:MCP_LAUNCHER,
    [int]$WatchSeconds = 2,
    [ValidateRange(1, 2147483)]
    [int]$TimeoutSeconds = 60,
    [string]$RuntimeDir = $env:MCP_MUX_CURRENT_TOPOLOGY_DIR
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($RuntimeDir)) {
    $RuntimeDir = Join-Path $RepoRoot ".agent\current-topology-poc"
}
$RuntimeDir = [System.IO.Path]::GetFullPath($RuntimeDir)
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
    if (-not (Test-Path -LiteralPath $Binary)) {
        return
    }
    if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Windows)) {
        $escapedBinary = $Binary.Replace('\', '\\').Replace("'", "\'")
        $owned = @(Get-CimInstance Win32_Process -Filter "ExecutablePath = '$escapedBinary'" |
            Where-Object { $_.CommandLine -eq "$Binary --muxcore-daemon" -or $_.CommandLine -eq "`"$Binary`" --muxcore-daemon" })
        if ($owned.Count -eq 0) {
            return
        }
        if ($owned.Count -ne 1) {
            throw "multiple daemon identities at exact private binary path; resources retained: $Binary"
        }
        $Status = Get-PocStatus
        if ($Status.pid -ne $owned[0].ProcessId) {
            throw "private control PID does not match exact executable identity; resources retained: $Binary"
        }
    } else {
        if (-not (Test-Path -LiteralPath $ControlSocket)) {
            return
        }
        $Status = Get-PocStatus
    }

    $Daemon = [System.Diagnostics.Process]::GetProcessById([int]$Status.pid)
    $Control = [System.Diagnostics.Process]::new()
    try {
        if ([System.IO.Path]::GetFullPath($Daemon.Path) -ne $Binary) {
            throw "private daemon executable mismatch; resources retained: pid=$($Status.pid)"
        }
        $startUtc = $Daemon.StartTime.ToUniversalTime().ToString('o')
        Write-Host "  shutdown daemon pid=$($Daemon.Id) start_utc=$startUtc path=$Binary generation=$($Status.daemon_generation)"
        $Control.StartInfo.FileName = $Binary
        $Control.StartInfo.Arguments = '--poc-control shutdown'
        $Control.StartInfo.UseShellExecute = $false
        $Control.StartInfo.RedirectStandardOutput = $true
        $Control.StartInfo.RedirectStandardError = $true
        $clock = [System.Diagnostics.Stopwatch]::StartNew()
        $timeoutMs = $TimeoutSeconds * 1000
        [void]$Control.Start()
        $stdout = $Control.StandardOutput.ReadToEndAsync()
        $stderr = $Control.StandardError.ReadToEndAsync()
        if (-not $Control.WaitForExit($timeoutMs)) {
            throw "private shutdown timed out after ${TimeoutSeconds}s; control_pid=$($Control.Id), daemon_pid=$($Daemon.Id); process authority and resources retained"
        }
        $stdoutClosed = $stdout.Wait([int][Math]::Max(1, $timeoutMs - $clock.ElapsedMilliseconds))
        $stderrClosed = $stderr.Wait([int][Math]::Max(1, $timeoutMs - $clock.ElapsedMilliseconds))
        Write-Host "  shutdown control pid=$($Control.Id) exit=$($Control.ExitCode) stdout_closed=$stdoutClosed stderr_closed=$stderrClosed"
        if (-not $stdoutClosed -or -not $stderrClosed) {
            throw "private shutdown output did not close within ${TimeoutSeconds}s; process authority and resources retained"
        }
        if ($stdout.Result) { $stdout.Result | Out-Host }
        if ($stderr.Result) { $stderr.Result | Out-Host }
        if ($Control.ExitCode -ne 0 -or ($stdout.Result | ConvertFrom-Json).ok -ne $true) {
            throw "private shutdown failed with exit code $($Control.ExitCode); process authority and resources retained"
        }
        if (-not $Daemon.WaitForExit([int][Math]::Max(1, $timeoutMs - $clock.ElapsedMilliseconds))) {
            throw "captured private daemon survived shutdown deadline; pid=$($Daemon.Id) start_utc=$startUtc path=$Binary; resources retained"
        }
        Write-Host "  shutdown OS exit observed pid=$($Daemon.Id) elapsed_ms=$($clock.ElapsedMilliseconds)"
    } finally {
        $Control.Dispose()
        $Daemon.Dispose()
    }
    Remove-Item -LiteralPath $ControlSocket -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath (Join-Path $RuntimeDir 'owners.snapshot.json') -ErrorAction SilentlyContinue
    Get-ChildItem -LiteralPath $RuntimeDir -Filter '*.owner.sock' -ErrorAction SilentlyContinue |
        Remove-Item -ErrorAction SilentlyContinue
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

$pocFailed = $false
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

} catch {
    $pocFailed = $true
    throw
} finally {
    try {
        Stop-PocDaemon
    } catch {
        if (-not $pocFailed) { throw }
        Write-Warning "PoC cleanup failed; original failure preserved and resources retained: $($_.Exception.Message)"
    } finally {
        Pop-Location
    }
}

Write-Host ''
Write-Host 'PASS current-topology PoC'
$global:LASTEXITCODE = 0
