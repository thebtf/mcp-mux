# @critical
# @category: behavioral
# @features: [MCP-MUX-PROCESS-LIFECYCLE, MCP-MUX-LAUNCHER-UPGRADE]
# @dev_stand: optional
param(
    [Parameter(Mandatory = $true)]
    [string]$CandidateBinary,

    [string]$EvidencePath = "",

    [string]$RunDir = "",

    [int]$TimeoutSeconds = 60
)

$ErrorActionPreference = "Stop"
$ParallelSessions = 8
$RepoRoot = Split-Path -Parent $PSScriptRoot
$CandidateBinary = [System.IO.Path]::GetFullPath((Resolve-Path -LiteralPath $CandidateBinary).Path)
if (-not $IsWindows) {
    throw "process lifecycle smoke currently requires Windows process-tree evidence"
}
if ($TimeoutSeconds -lt 30) {
    throw "TimeoutSeconds must be >= 30"
}
if ([string]::IsNullOrWhiteSpace($RunDir)) {
    $RunDir = Join-Path ([System.IO.Path]::GetTempPath()) "mcp-mux-lifecycle-$PID-$([guid]::NewGuid().ToString('N'))"
}
$RunDir = [System.IO.Path]::GetFullPath($RunDir)
if (Test-Path -LiteralPath $RunDir) {
    throw "RunDir must be a new path owned by this smoke: $RunDir"
}
$Launcher = Join-Path $RunDir "mcp-mux.exe"
$Pending = $Launcher + "~"
$Fixture = Join-Path $RunDir "lifecycle-upstream.exe"
$FixtureRecords = Join-Path $RunDir "fixture-records"

$captured = [System.Collections.Generic.List[object]]::new()
$sessions = [System.Collections.Generic.List[hashtable]]::new()
$stderrTails = [System.Collections.Generic.List[string]]::new()
$evidence = [ordered]@{
    verdict = "UNKNOWN"
    timestamp_utc = (Get-Date).ToUniversalTime().ToString("o")
    ui_replay = "N/A - CLI-only stdio/process lifecycle acceptance"
    behavior_signal = [ordered]@{
        name = "user-task-completion-rate"
        target = "8/8 parallel isolated host transports converge to launcher-only dormancy, wake on demand, and survive an installed active-engine switch"
        method = "real Windows process identities plus JSON-RPC responses over the original host stdio pipes"
        critical = $true
    }
    parallel_sessions = $ParallelSessions
    run_dir = $RunDir
    candidate_sha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $CandidateBinary).Hash
    engine_v1 = ""
    engine_v2 = ""
    initial_engine_pids = @()
    wake_engine_pids = @()
    post_upgrade_engine_pids = @()
    initial = @()
    wake = @()
    post_upgrade = @()
    daemon_pids = @()
    captured_processes = @()
    stale_descendants = @()
    captured_survivors_before_cleanup = @()
    scoped_survivors_before_cleanup = @()
    captured_survivors_after_cleanup = @()
    scoped_survivors_after_cleanup = @()
    launcher_only_convergences = 0
    stderr_tail = @()
    error = ""
}

function Set-Arguments {
    param([System.Diagnostics.ProcessStartInfo]$StartInfo, [string[]]$Arguments)
    foreach ($argument in $Arguments) {
        [void]$StartInfo.ArgumentList.Add($argument)
    }
}

function Set-IsolatedEnvironment {
    param([System.Diagnostics.ProcessStartInfo]$StartInfo, [string]$ShimLog = "")
    $StartInfo.EnvironmentVariables["TEMP"] = $RunDir
    $StartInfo.EnvironmentVariables["TMP"] = $RunDir
    $StartInfo.EnvironmentVariables["MCP_MUX_LIFECYCLE_FIXTURE_ROOT"] = $FixtureRecords
    $StartInfo.EnvironmentVariables["MCPMUX_SHIM_IDLE_TIMEOUT"] = "5s"
    $StartInfo.EnvironmentVariables["MCPMUX_SHIM_DORMANT_GRACE"] = "500ms"
    $StartInfo.EnvironmentVariables["MCP_MUX_OWNER_IDLE"] = "5s"
    $StartInfo.EnvironmentVariables["MCP_MUX_IDLE_TIMEOUT"] = "1s"
    $StartInfo.EnvironmentVariables["MCPMUX_LAUNCHER_TRACE"] = "1"
    if (-not [string]::IsNullOrWhiteSpace($ShimLog)) {
        $StartInfo.EnvironmentVariables["MCP_MUX_SHIM_LOG"] = $ShimLog
    }
}

