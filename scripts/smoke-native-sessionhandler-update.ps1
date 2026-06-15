param(
    [string]$RunDir = "",
    [string]$EvidencePath = "",
    [int]$TimeoutSeconds = 45
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent $PSScriptRoot
$Stamp = Get-Date -Format "yyyyMMdd-HHmmss"
if ([string]::IsNullOrWhiteSpace($RunDir)) {
    $TempRoot = $env:MUXCORE_NATIVE_SMOKE_ROOT
    if ([string]::IsNullOrWhiteSpace($TempRoot)) {
        $TempRoot = Join-Path $RepoRoot ".t"
    }
    $RunDir = Join-Path $TempRoot "mn-$Stamp"
}
if ([string]::IsNullOrWhiteSpace($EvidencePath)) {
    $EvidencePath = Join-Path $RepoRoot ".agent\reports\native-sessionhandler-update-$Stamp.json"
}

$RunDir = [System.IO.Path]::GetFullPath($RunDir)
$EvidencePath = [System.IO.Path]::GetFullPath($EvidencePath)

$OldBinary = Join-Path $RunDir "old.exe"
$NewBinary = Join-Path $RunDir "new.exe"
$BaseDir = Join-Path $RunDir "r"
$GoCache = Join-Path $RunDir "gocache"
$FixtureLog = Join-Path $RunDir "fixture.log"
$ReportsDir = Split-Path -Parent $EvidencePath

New-Item -ItemType Directory -Force -Path $RunDir | Out-Null
New-Item -ItemType Directory -Force -Path $BaseDir | Out-Null
New-Item -ItemType Directory -Force -Path $GoCache | Out-Null
New-Item -ItemType Directory -Force -Path $ReportsDir | Out-Null
Remove-Item -LiteralPath $FixtureLog -ErrorAction SilentlyContinue

$script:NextID = 0

function ConvertTo-CompactJson {
    param([object]$Value)
    return ($Value | ConvertTo-Json -Depth 32 -Compress)
}

function Start-FixtureSession {
    param(
        [string]$Binary,
        [string]$Name
    )

    $psi = [System.Diagnostics.ProcessStartInfo]::new()
    $psi.FileName = $Binary
    $psi.WorkingDirectory = $RepoRoot
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    $psi.Environment["NATIVE_FIXTURE_BASE_DIR"] = $BaseDir
    $psi.Environment["NATIVE_FIXTURE_LOG_PATH"] = $FixtureLog
    $psi.Environment["TEMP"] = $RunDir
    $psi.Environment["TMP"] = $RunDir

    $proc = [System.Diagnostics.Process]::new()
    $proc.StartInfo = $psi
    if (-not $proc.Start()) {
        throw "failed to start $Name"
    }
    return $proc
}

function Stop-FixtureSession {
    param([System.Diagnostics.Process]$Proc)
    if ($null -eq $Proc) {
        return
    }
    try {
        if (-not $Proc.HasExited) {
            $Proc.StandardInput.Close()
            if (-not $Proc.WaitForExit(3000)) {
                $Proc.Kill()
                $Proc.WaitForExit(3000) | Out-Null
            }
        }
    } catch {
    }
}

function Invoke-Rpc {
    param(
        [System.Diagnostics.Process]$Proc,
        [string]$Method,
        [object]$Params = $null
    )

    $script:NextID += 1
    $id = $script:NextID
    $payload = [ordered]@{
        jsonrpc = "2.0"
        id = $id
        method = $Method
    }
    if ($null -ne $Params) {
        $payload.params = $Params
    }

    $Proc.StandardInput.WriteLine((ConvertTo-CompactJson $payload))
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $line = $Proc.StandardOutput.ReadLine()
        if ($null -eq $line) {
            $stderr = ""
            if ($Proc.HasExited) {
                $stderr = $Proc.StandardError.ReadToEnd()
            }
            throw "fixture session exited while waiting for response id=$id method=$Method stderr=$stderr"
        }
        if ([string]::IsNullOrWhiteSpace($line)) {
            continue
        }
        try {
            $msg = $line | ConvertFrom-Json
        } catch {
            $stderr = ""
            if ($Proc.HasExited) {
                $stderr = $Proc.StandardError.ReadToEnd()
            }
            throw "non-JSON stdout from fixture while waiting for id=$id method=$Method line=[$line] stderr=$stderr"
        }
        if ($null -ne $msg.id -and [int]$msg.id -eq $id) {
            if ($null -ne $msg.error) {
                throw "RPC $Method failed: $($msg.error | ConvertTo-Json -Depth 16 -Compress)"
            }
            return $msg
        }
    }
    throw "timeout waiting for response id=$id method=$Method"
}

