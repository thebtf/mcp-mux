param(
    [Parameter(Mandatory = $true)]
    [string]$Binary,

    [string[]]$UpstreamCommand = @("uvx", "mcp-server-time"),

    [string]$EvidencePath = "",

    [string]$RuntimeDir = "",

    [int]$TimeoutSeconds = 30,

    [switch]$SkipUpstreamWarmup
)

$ErrorActionPreference = "Stop"

$FailureString = "connection closed: initialize response"
$RepoRoot = Split-Path -Parent $PSScriptRoot
$ProductionBinary = [System.IO.Path]::GetFullPath((Join-Path $RepoRoot "mcp-mux.exe"))
$ResolvedBinary = [System.IO.Path]::GetFullPath((Resolve-Path -LiteralPath $Binary).Path)
$TempRoot = if ([string]::IsNullOrWhiteSpace($env:TEMP)) { [System.IO.Path]::GetTempPath() } else { $env:TEMP }

if ($ResolvedBinary -ieq $ProductionBinary) {
    throw "Refusing to smoke-test the production binary path: $ResolvedBinary"
}

if ($UpstreamCommand.Count -lt 1 -or [string]::IsNullOrWhiteSpace($UpstreamCommand[0])) {
    throw "UpstreamCommand must contain at least one executable name"
}

if ($TimeoutSeconds -lt 5) {
    throw "TimeoutSeconds must be >= 5"
}

if ([string]::IsNullOrWhiteSpace($RuntimeDir)) {
    $stamp = Get-Date -Format "yyyyMMdd-HHmmss"
    $RuntimeDir = Join-Path $TempRoot "mcp-mux-smoke-$stamp-$PID"
}
$RuntimeDir = [System.IO.Path]::GetFullPath($RuntimeDir)
New-Item -ItemType Directory -Force -Path $RuntimeDir | Out-Null

$ShimLogPath = Join-Path $RuntimeDir "shim.log"
$DaemonLogPath = Join-Path $RuntimeDir "mcp-muxd-debug.log"
$UvCacheDir = Join-Path $TempRoot "mcp-mux-smoke-uv-cache"
$UvToolDir = Join-Path $RuntimeDir "uv-tools"
New-Item -ItemType Directory -Force -Path $UvCacheDir | Out-Null
New-Item -ItemType Directory -Force -Path $UvToolDir | Out-Null

function ConvertTo-NativeArgument {
    param([string]$Argument)

    if ($null -eq $Argument -or $Argument -eq "") {
        return '""'
    }
    if ($Argument -notmatch '[\s"]') {
        return $Argument
    }
    return '"' + ($Argument -replace '\\', '\\' -replace '"', '\"') + '"'
}

function Set-ProcessArguments {
    param(
        [System.Diagnostics.ProcessStartInfo]$StartInfo,
        [string[]]$Arguments
    )

    $hasArgumentList = $StartInfo.PSObject.Properties.Name -contains "ArgumentList"
    if ($hasArgumentList -and $null -ne $StartInfo.ArgumentList) {
        foreach ($arg in $Arguments) {
            [void]$StartInfo.ArgumentList.Add($arg)
        }
        return
    }

    $StartInfo.Arguments = ($Arguments | ForEach-Object { ConvertTo-NativeArgument $_ }) -join " "
}

function Set-SmokeEnvironment {
    param([System.Diagnostics.ProcessStartInfo]$StartInfo)

    $StartInfo.EnvironmentVariables["TEMP"] = $RuntimeDir
    $StartInfo.EnvironmentVariables["TMP"] = $RuntimeDir
    $StartInfo.EnvironmentVariables["MCP_MUX_SHIM_LOG"] = $ShimLogPath
    $StartInfo.EnvironmentVariables["MCP_MUX_OWNER_IDLE"] = "30s"
    $StartInfo.EnvironmentVariables["MCP_MUX_IDLE_TIMEOUT"] = "30s"
    $StartInfo.EnvironmentVariables["UV_CACHE_DIR"] = $UvCacheDir
    $StartInfo.EnvironmentVariables["UV_TOOL_DIR"] = $UvToolDir
}

function New-SmokeProcessStartInfo {
    param(
        [string]$FileName,
        [string[]]$Arguments
    )

    $psi = [System.Diagnostics.ProcessStartInfo]::new()
    $psi.FileName = $FileName
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    Set-ProcessArguments -StartInfo $psi -Arguments $Arguments
    Set-SmokeEnvironment -StartInfo $psi
    return $psi
}