function New-StartInfo {
    param([string]$FileName, [string[]]$Arguments, [string]$ShimLog = "")
    $info = [System.Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $FileName
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    Set-Arguments -StartInfo $info -Arguments $Arguments
    Set-IsolatedEnvironment -StartInfo $info -ShimLog $ShimLog
    return $info
}

function Invoke-IsolatedProcess {
    param([string]$FileName, [string[]]$Arguments, [int]$TimeoutMs = 30000)
    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = New-StartInfo -FileName $FileName -Arguments $Arguments
    if (-not $process.Start()) {
        throw "failed to start $FileName"
    }
    $process.StandardInput.Close()
    $stdout = $process.StandardOutput.ReadToEndAsync()
    $stderr = $process.StandardError.ReadToEndAsync()
    if (-not $process.WaitForExit($TimeoutMs)) {
        $process.Kill($true)
        throw "timed out running $FileName $($Arguments -join ' ')"
    }
    return [pscustomobject]@{ exit_code = $process.ExitCode; stdout = $stdout.Result; stderr = $stderr.Result }
}

function Add-ExecutableMarker {
    param([string]$Path, [string]$Marker)
    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Append, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
    try {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes("`n$Marker`n")
        $stream.Write($bytes, 0, $bytes.Length)
        $stream.Flush($true)
    } finally {
        $stream.Dispose()
    }
}

function Resolve-ActiveEngine {
    $pointer = Join-Path $RunDir "mcp-mux.versions\active.txt"
    $raw = (Get-Content -Raw -LiteralPath $pointer).Trim()
    if ([System.IO.Path]::IsPathRooted($raw)) {
        return [System.IO.Path]::GetFullPath($raw)
    }
    return [System.IO.Path]::GetFullPath((Join-Path (Split-Path -Parent $pointer) $raw))
}

function Install-Engine {
    param([string]$Marker)
    Copy-Item -LiteralPath $CandidateBinary -Destination $Pending -Force
    Add-ExecutableMarker -Path $Pending -Marker $Marker
    $result = Invoke-IsolatedProcess -FileName $Launcher -Arguments @("upgrade") -TimeoutMs 30000
    if ($result.exit_code -ne 0) {
        throw "engine install failed ($($result.exit_code)): $($result.stderr)"
    }
    return Resolve-ActiveEngine
}

function Get-Identity {
    param([int]$ProcessId, [string]$Label, [int]$ParentProcessId = 0, [string]$ExpectedPath = "")
    $deadline = [DateTime]::UtcNow.AddSeconds(2)
    while ($true) {
        try {
            $process = [System.Diagnostics.Process]::GetProcessById($ProcessId)
            $path = [System.IO.Path]::GetFullPath($process.MainModule.FileName)
            $started = $process.StartTime.ToUniversalTime()
            $process.Dispose()
            if (-not [string]::IsNullOrWhiteSpace($ExpectedPath) -and $path -ine $ExpectedPath) {
                throw "process image is not ready"
            }
            break
        } catch {
            if ([DateTime]::UtcNow -ge $deadline) {
                throw "$Label pid=$ProcessId is not alive or has no executable identity"
            }
            Start-Sleep -Milliseconds 25
        }
    }
    $identity = [pscustomobject]@{
        label = $Label
        pid = $ProcessId
        parent_pid = $ParentProcessId
        executable = $path
        created_utc = $started.ToString("o")
        start_ticks = $started.Ticks
    }
    [void]$captured.Add($identity)
    return $identity
}

function Test-IdentityAlive {
    param([object]$Identity)
    try {
        $process = [System.Diagnostics.Process]::GetProcessById($Identity.pid)
        $path = [System.IO.Path]::GetFullPath($process.MainModule.FileName)
        $ticks = $process.StartTime.ToUniversalTime().Ticks
        $process.Dispose()
    } catch {
        return $false
    }
    return $path -ieq $Identity.executable -and $ticks -eq $Identity.start_ticks
}

function Stop-CapturedIdentity {
    param([object]$Identity)
    if (-not (Test-IdentityAlive -Identity $Identity)) {
        return
    }
    Stop-Process -Id $Identity.pid -Force -ErrorAction Stop
}

function Send-Message {
    param([hashtable]$Session, [hashtable]$Message)
    $Session.process.StandardInput.WriteLine(($Message | ConvertTo-Json -Compress -Depth 16))
    $Session.process.StandardInput.Flush()
}

function Wait-Response {
    param([hashtable]$Session, [int]$Id, [int]$TimeoutMs)
    $deadline = [DateTime]::UtcNow.AddMilliseconds($TimeoutMs)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($null -eq $Session.stdout_read) {
            $Session.stdout_read = $Session.process.StandardOutput.ReadLineAsync()
        }
        if (-not $Session.stdout_read.Wait(50)) {
            if ($Session.process.HasExited) {
                throw "launcher $($Session.index) exited with code $($Session.process.ExitCode) while waiting for id=$Id"
            }
            continue
        }
        $line = $Session.stdout_read.Result
        $Session.stdout_read = $null
        if ($null -eq $line) {
            throw "launcher $($Session.index) stdout closed while waiting for id=$Id"
        }
        if ([string]::IsNullOrWhiteSpace($line)) {
            continue
        }
        $message = $line | ConvertFrom-Json
        if ($null -ne $message.id -and [int]$message.id -eq $Id) {
            if ($null -ne $message.error) {
                throw "launcher $($Session.index) id=$Id returned error: $($message.error | ConvertTo-Json -Compress -Depth 8)"
            }
            return $message
        }
    }
    throw "timed out waiting for launcher $($Session.index) response id=$Id"
}