function Invoke-FixtureStatusTool {
    param([System.Diagnostics.Process]$Proc)

    $resp = Invoke-Rpc -Proc $Proc -Method "tools/call" -Params @{
        name = "fixture_status"
        arguments = @{}
    }
    $text = $resp.result.content[0].text
    return ($text | ConvertFrom-Json)
}

function Invoke-FixtureCommand {
    param(
        [string]$Binary,
        [string[]]$Arguments
    )

    $oldBase = $env:NATIVE_FIXTURE_BASE_DIR
    $oldLog = $env:NATIVE_FIXTURE_LOG_PATH
    $oldTemp = $env:TEMP
    $oldTmp = $env:TMP
    $env:NATIVE_FIXTURE_BASE_DIR = $BaseDir
    $env:NATIVE_FIXTURE_LOG_PATH = $FixtureLog
    $env:TEMP = $RunDir
    $env:TMP = $RunDir
    try {
        $output = & $Binary @Arguments
        $exit = $LASTEXITCODE
    } finally {
        if ($null -eq $oldBase) {
            Remove-Item Env:NATIVE_FIXTURE_BASE_DIR -ErrorAction SilentlyContinue
        } else {
            $env:NATIVE_FIXTURE_BASE_DIR = $oldBase
        }
        if ($null -eq $oldLog) {
            Remove-Item Env:NATIVE_FIXTURE_LOG_PATH -ErrorAction SilentlyContinue
        } else {
            $env:NATIVE_FIXTURE_LOG_PATH = $oldLog
        }
        if ($null -eq $oldTemp) {
            Remove-Item Env:TEMP -ErrorAction SilentlyContinue
        } else {
            $env:TEMP = $oldTemp
        }
        if ($null -eq $oldTmp) {
            Remove-Item Env:TMP -ErrorAction SilentlyContinue
        } else {
            $env:TMP = $oldTmp
        }
    }
    return [pscustomobject]@{
        exit_code = $exit
        stdout = ($output -join "`n")
    }
}

function Assert-Equal {
    param(
        [object]$Actual,
        [object]$Expected,
        [string]$Message
    )
    if ("$Actual" -ne "$Expected") {
        throw "$Message; got '$Actual', want '$Expected'"
    }
}

function Assert-True {
    param(
        [bool]$Condition,
        [string]$Message
    )
    if (-not $Condition) {
        throw $Message
    }
}

function Invoke-GoBuildFixture {
    param(
        [string]$Version,
        [string]$Output
    )

    $oldGoCache = $env:GOCACHE
    $env:GOCACHE = $GoCache
    try {
        go build -trimpath -ldflags "-X main.productVersion=$Version" -o $Output .\experiments\native-sessionhandler-update-fixture
        if ($LASTEXITCODE -ne 0) {
            throw "go build fixture version=$Version failed with exit code $LASTEXITCODE"
        }
        if (-not (Test-Path -LiteralPath $Output)) {
            throw "go build fixture version=$Version did not create $Output"
        }
    } finally {
        if ($null -eq $oldGoCache) {
            Remove-Item Env:GOCACHE -ErrorAction SilentlyContinue
        } else {
            $env:GOCACHE = $oldGoCache
        }
    }
}

$started = Get-Date
$oldSession = $null
$freshSession = $null
$evidence = [ordered]@{
    timestamp = $started.ToUniversalTime().ToString("o")
    fixture = "native-sessionhandler-update-fixture"
    run_dir = $RunDir
    base_dir = $BaseDir
    fixture_log = $FixtureLog
    old_binary = $OldBinary
    new_binary = $NewBinary
    verdict = "FAIL"
    error = ""
}