function Invoke-SmokeNative {
    param(
        [string]$FileName,
        [string[]]$Arguments,
        [int]$TimeoutMs
    )

    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = New-SmokeProcessStartInfo -FileName $FileName -Arguments $Arguments
    [void]$process.Start()
    $stdoutTask = $process.StandardOutput.ReadToEndAsync()
    $stderrTask = $process.StandardError.ReadToEndAsync()
    if (-not $process.WaitForExit($TimeoutMs)) {
        try {
            $process.Kill($true)
        } catch {
            $process.Kill()
        }
        throw "Timed out running $FileName $($Arguments -join ' ')"
    }
    return [pscustomobject]@{
        exit_code = $process.ExitCode
        stdout    = $stdoutTask.Result
        stderr    = $stderrTask.Result
    }
}

function Read-TextIfExists {
    param([string]$Path)

    if (Test-Path -LiteralPath $Path) {
        return Get-Content -Raw -LiteralPath $Path
    }
    return ""
}

function Select-LastLines {
    param(
        [string]$Text,
        [int]$Count = 120
    )

    if ([string]::IsNullOrEmpty($Text)) {
        return @()
    }
    $lines = $Text -split "`r`n|`n|`r"
    if ($lines.Count -le $Count) {
        return @($lines)
    }
    return @($lines | Select-Object -Last $Count)
}

function Test-ContainsFailureString {
    param([string[]]$Texts)

    foreach ($text in $Texts) {
        if (-not [string]::IsNullOrEmpty($text) -and $text.Contains($FailureString)) {
            return $true
        }
    }
    return $false
}

function ConvertTo-SmokeJson {
    param(
        [object]$Value,
        [switch]$Compress
    )

    if ($Compress) {
        return $Value | ConvertTo-Json -Depth 64 -Compress
    }
    return $Value | ConvertTo-Json -Depth 64
}

function Write-SmokeEvidence {
    param([hashtable]$Evidence)

    $json = ConvertTo-SmokeJson ([pscustomobject]$Evidence)
    if (-not [string]::IsNullOrWhiteSpace($EvidencePath)) {
        $evidenceFullPath = [System.IO.Path]::GetFullPath($EvidencePath)
        $evidenceDir = Split-Path -Parent $evidenceFullPath
        if (-not [string]::IsNullOrWhiteSpace($evidenceDir)) {
            New-Item -ItemType Directory -Force -Path $evidenceDir | Out-Null
        }
        Set-Content -LiteralPath $evidenceFullPath -Value $json -Encoding UTF8
    }
    Write-Output $json
}

function Send-McpLine {
    param(
        [System.Diagnostics.Process]$Process,
        [hashtable]$Request
    )

    $line = ConvertTo-SmokeJson ([pscustomobject]$Request) -Compress
    $Process.StandardInput.WriteLine($line)
    $Process.StandardInput.Flush()
}

function Convert-McpResponseLine {
    param([string]$Line)

    try {
        return $Line | ConvertFrom-Json
    } catch {
        throw "Malformed JSON-RPC stdout line: $Line"
    }
}

function Read-StdoutLine {
    param(
        [System.Diagnostics.Process]$Process,
        [int]$PollMs
    )

    if ($null -eq $script:PendingStdoutRead) {
        $script:PendingStdoutRead = $Process.StandardOutput.ReadLineAsync()
    }
    if ($script:PendingStdoutRead.Wait($PollMs)) {
        $line = $script:PendingStdoutRead.Result
        $script:PendingStdoutRead = $null
        return $line
    }
    return $null
}

function Wait-McpResponse {
    param(
        [System.Diagnostics.Process]$Process,
        [System.Collections.Generic.List[string]]$ObservedStdout,
        [object]$ExpectedID,
        [int]$TimeoutMs
    )

    $deadline = [DateTime]::UtcNow.AddMilliseconds($TimeoutMs)
    while ([DateTime]::UtcNow -lt $deadline) {
        $line = Read-StdoutLine -Process $Process -PollMs 50
        if ($null -eq $line) {
            if ($Process.HasExited) {
                throw "Process exited while waiting for JSON-RPC response id=$ExpectedID (exit=$($Process.ExitCode))"
            }
            continue
        }
        if ([string]::IsNullOrWhiteSpace($line)) {
            continue
        }
        [void]$ObservedStdout.Add($line)
        $message = Convert-McpResponseLine -Line $line
        if ($null -ne $message.id -and "$($message.id)" -eq "$ExpectedID") {
            if ($null -ne $message.error) {
                throw "JSON-RPC response id=$ExpectedID returned error: $($message.error | ConvertTo-Json -Compress -Depth 16)"
            }
            return $message
        }
    }

    throw "Timed out waiting for JSON-RPC response id=$ExpectedID"
}