function Start-Sessions {
    param([string]$ExpectedEngine)
    for ($i = 0; $i -lt $ParallelSessions; $i++) {
        $process = [System.Diagnostics.Process]::new()
        $process.StartInfo = New-StartInfo -FileName $Launcher -Arguments @("--isolated", $Fixture) -ShimLog (Join-Path $RunDir "shim-$i.log")
        if (-not $process.Start()) {
            throw "failed to start launcher $i"
        }
        $session = @{
            index = $i
            process = $process
            stdout_read = $null
            stderr_task = $process.StandardError.ReadToEndAsync()
            launcher_identity = $null
        }
        $session.launcher_identity = Get-Identity -ProcessId $process.Id -Label "launcher-$i" -ExpectedPath $Launcher
        [void]$sessions.Add($session)
    }

    $engineIdentities = Wait-EngineChildren -ExpectedEngine $ExpectedEngine -Phase "initial"

    foreach ($session in $sessions) {
        Send-Message -Session $session -Message @{
            jsonrpc = "2.0"; id = 1; method = "initialize"
            params = @{ protocolVersion = "2025-11-25"; capabilities = @{}; clientInfo = @{ name = "lifecycle-smoke"; version = "1" } }
        }
    }
    foreach ($session in $sessions) {
        [void](Wait-Response -Session $session -Id 1 -TimeoutMs ($TimeoutSeconds * 1000))
        Send-Message -Session $session -Message @{ jsonrpc = "2.0"; method = "notifications/initialized"; params = @{} }
    }
    return @($engineIdentities)
}