Push-Location $RepoRoot
try {
    Invoke-GoBuildFixture -Version "old" -Output $OldBinary
    Invoke-GoBuildFixture -Version "new" -Output $NewBinary

    $oldSession = Start-FixtureSession -Binary $OldBinary -Name "old fixture session"
    Invoke-Rpc -Proc $oldSession -Method "initialize" -Params @{
        protocolVersion = "2025-11-25"
        capabilities = @{}
        clientInfo = @{ name = "native-smoke"; version = "1.0.0" }
    } | Out-Null
    Invoke-Rpc -Proc $oldSession -Method "tools/list" -Params @{} | Out-Null
    $oldStatus = Invoke-FixtureStatusTool -Proc $oldSession
    $evidence.old_status = $oldStatus

    Assert-Equal $oldStatus.product_version "old" "old session must report old version before update"
    Assert-Equal $oldStatus.engine_name "native-sessionhandler-update-fixture" "engine name mismatch before update"

    $oldGeneration = $oldStatus.status.daemon_generation
    $updateCommand = Invoke-FixtureCommand -Binary $OldBinary -Arguments @("--fixture-apply-successor", $NewBinary)
    $evidence.update_command = $updateCommand
    if ($updateCommand.exit_code -ne 0) {
        throw "fixture update command failed with exit code $($updateCommand.exit_code): $($updateCommand.stdout)"
    }
    $updateResult = $updateCommand.stdout | ConvertFrom-Json
    $evidence.update_result = $updateResult
    Assert-True ([bool]$updateResult.ok) "update result ok=false"
    Assert-True ([bool]$updateResult.result.ReplacementReady) "replacement daemon did not report ready"

    $postStatus = Invoke-FixtureStatusTool -Proc $oldSession
    $evidence.post_update_same_session_status = $postStatus
    Assert-Equal $postStatus.product_version "new" "same open session must report new version after update"
    Assert-Equal $postStatus.engine_name "native-sessionhandler-update-fixture" "engine name mismatch after update"
    Assert-True ("$($postStatus.status.daemon_generation)" -ne "$oldGeneration") "daemon_generation must change after update"
    Assert-True ([int]$postStatus.status.shim_reconnect_refreshed -ge 1) "same open session did not refresh reconnect token"
    Assert-Equal $postStatus.status.shim_reconnect_fallback_spawned 0 "same open session used fallback spawn"
    Assert-Equal $postStatus.status.shim_reconnect_gave_up 0 "shim reconnect gave up"

    $freshSession = Start-FixtureSession -Binary $OldBinary -Name "fresh fixture session"
    Invoke-Rpc -Proc $freshSession -Method "initialize" -Params @{
        protocolVersion = "2025-11-25"
        capabilities = @{}
        clientInfo = @{ name = "native-smoke-fresh"; version = "1.0.0" }
    } | Out-Null
    Invoke-Rpc -Proc $freshSession -Method "tools/list" -Params @{} | Out-Null
    $freshStatus = Invoke-FixtureStatusTool -Proc $freshSession
    $evidence.fresh_session_status = $freshStatus
    Assert-Equal $freshStatus.product_version "new" "fresh session through old entrypoint must bind to new daemon"
    Assert-Equal $freshStatus.status.shim_reconnect_fallback_spawned 0 "fresh session reports fallback spawn"
    Assert-Equal $freshStatus.status.shim_reconnect_gave_up 0 "fresh session reports reconnect give-up"

    $evidence.verdict = "PASS"
} catch {
    $evidence.error = $_.Exception.Message
    throw
} finally {
    Stop-FixtureSession $freshSession
    Stop-FixtureSession $oldSession
    try {
        Invoke-FixtureCommand -Binary $NewBinary -Arguments @("--fixture-shutdown") | Out-Null
    } catch {
    }
    $finished = Get-Date
    $evidence.finished = $finished.ToUniversalTime().ToString("o")
    $evidence.runtime_seconds = [math]::Round(($finished - $started).TotalSeconds, 3)
    if (Test-Path -LiteralPath $FixtureLog) {
        $evidence.fixture_log_content = Get-Content -LiteralPath $FixtureLog -Raw
    }
    $evidence | ConvertTo-Json -Depth 64 | Set-Content -LiteralPath $EvidencePath -Encoding UTF8
    Write-Output ($evidence | ConvertTo-Json -Depth 64)
    Pop-Location
}

if ($evidence.verdict -ne "PASS") {
    exit 1
}