function Drain-Stdout {
    param(
        [System.Diagnostics.Process]$Process,
        [System.Collections.Generic.List[string]]$Observed
    )

    while ($true) {
        $line = Read-StdoutLine -Process $Process -PollMs 10
        if ($null -eq $line) {
            return
        }
        if (-not [string]::IsNullOrWhiteSpace($line)) {
            [void]$Observed.Add($line)
        }
    }
}

function Stop-SmokeProcess {
    param(
        [System.Diagnostics.Process]$Process,
        [System.Threading.Tasks.Task[string]]$StderrTask
    )

    if ($null -eq $Process) {
        return ""
    }

    try {
        $Process.StandardInput.Close()
    } catch {
    }
    if (-not $Process.HasExited) {
        if (-not $Process.WaitForExit(5000)) {
            try {
                $Process.Kill($true)
            } catch {
                $Process.Kill()
            }
            $Process.WaitForExit(5000) | Out-Null
        }
    }
    if ($null -ne $StderrTask) {
        return $StderrTask.Result
    }
    return ""
}

function Get-ToolNames {
    param([object]$ToolsListResponse)

    $names = New-Object System.Collections.Generic.List[string]
    if ($null -ne $ToolsListResponse.result -and $null -ne $ToolsListResponse.result.tools) {
        foreach ($tool in $ToolsListResponse.result.tools) {
            if ($null -ne $tool.name) {
                [void]$names.Add([string]$tool.name)
            }
        }
    }
    return @($names)
}

function Test-DefaultTimeUpstream {
    if ($UpstreamCommand.Count -ne 2) {
        return $false
    }
    return $UpstreamCommand[0] -ieq "uvx" -and $UpstreamCommand[1] -eq "mcp-server-time"
}

function Invoke-DefaultTimeWarmup {
    $oldTemp = $env:TEMP
    $oldTmp = $env:TMP
    $oldUvCache = $env:UV_CACHE_DIR
    $oldUvTool = $env:UV_TOOL_DIR
    $attempts = @()
    try {
        $env:TEMP = $RuntimeDir
        $env:TMP = $RuntimeDir
        $env:UV_CACHE_DIR = $UvCacheDir
        $env:UV_TOOL_DIR = $UvToolDir
        for ($attempt = 1; $attempt -le 3; $attempt++) {
            $output = & $UpstreamCommand[0] $UpstreamCommand[1] "--help" 2>&1
            $exitCode = $LASTEXITCODE
            $lines = @($output | ForEach-Object { "$_" })
            $attempts += [pscustomobject]@{
                attempt   = $attempt
                exit_code = $exitCode
                output    = $lines
            }
            if ($exitCode -eq 0) {
                return [pscustomobject]@{
                    exit_code = 0
                    attempts  = @($attempts)
                    output    = $lines
                }
            }
            if (($lines -join "`n") -notmatch "failed to persist temporary file|Отказано в доступе|Access is denied") {
                break
            }
            Start-Sleep -Milliseconds (500 * $attempt)
        }

        $last = $attempts[$attempts.Count - 1]
        return [pscustomobject]@{
            exit_code = $last.exit_code
            attempts  = @($attempts)
            output    = @($last.output)
        }
    } finally {
        $env:TEMP = $oldTemp
        $env:TMP = $oldTmp
        $env:UV_CACHE_DIR = $oldUvCache
        $env:UV_TOOL_DIR = $oldUvTool
    }
}

$evidence = [ordered]@{
    verdict                    = "UNKNOWN"
    timestamp_utc              = (Get-Date).ToUniversalTime().ToString("o")
    binary_path                = $ResolvedBinary
    production_binary_path     = $ProductionBinary
    production_binary_used     = $false
    binary_hash_sha256         = (Get-FileHash -Algorithm SHA256 -LiteralPath $ResolvedBinary).Hash
    binary_size                = (Get-Item -LiteralPath $ResolvedBinary).Length
    runtime_dir                = $RuntimeDir
    uv_cache_dir               = $UvCacheDir
    uv_tool_dir                = $UvToolDir
    upstream_command           = @($UpstreamCommand)
    upstream_warmup            = $null
    failure_string             = $FailureString
    failure_string_present     = $null
    initialize                 = $null
    tools_list                 = $null
    reconnect_probe            = $null
    status_before_reconnect    = $null
    status_after_reconnect     = $null
    stdout_lines               = @()
    stderr_lines               = @()
    shim_log_tail              = @()
    daemon_log_tail            = @()
}