function Invoke-ParallelProbe {
    param([int]$BaseId, [string]$Phase, [string]$ExpectedEngine = "")
    foreach ($session in $sessions) {
        Send-Message -Session $session -Message @{
            jsonrpc = "2.0"; id = ($BaseId + $session.index); method = "tools/call"
            params = @{ name = "lifecycle_probe"; arguments = @{} }
        }
    }
    $script:LastProbeEngines = if ([string]::IsNullOrWhiteSpace($ExpectedEngine)) {
        @()
    } else {
        @(Wait-EngineChildren -ExpectedEngine $ExpectedEngine -Phase $Phase)
    }
    $probes = @()
    foreach ($session in $sessions) {
        $id = $BaseId + $session.index
        $response = Wait-Response -Session $session -Id $id -TimeoutMs ($TimeoutSeconds * 1000)
        $payload = $response.result.content[0].text | ConvertFrom-Json
        $leader = Get-Identity -ProcessId ([int]$payload.leader_pid) -Label "$Phase-leader-$($session.index)" -ExpectedPath $Fixture
        $descendant = Get-Identity -ProcessId ([int]$payload.descendant_pid) -Label "$Phase-descendant-$($session.index)" -ExpectedPath $Fixture
        if ($leader.executable -ine $Fixture -or $descendant.executable -ine $Fixture) {
            throw "$Phase session $($session.index) escaped fixture binary identity"
        }
        $probes += [pscustomobject]@{
            session = $session.index
            leader_pid = $leader.pid
            descendant_pid = $descendant.pid
            leader_identity = $leader
            descendant_identity = $descendant
        }
    }
    if (@($probes.leader_pid | Sort-Object -Unique).Count -ne $ParallelSessions) {
        throw "$Phase did not create $ParallelSessions unique isolated leaders"
    }
    if (@($probes.descendant_pid | Sort-Object -Unique).Count -ne $ParallelSessions) {
        throw "$Phase did not create $ParallelSessions unique descendants"
    }
    $rows = @(Get-CimInstance Win32_Process -Filter "name = 'lifecycle-upstream.exe'" -ErrorAction SilentlyContinue)
    foreach ($probe in $probes) {
        $descendant = $rows | Where-Object { [int]$_.ProcessId -eq $probe.descendant_pid } | Select-Object -First 1
        if ($null -eq $descendant -or [int]$descendant.ParentProcessId -ne $probe.leader_pid) {
            throw "$Phase descendant $($probe.descendant_pid) is not a child of leader $($probe.leader_pid)"
        }
    }
    return @($probes)
}

function Wait-DaemonIdentity {
    param([string]$Label, [string]$ExpectedEngine)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $last = ""
    while ([DateTime]::UtcNow -lt $deadline) {
        $status = Invoke-IsolatedProcess -FileName $Launcher -Arguments @("status") -TimeoutMs 10000
        $last = $status.stdout + $status.stderr
        if ($status.exit_code -eq 0 -and $status.stdout.Trim().StartsWith("{")) {
            $parsed = $status.stdout | ConvertFrom-Json
            if ($parsed.daemon -eq $true -and [int]$parsed.pid -gt 0) {
                return Get-Identity -ProcessId ([int]$parsed.pid) -Label $Label -ExpectedPath $ExpectedEngine
            }
        }
        Start-Sleep -Milliseconds 200
    }
    throw "daemon identity unavailable: $last"
}

function Wait-EngineChildren {
    param([string]$ExpectedEngine, [string]$Phase)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $rows = @(Get-CimInstance Win32_Process -Filter "name = 'mcp-mux-engine.exe'" -ErrorAction SilentlyContinue | Where-Object {
            -not [string]::IsNullOrWhiteSpace([string]$_.ExecutablePath) -and
            [System.IO.Path]::GetFullPath([string]$_.ExecutablePath) -ieq $ExpectedEngine
        })
        $found = @()
        foreach ($session in $sessions) {
            $row = $rows | Where-Object {
                [int]$_.ParentProcessId -eq $session.launcher_identity.pid -and
                -not [string]::IsNullOrWhiteSpace([string]$_.ExecutablePath) -and
                [System.IO.Path]::GetFullPath([string]$_.ExecutablePath) -ieq $ExpectedEngine
            } | Select-Object -First 1
            if ($null -ne $row) {
                $found += $row
            }
        }
        if ($found.Count -eq $ParallelSessions) {
            $identities = @($found | ForEach-Object { Get-Identity -ProcessId ([int]$_.ProcessId) -Label "$Phase-engine" -ParentProcessId ([int]$_.ParentProcessId) -ExpectedPath $ExpectedEngine })
            if (@($identities.pid | Sort-Object -Unique).Count -ne $ParallelSessions) {
                throw "$Phase did not create $ParallelSessions distinct isolated engine children"
            }
            return $identities
        }
        Start-Sleep -Milliseconds 200
    }
    $diagnostic = @(Get-ScopedProcesses | ForEach-Object { "$($_.Name):$($_.ProcessId):parent=$($_.ParentProcessId):$($_.ExecutablePath)" })
    throw "$Phase did not select engine $ExpectedEngine for all $ParallelSessions launchers; scoped=$($diagnostic -join ',')"
}

function Get-ScopedProcesses {
    $prefix = $RunDir.TrimEnd('\') + '\*'
    $rows = @()
    foreach ($name in @('mcp-mux.exe', 'mcp-mux-engine.exe', 'lifecycle-upstream.exe')) {
        $rows += @(Get-CimInstance Win32_Process -Filter "name = '$name'" -ErrorAction SilentlyContinue)
    }
    return @($rows | Where-Object {
        -not [string]::IsNullOrWhiteSpace([string]$_.ExecutablePath) -and
        [System.IO.Path]::GetFullPath([string]$_.ExecutablePath) -like $prefix
    })
}

function Get-CapturedSurvivors {
    return @($captured | Where-Object { Test-IdentityAlive -Identity $_ } | Select-Object label, pid, parent_pid, executable, created_utc)
}

function Get-ScopedSurvivors {
    return @(Get-ScopedProcesses | ForEach-Object {
        [pscustomobject]@{
            name = $_.Name
            pid = [int]$_.ProcessId
            parent_pid = [int]$_.ParentProcessId
            executable = [System.IO.Path]::GetFullPath([string]$_.ExecutablePath)
        }
    })
}

function Add-CleanupError {
    param([string]$Message)
    $evidence.verdict = "FAIL"
    if ([string]::IsNullOrWhiteSpace($evidence.error)) {
        $evidence.error = $Message
    } else {
        $evidence.error += "; cleanup: $Message"
    }
}

function Wait-LauncherOnly {
    param([object[]]$MustExit, [string]$Phase)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $alive = @($MustExit | Where-Object { Test-IdentityAlive -Identity $_ })
        $launchersAlive = @($sessions | Where-Object { Test-IdentityAlive -Identity $_.launcher_identity })
        if ($alive.Count -eq 0 -and $launchersAlive.Count -eq $ParallelSessions) {
            $scoped = Get-ScopedProcesses
            $scopedPids = @($scoped.ProcessId | ForEach-Object { [int]$_ } | Sort-Object)
            $launcherPids = @($sessions.launcher_identity.pid | Sort-Object)
            if (($scopedPids -join ',') -eq ($launcherPids -join ',')) {
                $evidence.launcher_only_convergences++
                return
            }
        }
        Start-Sleep -Milliseconds 250
    }
    $remaining = @($MustExit | Where-Object { Test-IdentityAlive -Identity $_ } | ForEach-Object { "$($_.label):$($_.pid)" })
    $scoped = @(Get-ScopedProcesses | ForEach-Object { "$($_.Name):$($_.ProcessId):$($_.ExecutablePath)" })
    throw "$Phase failed launcher-only convergence; captured_alive=$($remaining -join ','); scoped=$($scoped -join ',')"
}