$process = $null
$stderrTask = $null
$processStopped = $false
$script:PendingStdoutRead = $null
$observedStdout = [System.Collections.Generic.List[string]]::new()
$observedStderr = [System.Collections.Generic.List[string]]::new()

try {
    if (-not $SkipUpstreamWarmup -and (Test-DefaultTimeUpstream)) {
        $warmup = Invoke-DefaultTimeWarmup
        $evidence.upstream_warmup = [ordered]@{
            command   = @($UpstreamCommand[0], $UpstreamCommand[1], "--help")
            exit_code = $warmup.exit_code
            attempts  = @($warmup.attempts)
            output    = @($warmup.output)
        }
        if ($warmup.exit_code -ne 0) {
            throw "default time upstream warmup failed: $($warmup.output -join "`n")"
        }
    } else {
        $evidence.upstream_warmup = [ordered]@{
            skipped = $true
            reason  = if ($SkipUpstreamWarmup) { "SkipUpstreamWarmup" } else { "non-default-upstream" }
        }
    }

    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = New-SmokeProcessStartInfo -FileName $ResolvedBinary -Arguments $UpstreamCommand
    [void]$process.Start()
    $stderrTask = $process.StandardError.ReadToEndAsync()

    Send-McpLine -Process $process -Request @{
        jsonrpc = "2.0"
        id      = 1
        method  = "initialize"
        params  = @{
            protocolVersion = "2025-11-25"
            capabilities    = @{}
            clientInfo      = @{
                name    = "mcp-mux-smoke"
                version = "1.0.0"
            }
        }
    }
    $initResponse = Wait-McpResponse -Process $process -ObservedStdout $observedStdout -ExpectedID 1 -TimeoutMs ($TimeoutSeconds * 1000)
    if ($null -eq $initResponse.result -or [string]::IsNullOrWhiteSpace([string]$initResponse.result.protocolVersion)) {
        throw "Initialize response is missing result.protocolVersion"
    }
    $evidence.initialize = [ordered]@{
        ok               = $true
        protocol_version = [string]$initResponse.result.protocolVersion
        server_name      = [string]$initResponse.result.serverInfo.name
        server_version   = [string]$initResponse.result.serverInfo.version
    }

    Send-McpLine -Process $process -Request @{
        jsonrpc = "2.0"
        method  = "notifications/initialized"
        params  = @{}
    }

    Send-McpLine -Process $process -Request @{
        jsonrpc = "2.0"
        id      = 2
        method  = "tools/list"
        params  = @{}
    }
    $toolsResponse = Wait-McpResponse -Process $process -ObservedStdout $observedStdout -ExpectedID 2 -TimeoutMs ($TimeoutSeconds * 1000)
    $toolNames = Get-ToolNames -ToolsListResponse $toolsResponse
    $evidence.tools_list = [ordered]@{
        ok         = $true
        tool_count = $toolNames.Count
        tool_names = @($toolNames)
    }

    $statusBefore = Invoke-SmokeNative -FileName $ResolvedBinary -Arguments @("status") -TimeoutMs 10000
    if ($statusBefore.exit_code -ne 0) {
        throw "status before reconnect failed: $($statusBefore.stderr)"
    }
    $evidence.status_before_reconnect = $statusBefore.stdout | ConvertFrom-Json

    $stopResult = Invoke-SmokeNative -FileName $ResolvedBinary -Arguments @("stop", "--force") -TimeoutMs 15000
    $evidence.reconnect_probe = [ordered]@{
        stop_exit_code = $stopResult.exit_code
        stop_stdout    = $stopResult.stdout
        stop_stderr    = $stopResult.stderr
    }
    if ($stopResult.exit_code -ne 0) {
        throw "isolated stop --force failed: $($stopResult.stderr)"
    }

    Send-McpLine -Process $process -Request @{
        jsonrpc = "2.0"
        id      = 3
        method  = "tools/list"
        params  = @{}
    }
    $postReconnectTools = Wait-McpResponse -Process $process -ObservedStdout $observedStdout -ExpectedID 3 -TimeoutMs ($TimeoutSeconds * 1000)
    $postReconnectToolNames = Get-ToolNames -ToolsListResponse $postReconnectTools
    $evidence.reconnect_probe.post_reconnect_tools_list = [ordered]@{
        ok         = $true
        tool_count = $postReconnectToolNames.Count
        tool_names = @($postReconnectToolNames)
    }

    Start-Sleep -Milliseconds 300
    $statusAfter = Invoke-SmokeNative -FileName $ResolvedBinary -Arguments @("status") -TimeoutMs 10000
    if ($statusAfter.exit_code -ne 0) {
        throw "status after reconnect failed: $($statusAfter.stderr)"
    }
    $evidence.status_after_reconnect = $statusAfter.stdout | ConvertFrom-Json

    Drain-Stdout -Process $process -Observed $observedStdout
    $stderrText = Stop-SmokeProcess -Process $process -StderrTask $stderrTask
    $processStopped = $true
    foreach ($line in ($stderrText -split "`r`n|`n|`r")) {
        if (-not [string]::IsNullOrWhiteSpace($line)) {
            [void]$observedStderr.Add($line)
        }
    }
    $shimLog = Read-TextIfExists -Path $ShimLogPath
    $daemonLog = Read-TextIfExists -Path $DaemonLogPath
    $allText = @(
        ($observedStdout -join "`n"),
        ($observedStderr -join "`n"),
        $shimLog,
        $daemonLog,
        $statusBefore.stdout,
        $statusBefore.stderr,
        $statusAfter.stdout,
        $statusAfter.stderr,
        $stopResult.stdout,
        $stopResult.stderr
    )
    $evidence.failure_string_present = Test-ContainsFailureString -Texts $allText
    if ($evidence.failure_string_present) {
        throw "Forbidden failure string observed: $FailureString"
    }

    $serversAfter = @($evidence.status_after_reconnect.servers)
    if ([string]::IsNullOrWhiteSpace([string]$evidence.status_after_reconnect.daemon_generation)) {
        throw "status_after_reconnect missing daemon_generation"
    }
    if ($serversAfter.Count -lt 1) {
        throw "status_after_reconnect contains no owners"
    }
    if ([string]::IsNullOrWhiteSpace([string]$serversAfter[0].owner_generation)) {
        throw "status_after_reconnect first owner missing owner_generation"
    }

    $evidence.verdict = "PASS"
    $evidence.stdout_lines = @($observedStdout)
    $evidence.stderr_lines = @($observedStderr)
    $evidence.shim_log_tail = Select-LastLines -Text $shimLog
    $evidence.daemon_log_tail = Select-LastLines -Text $daemonLog
    Write-SmokeEvidence -Evidence $evidence
} catch {
    if ($null -ne $process -and -not $processStopped) {
        try {
            Drain-Stdout -Process $process -Observed $observedStdout
        } catch {
        }
        $stderrText = Stop-SmokeProcess -Process $process -StderrTask $stderrTask
        $processStopped = $true
        foreach ($line in ($stderrText -split "`r`n|`n|`r")) {
            if (-not [string]::IsNullOrWhiteSpace($line)) {
                [void]$observedStderr.Add($line)
            }
        }
    }
    $shimLog = Read-TextIfExists -Path $ShimLogPath
    $daemonLog = Read-TextIfExists -Path $DaemonLogPath
    $evidence.verdict = "FAIL"
    $evidence.error = $_.Exception.Message
    $evidence.stdout_lines = @($observedStdout)
    $evidence.stderr_lines = @($observedStderr)
    $evidence.shim_log_tail = Select-LastLines -Text $shimLog
    $evidence.daemon_log_tail = Select-LastLines -Text $daemonLog
    $evidence.failure_string_present = Test-ContainsFailureString -Texts @(
        ($observedStdout -join "`n"),
        ($observedStderr -join "`n"),
        $shimLog,
        $daemonLog
    )
    Write-SmokeEvidence -Evidence $evidence | Out-Host
    throw
} finally {
    if ($null -ne $process -and -not $processStopped) {
        try {
            Stop-SmokeProcess -Process $process -StderrTask $stderrTask | Out-Null
        } catch {
        }
    }
    if ($null -ne $process) {
        $process.Dispose()
    }

    try {
        Invoke-SmokeNative -FileName $ResolvedBinary -Arguments @("stop", "--force") -TimeoutMs 10000 | Out-Null
    } catch {
    }
}