function Assert-UniqueFromPrevious {
    param([object[]]$Current, [object[]]$Previous, [string]$Phase)
    $old = @{}
    foreach ($probe in $Previous) {
        $old["$($probe.leader_identity.executable)|$($probe.leader_identity.start_ticks)"] = $true
        $old["$($probe.descendant_identity.executable)|$($probe.descendant_identity.start_ticks)"] = $true
    }
    foreach ($probe in $Current) {
        $leader = "$($probe.leader_identity.executable)|$($probe.leader_identity.start_ticks)"
        $descendant = "$($probe.descendant_identity.executable)|$($probe.descendant_identity.start_ticks)"
        if ($old.ContainsKey($leader) -or $old.ContainsKey($descendant)) {
            throw "$Phase reused a stale process identity"
        }
    }
}

New-Item -ItemType Directory -Force -Path $RunDir, $FixtureRecords | Out-Null
try {
    Copy-Item -LiteralPath $CandidateBinary -Destination $Launcher -Force
    $oldGoCache = $env:GOCACHE
    $oldGoTmp = $env:GOTMPDIR
    $oldGo111Module = $env:GO111MODULE
    try {
        $env:GOCACHE = Join-Path $RunDir "go-cache"
        $env:GOTMPDIR = Join-Path $RunDir "go-tmp"
        $env:GO111MODULE = "off"
        New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR | Out-Null
        & go build -trimpath -o $Fixture .\scripts\lifecycle-smoke-upstream\main.go
        if ($LASTEXITCODE -ne 0) {
            throw "fixture build exited with code $LASTEXITCODE"
        }
    } finally {
        $env:GOCACHE = $oldGoCache
        $env:GOTMPDIR = $oldGoTmp
        $env:GO111MODULE = $oldGo111Module
    }

    $engineV1 = Install-Engine -Marker "lifecycle-engine-v1"
    $evidence.engine_v1 = $engineV1
    $initialEngines = @(Start-Sessions -ExpectedEngine $engineV1)
    $evidence.initial_engine_pids = @($initialEngines.pid)
    $initial = Invoke-ParallelProbe -BaseId 100 -Phase "initial"
    $evidence.initial = @($initial | Select-Object session, leader_pid, descendant_pid)
    $initialDaemon = Wait-DaemonIdentity -Label "initial-daemon" -ExpectedEngine $engineV1
    $evidence.daemon_pids += $initialDaemon.pid

    $initialMustExit = @($initialEngines) + @($initialDaemon) + @($initial.leader_identity) + @($initial.descendant_identity)
    Wait-LauncherOnly -MustExit $initialMustExit -Phase "initial idle"

    $wake = Invoke-ParallelProbe -BaseId 200 -Phase "wake" -ExpectedEngine $engineV1
    $wakeEngines = @($script:LastProbeEngines)
    $evidence.wake_engine_pids = @($wakeEngines.pid)
    Assert-UniqueFromPrevious -Current $wake -Previous $initial -Phase "demand wake"
    $wakeDaemon = Wait-DaemonIdentity -Label "wake-daemon" -ExpectedEngine $engineV1
    $evidence.daemon_pids += $wakeDaemon.pid
    $evidence.wake = @($wake | Select-Object session, leader_pid, descendant_pid)

    $engineV2 = Install-Engine -Marker "lifecycle-engine-v2"
    $evidence.engine_v2 = $engineV2
    if ($engineV2 -ieq $engineV1) {
        throw "active engine pointer did not change across installed candidates"
    }
    $postUpgrade = Invoke-ParallelProbe -BaseId 300 -Phase "post-upgrade" -ExpectedEngine $engineV2
    $upgradeEngines = @($script:LastProbeEngines)
    $evidence.post_upgrade_engine_pids = @($upgradeEngines.pid)
    $evidence.post_upgrade = @($postUpgrade | Select-Object session, leader_pid, descendant_pid)

    $allMustExit = @($wakeEngines) + @($upgradeEngines) + @($wakeDaemon) + @($wake.leader_identity) + @($wake.descendant_identity) + @($postUpgrade.leader_identity) + @($postUpgrade.descendant_identity)
    Wait-LauncherOnly -MustExit $allMustExit -Phase "post-upgrade idle"

    foreach ($session in $sessions) {
        $session.process.StandardInput.Close()
    }
    foreach ($session in $sessions) {
        if (-not $session.process.WaitForExit(5000) -or $session.process.ExitCode -ne 0) {
            throw "launcher $($session.index) did not exit cleanly on host EOF"
        }
    }
    if ((Get-ScopedProcesses).Count -ne 0) {
        throw "run-scoped processes remained after host EOF"
    }

    $stale = @(Get-CapturedSurvivors)
    $evidence.stale_descendants = $stale
    if ($stale.Count -ne 0) {
        throw "captured process identities remained alive after convergence"
    }
    $evidence.verdict = "PASS"
} catch {
    $evidence.verdict = "FAIL"
    $evidence.error = $_.Exception.Message
} finally {
    foreach ($session in $sessions) {
        try { $session.process.StandardInput.Close() } catch {}
    }
    $evidence.captured_survivors_before_cleanup = @(Get-CapturedSurvivors)
    $evidence.scoped_survivors_before_cleanup = @(Get-ScopedSurvivors)
    foreach ($row in @($evidence.scoped_survivors_before_cleanup)) {
        try {
            [void](Get-Identity -ProcessId $row.pid -Label "cleanup-scoped" -ExpectedPath $row.executable)
        } catch {
            if (@(Get-ScopedSurvivors | Where-Object { $_.pid -eq $row.pid }).Count -ne 0) {
                Add-CleanupError "failed to capture scoped process $($row.pid): $($_.Exception.Message)"
            }
        }
    }
    foreach ($identity in @($captured)) {
        try {
            Stop-CapturedIdentity -Identity $identity
        } catch {
            Add-CleanupError "failed to stop captured process $($identity.label):$($identity.pid): $($_.Exception.Message)"
        }
    }
    Start-Sleep -Milliseconds 100
    $evidence.captured_survivors_after_cleanup = @(Get-CapturedSurvivors)
    $evidence.scoped_survivors_after_cleanup = @(Get-ScopedSurvivors)
    $evidence.stale_descendants = @($evidence.captured_survivors_after_cleanup)
    if ($evidence.captured_survivors_after_cleanup.Count -ne 0 -or $evidence.scoped_survivors_after_cleanup.Count -ne 0) {
        Add-CleanupError "cleanup left captured=$($evidence.captured_survivors_after_cleanup.Count) scoped=$($evidence.scoped_survivors_after_cleanup.Count) run-scoped processes"
    }
    foreach ($session in $sessions) {
        if ($null -ne $session.stderr_task) {
            [void]$session.stderr_task.Wait(2000)
            if ($session.stderr_task.IsCompletedSuccessfully) {
                $text = $session.stderr_task.Result
                if (-not [string]::IsNullOrWhiteSpace($text)) {
                    [void]$stderrTails.Add((($text -split "`r`n|`n|`r" | Select-Object -Last 30) -join "`n"))
                }
            }
        }
    }
    foreach ($log in @(Get-ChildItem -LiteralPath $RunDir -Filter "*.log" -File -ErrorAction SilentlyContinue)) {
        $text = Get-Content -Raw -LiteralPath $log.FullName
        if (-not [string]::IsNullOrWhiteSpace($text)) {
            [void]$stderrTails.Add(("$($log.Name):`n" + (($text -split "`r`n|`n|`r" | Select-Object -Last 40) -join "`n")))
        }
    }
    $evidence.captured_processes = @($captured | Select-Object label, pid, parent_pid, executable, created_utc)
    $evidence.stderr_tail = @($stderrTails)
    Remove-Item -LiteralPath $RunDir -Recurse -Force -ErrorAction SilentlyContinue
}

$json = [pscustomobject]$evidence | ConvertTo-Json -Depth 32
if (-not [string]::IsNullOrWhiteSpace($EvidencePath)) {
    $EvidencePath = [System.IO.Path]::GetFullPath($EvidencePath)
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $EvidencePath) | Out-Null
    Set-Content -LiteralPath $EvidencePath -Value $json -Encoding UTF8
}
Write-Output $json
if ($evidence.verdict -ne "PASS") {
    exit 1
}
