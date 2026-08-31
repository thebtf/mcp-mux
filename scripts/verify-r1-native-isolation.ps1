#Requires -Version 5.1
<#
.SYNOPSIS
    Runs the Windows customer proof for the MCP 2026-07-28 native isolation route.

.DESCRIPTION
    Builds the exact supplied candidate and deterministic fixtures into a fresh
    base directory under OutputDir, drives the eight R1 customer scenarios, and
    emits the shared summary.json, transcript.ndjson, and named artifacts.

    Exit codes:
      0 = PASS
      1 = customer-proof verification failure
      2 = unavailable/invalid source, tool, or output setup
#>

[CmdletBinding()]
param(
    [string] $SourceRoot = "",
    [string] $OutputDir = "",
    [switch] $KeepBaseDir
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$script:RunnerName = "verify-r1-native-isolation"
$script:Policy = "--mcp-protocol=2026-07-28"
$script:Utf8NoBom = [System.Text.UTF8Encoding]::new($false)
$script:SourceRootFull = $null
$script:OutputDirFull = $null
$script:BaseDir = $null
$script:RuntimeDir = $null
$script:BinDir = $null
$script:ArtifactDir = $null
$script:CaptureDir = $null
$script:TranscriptPath = $null
$script:TranscriptWriter = $null
$script:GoPath = $null
$script:GitPath = $null
$script:MuxBinary = $null
$script:ModernFixture = $null
$script:LegacyFixture = $null
$script:CorpusPath = $null
$script:SourceSHA = $null
$script:BinarySHA = $null
$script:ModernFixtureSHA = $null
$script:LegacyFixtureSHA = $null
$script:CorpusSHA = $null
$script:GoVersion = $null
$script:ActiveSessions = New-Object System.Collections.ArrayList
$script:CleanupCompleted = $false
$script:BaseRemoved = $false
$script:ScenarioResults = [ordered]@{}
foreach ($scenarioID in 1..8) {
    $script:ScenarioResults[[string] $scenarioID] = "NOT_RUN"
}

$script:ArtifactRefs = [ordered]@{
    modern = "artifacts/modern.json"
    admission = "artifacts/admission.json"
    lifecycle = "artifacts/lifecycle.json"
    readback = "artifacts/readback.json"
    legacy = "artifacts/legacy.json"
    rollback = "artifacts/rollback.json"
}

function Write-Step {
    param([string] $Message)
    Write-Host "[$script:RunnerName] $Message"
}

function New-RunnerException {
    param(
        [int] $ExitCode,
        [string] $Message
    )

    $exception = New-Object System.InvalidOperationException -ArgumentList $Message
    $exception.Data["runner_exit_code"] = $ExitCode
    return $exception
}

function Stop-Setup {
    param([string] $Message)
    throw (New-RunnerException -ExitCode 2 -Message $Message)
}

function Stop-Verification {
    param([string] $Message)
    throw (New-RunnerException -ExitCode 1 -Message $Message)
}

function Assert-Setup {
    param(
        [bool] $Condition,
        [string] $Message
    )

    if (-not $Condition) {
        Stop-Setup $Message
    }
}

function Assert-Verification {
    param(
        [bool] $Condition,
        [string] $Message
    )

    if (-not $Condition) {
        Stop-Verification $Message
    }
}

function Test-NonEmptyFile {
    param([string] $Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $false
    }

    try {
        return ((Get-Item -LiteralPath $Path -ErrorAction Stop).Length -gt 0)
    }
    catch {
        return $false
    }
}

function Get-JsonProperty {
    param(
        [object] $Object,
        [string] $Name
    )

    if ($null -eq $Object) {
        return $null
    }
    if ($Object -is [System.Collections.IDictionary]) {
        if ($Object.Contains($Name)) {
            return $Object[$Name]
        }
        return $null
    }

    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }
    return $property.Value
}

function Test-JsonProperty {
    param(
        [object] $Object,
        [string] $Name
    )

    if ($null -eq $Object) {
        return $false
    }
    if ($Object -is [System.Collections.IDictionary]) {
        return $Object.Contains($Name)
    }
    return ($null -ne $Object.PSObject.Properties[$Name])
}

function ConvertTo-CompactJson {
    param([object] $Value)
    return ($Value | ConvertTo-Json -Depth 100 -Compress)
}

function Write-Utf8Text {
    param(
        [string] $Path,
        [string] $Text
    )

    [System.IO.File]::WriteAllText($Path, $Text, $script:Utf8NoBom)
}

function Write-JsonFile {
    param(
        [string] $Path,
        [object] $Value
    )

    Write-Utf8Text -Path $Path -Text ($Value | ConvertTo-Json -Depth 100)
}

function Initialize-Transcript {
    $script:TranscriptPath = Join-Path -Path $script:OutputDirFull -ChildPath "transcript.ndjson"
    $script:TranscriptWriter = New-Object System.IO.StreamWriter($script:TranscriptPath, $false, $script:Utf8NoBom)
    $script:TranscriptWriter.AutoFlush = $true
}

function Add-Transcript {
    param(
        [int] $ScenarioID,
        [string] $Scenario,
        [object] $Expected,
        [object] $Observed,
        [string] $Verdict,
        [string] $ArtifactRef = "",
        [object] $Commands = @()
    )

    if ($null -eq $script:TranscriptWriter) {
        return
    }

    $entry = [ordered]@{
        timestamp_utc = (Get-Date).ToUniversalTime().ToString("o")
        scenario_id = $ScenarioID
        scenario = $Scenario
        expected = $Expected
        observed = $Observed
        verdict = $Verdict
        artifact = $ArtifactRef
        commands = $Commands
    }
    $script:TranscriptWriter.WriteLine((ConvertTo-CompactJson $entry))
    $script:TranscriptWriter.Flush()
}

function Flush-Transcript {
    if ($null -ne $script:TranscriptWriter) {
        $script:TranscriptWriter.Flush()
    }
}

function Close-Transcript {
    if ($null -ne $script:TranscriptWriter) {
        $script:TranscriptWriter.Dispose()
        $script:TranscriptWriter = $null
    }
}

function Get-FileSha256 {
    param([string] $Path)

    if (-not (Test-NonEmptyFile $Path)) {
        Stop-Verification "cannot hash missing or empty file: $Path"
    }

    $stream = $null
    $algorithm = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
        $algorithm = [System.Security.Cryptography.SHA256]::Create()
        $bytes = $algorithm.ComputeHash($stream)
        return ([System.BitConverter]::ToString($bytes).Replace("-", "").ToLowerInvariant())
    }
    finally {
        if ($null -ne $algorithm) {
            $algorithm.Dispose()
        }
        if ($null -ne $stream) {
            $stream.Dispose()
        }
    }
}

function Get-TextSha256 {
    param([string] $Text)

    $algorithm = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = $script:Utf8NoBom.GetBytes($Text)
        return ([System.BitConverter]::ToString($algorithm.ComputeHash($bytes)).Replace("-", "").ToLowerInvariant())
    }
    finally {
        $algorithm.Dispose()
    }
}

function Get-RequiredApplication {
    param([string] $Name)

    $command = Get-Command -Name $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $command) {
        Stop-Setup "required executable is unavailable on PATH: $Name"
    }

    $path = [string] $command.Source
    if ([string]::IsNullOrWhiteSpace($path)) {
        $path = [string] $command.Definition
    }
    if ([string]::IsNullOrWhiteSpace($path) -or -not (Test-Path -LiteralPath $path -PathType Leaf)) {
        Stop-Setup "required executable has no usable path: $Name"
    }
    return [System.IO.Path]::GetFullPath($path)
}

function ConvertTo-WindowsCommandLineArgument {
    param([AllowEmptyString()] [string] $Argument)

    if ($null -eq $Argument -or $Argument.Length -eq 0) {
        return '""'
    }
    if ($Argument -notmatch '[\s"]') {
        return $Argument
    }

    $builder = New-Object System.Text.StringBuilder
    [void] $builder.Append([char] 34)
    $backslashes = 0
    foreach ($character in $Argument.ToCharArray()) {
        if ($character -eq [char] 92) {
            $backslashes++
            continue
        }
        if ($character -eq [char] 34) {
            for ($index = 0; $index -lt ($backslashes * 2 + 1); $index++) {
                [void] $builder.Append([char] 92)
            }
            [void] $builder.Append([char] 34)
            $backslashes = 0
            continue
        }
        for ($index = 0; $index -lt $backslashes; $index++) {
            [void] $builder.Append([char] 92)
        }
        $backslashes = 0
        [void] $builder.Append($character)
    }
    for ($index = 0; $index -lt ($backslashes * 2); $index++) {
        [void] $builder.Append([char] 92)
    }
    [void] $builder.Append([char] 34)
    return $builder.ToString()
}

function ConvertTo-WindowsCommandLine {
    param([string[]] $Arguments)

    $rendered = New-Object 'System.Collections.Generic.List[string]'
    foreach ($argument in @($Arguments)) {
        [void] $rendered.Add((ConvertTo-WindowsCommandLineArgument -Argument $argument))
    }
    return [string]::Join(" ", $rendered.ToArray())
}

function New-NativeProcessStartInfo {
    param(
        [string] $FilePath,
        [string[]] $Arguments,
        [string] $WorkingDirectory,
        [hashtable] $EnvironmentOverrides
    )

    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $FilePath
    $startInfo.Arguments = ConvertTo-WindowsCommandLine -Arguments $Arguments
    $startInfo.WorkingDirectory = $WorkingDirectory
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardInput = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    $startInfo.StandardOutputEncoding = $script:Utf8NoBom
    $startInfo.StandardErrorEncoding = $script:Utf8NoBom

    foreach ($key in $EnvironmentOverrides.Keys) {
        $value = $EnvironmentOverrides[$key]
        if ($null -eq $value) {
            [void] $startInfo.EnvironmentVariables.Remove($key)
        }
        else {
            $startInfo.EnvironmentVariables[$key] = [string] $value
        }
    }
    return $startInfo
}

function Get-NativeTaskText {
    param(
        [object] $Task,
        [string] $Label
    )

    if ($null -eq $Task) {
        return ""
    }
    if (-not $Task.Wait(5000)) {
        throw "timed out draining $Label after process exit"
    }
    return [string] $Task.Result
}

function Invoke-NativeProcess {
    param(
        [string] $FilePath,
        [string[]] $Arguments = @(),
        [string] $WorkingDirectory,
        [AllowNull()] [string] $StdinText = $null,
        [hashtable] $EnvironmentOverrides = @{},
        [int] $TimeoutSeconds = 30
    )

    if ($TimeoutSeconds -le 0) {
        throw "native process timeout must be positive"
    }

    $process = $null
    $stdoutTask = $null
    $stderrTask = $null
    try {
        $process = New-Object System.Diagnostics.Process
        $process.StartInfo = New-NativeProcessStartInfo -FilePath $FilePath -Arguments $Arguments -WorkingDirectory $WorkingDirectory -EnvironmentOverrides $EnvironmentOverrides
        if (-not $process.Start()) {
            throw "failed to start native process: $FilePath"
        }

        # Begin both drains before writing stdin or waiting. This avoids the
        # Windows pipe deadlock and preserves complete stdout/stderr evidence.
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        if ($null -ne $StdinText) {
            $process.StandardInput.Write($StdinText)
            $process.StandardInput.Flush()
        }
        $process.StandardInput.Close()

        $timeoutMs = [Math]::Max(1, [int] ($TimeoutSeconds * 1000))
        if (-not $process.WaitForExit($timeoutMs)) {
            try {
                $process.Kill()
                [void] $process.WaitForExit(5000)
            }
            catch {
            }
            $stderr = Get-NativeTaskText -Task $stderrTask -Label "stderr"
            throw "timed out after $TimeoutSeconds seconds: $FilePath $($Arguments -join ' '); stderr=$stderr"
        }

        $stdout = Get-NativeTaskText -Task $stdoutTask -Label "stdout"
        $stderr = Get-NativeTaskText -Task $stderrTask -Label "stderr"
        return [pscustomobject]@{
            exit_code = [int] $process.ExitCode
            stdout = $stdout
            stderr = $stderr
            executable = $FilePath
            arguments = @($Arguments)
        }
    }
    finally {
        if ($null -ne $process) {
            $process.Dispose()
        }
    }
}

function Get-ProductEnvironment {
    param(
        [string] $CapturePath = "",
        [string] $Mode = "",
        [string] $ShimLogPath = ""
    )

    # Every product invocation receives an isolated standard temporary root.
    # Overrides live only on ProcessStartInfo, so the caller's environment is
    # never changed and therefore needs no host-environment repair afterward.
    return @{
        TEMP = $script:RuntimeDir
        TMP = $script:RuntimeDir
        TMPDIR = $script:RuntimeDir
        MCP_MUX_MODERN_CAPTURE_FILE = $CapturePath
        MCP_MUX_MODERN_MODE = $Mode
        MCP_MUX_SHIM_LOG = $ShimLogPath
        MCP_MUX_NO_DAEMON = $null
        MCP_MUX_DAEMON = $null
        MCP_MUX_ISOLATED = $null
        MCP_MUX_STATELESS = $null
        MCP_MUX_DEFAULT_MODE = $null
        MCPMUX_STATUS_TRACE = $null
    }
}

function Get-BuildEnvironment {
    return @{
        TEMP = (Join-Path -Path $script:BaseDir -ChildPath "build-temp")
        TMP = (Join-Path -Path $script:BaseDir -ChildPath "build-temp")
        TMPDIR = (Join-Path -Path $script:BaseDir -ChildPath "build-temp")
        GOCACHE = (Join-Path -Path $script:BaseDir -ChildPath "gocache")
        GOTMPDIR = (Join-Path -Path $script:BaseDir -ChildPath "gotmp")
    }
}

function New-CommandEvidence {
    param(
        [string] $Label,
        [string] $Executable,
        [string[]] $Arguments
    )

    return [ordered]@{
        label = $Label
        executable = $Executable
        arguments = @($Arguments)
    }
}

function ConvertFrom-NdjsonText {
    param(
        [string] $Text,
        [string] $Label
    )

    $messages = New-Object System.Collections.ArrayList
    foreach ($line in [System.Text.RegularExpressions.Regex]::Split($Text, '\r?\n')) {
        if ([string]::IsNullOrWhiteSpace($line)) {
            continue
        }
        try {
            $message = $line | ConvertFrom-Json -ErrorAction Stop
        }
        catch {
            Stop-Verification "$Label emitted non-JSON stdout: [$line]"
        }
        [void] $messages.Add([pscustomobject]@{ raw = $line; message = $message })
    }
    return $messages.ToArray()
}

function Read-Utf8TextShared {
    param([string] $Path)

    $stream = [System.IO.File]::Open(
        $Path,
        [System.IO.FileMode]::Open,
        [System.IO.FileAccess]::Read,
        [System.IO.FileShare]::ReadWrite
    )
    try {
        $reader = New-Object System.IO.StreamReader($stream, $script:Utf8NoBom, $true)
        try {
            return $reader.ReadToEnd()
        }
        finally {
            $reader.Dispose()
        }
    }
    finally {
        $stream.Dispose()
    }
}

function Get-CapturedFrameLines {
    param([string] $Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return @()
    }
    $text = Read-Utf8TextShared -Path $Path
    if ($text.Length -eq 0) {
        return @()
    }

    $lines = New-Object 'System.Collections.Generic.List[string]'
    $parts = [System.Text.RegularExpressions.Regex]::Split($text, "`n")
    $last = $parts.Length - 1
    for ($index = 0; $index -lt $parts.Length; $index++) {
        $line = $parts[$index]
        if ($index -eq $last -and $line.Length -eq 0) {
            continue
        }
        if ($line.EndsWith("`r")) {
            $line = $line.Substring(0, $line.Length - 1)
        }
        [void] $lines.Add($line)
    }
    return $lines.ToArray()
}

function Assert-ExactCapture {
    param(
        [string] $Path,
        [string[]] $ExpectedFrames,
        [string] $Label
    )

    Assert-Verification (Test-NonEmptyFile $Path) "$Label did not create a nonempty upstream capture"
    $actualFrames = @(Get-CapturedFrameLines -Path $Path)
    Assert-Verification ($actualFrames.Count -eq $ExpectedFrames.Count) "$Label captured $($actualFrames.Count) upstream frame(s), want $($ExpectedFrames.Count)"
    for ($index = 0; $index -lt $ExpectedFrames.Count; $index++) {
        Assert-Verification ($actualFrames[$index] -ceq $ExpectedFrames[$index]) "$Label upstream frame $($index + 1) is not byte-identical to the host frame"
    }
    return $actualFrames
}


function Wait-ForFileLength {
    param(
        [string] $Path,
        [long] $ExpectedLength,
        [int] $TimeoutSeconds,
        [string] $Label
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $Path -PathType Leaf) {
            $item = Get-Item -LiteralPath $Path
            if ($item.Length -eq $ExpectedLength) {
                return $item
            }
        }
        Start-Sleep -Milliseconds 25
    }
    Stop-Verification "$Label did not reach length $ExpectedLength within $TimeoutSeconds seconds"
}

function Start-RpcSession {
    param(
        [string] $Label,
        [string[]] $Arguments,
        [string] $CapturePath = "",
        [string] $Mode = ""
    )

    $process = New-Object System.Diagnostics.Process
    $shimLog = Join-Path -Path $script:ArtifactDir -ChildPath ("{0}.shim.log" -f ($Label -replace '[^A-Za-z0-9_.-]', '-'))
    $process.StartInfo = New-NativeProcessStartInfo -FilePath $script:MuxBinary -Arguments $Arguments -WorkingDirectory $script:SourceRootFull -EnvironmentOverrides (Get-ProductEnvironment -CapturePath $CapturePath -Mode $Mode -ShimLogPath $shimLog)
    if (-not $process.Start()) {
        $process.Dispose()
        Stop-Verification "failed to start RPC session: $Label"
    }
    $process.StandardInput.NewLine = "`n"

    $session = [pscustomobject]@{
        label = $Label
        process = $process
        pending_stdout = $null
        stderr_task = $process.StandardError.ReadToEndAsync()
        messages = New-Object System.Collections.ArrayList
        closed = $false
        close_result = $null
        command = New-CommandEvidence -Label $Label -Executable $script:MuxBinary -Arguments $Arguments
    }
    [void] $script:ActiveSessions.Add($session)
    return $session
}

function Send-RpcFrame {
    param(
        [object] $Session,
        [string] $Frame
    )

    Assert-Verification (-not $Session.closed) "cannot send a frame to closed session $($Session.label)"
    try {
        $Session.process.StandardInput.Write($Frame)
        $Session.process.StandardInput.Write("`n")
        $Session.process.StandardInput.Flush()
    }
    catch {
        Stop-Verification "failed to write host frame to $($Session.label): $($_.Exception.Message)"
    }
}

function Get-SessionStderr {
    param([object] $Session)

    if ($null -eq $Session.stderr_task -or -not $Session.stderr_task.IsCompleted) {
        return ""
    }
    try {
        return [string] $Session.stderr_task.Result
    }
    catch {
        return "<stderr unavailable: $($_.Exception.Message)>"
    }
}

function Read-RpcUntil {
    param(
        [object] $Session,
        [object] $ExpectedID,
        [int] $TimeoutSeconds
    )

    $observed = New-Object System.Collections.ArrayList
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($null -eq $Session.pending_stdout) {
            $Session.pending_stdout = $Session.process.StandardOutput.ReadLineAsync()
        }
        if (-not $Session.pending_stdout.Wait(50)) {
            if ($Session.process.HasExited) {
                Stop-Verification "session $($Session.label) exited with code $($Session.process.ExitCode) while waiting for response ID $ExpectedID; stderr=$(Get-SessionStderr $Session)"
            }
            continue
        }

        $line = $Session.pending_stdout.Result
        $Session.pending_stdout = $null
        if ($null -eq $line) {
            Stop-Verification "session $($Session.label) closed stdout while waiting for response ID $ExpectedID; stderr=$(Get-SessionStderr $Session)"
        }
        if ([string]::IsNullOrWhiteSpace($line)) {
            continue
        }

        try {
            $message = $line | ConvertFrom-Json -ErrorAction Stop
        }
        catch {
            Stop-Verification "session $($Session.label) emitted non-JSON stdout: [$line]"
        }

        $record = [pscustomobject]@{
            timestamp_utc = (Get-Date).ToUniversalTime().ToString("o")
            raw = $line
            message = $message
        }
        [void] $Session.messages.Add($record)
        [void] $observed.Add($record)
        if ((Test-JsonProperty -Object $message -Name "id") -and ("$(Get-JsonProperty -Object $message -Name 'id')" -ceq "$ExpectedID")) {
            return [pscustomobject]@{
                response = $message
                response_raw = $line
                messages = $observed.ToArray()
            }
        }
    }

    Stop-Verification "timed out after $TimeoutSeconds seconds waiting for response ID $ExpectedID from $($Session.label)"
}

function Close-RpcSession {
    param(
        [object] $Session,
        [int] $TimeoutSeconds = 5
    )

    if ($null -eq $Session) {
        return [pscustomobject]@{ label = ""; closed = $true; exit_code = $null; stderr = "" }
    }
    if ($Session.closed) {
        return $Session.close_result
    }

    try {
        try {
            $Session.process.StandardInput.Close()
        }
        catch {
        }

        $timeoutMs = [Math]::Max(1, [int] ($TimeoutSeconds * 1000))
        if (-not $Session.process.WaitForExit($timeoutMs)) {
            try {
                if (-not $Session.process.HasExited) {
                    $Session.process.Kill()
                }
            }
            catch {
                if (-not $Session.process.HasExited) {
                    throw
                }
            }
            if (-not $Session.process.WaitForExit(5000)) {
                Stop-Verification "owned session $($Session.label) did not exit after forced termination"
            }
        }

        $stderr = Get-NativeTaskText -Task $Session.stderr_task -Label "stderr for $($Session.label)"
        $closeResult = [pscustomobject]@{
            label = $Session.label
            closed = $true
            exit_code = [int] $Session.process.ExitCode
            stderr = $stderr
        }
        $Session.close_result = $closeResult
        return $closeResult
    }
    finally {
        $Session.closed = $true
        [void] $script:ActiveSessions.Remove($Session)
        $Session.process.Dispose()
    }
}

function Wait-ForIsolatedDaemonExit {
    param(
        [string] $Reason,
        [int] $TimeoutSeconds = 10
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $attempts = 0
    $last = $null
    while ([DateTime]::UtcNow -lt $deadline) {
        $attempts++
        $last = Invoke-NativeProcess -FilePath $script:MuxBinary -Arguments @("status") -WorkingDirectory $script:SourceRootFull -EnvironmentOverrides (Get-ProductEnvironment) -TimeoutSeconds 5
        $text = $last.stdout.Trim()
        if ($last.exit_code -eq 0 -and $text -ceq "No active mcp-mux instances found.") {
            return [pscustomobject]@{
                attempts = $attempts
                status = $text
                command = New-CommandEvidence -Label "wait-stopped-$Reason" -Executable $script:MuxBinary -Arguments @("status")
            }
        }
        Start-Sleep -Milliseconds 50
    }

    Stop-Verification "daemon did not retire after stop during ${Reason}: $($last.stdout) $($last.stderr)"
}

function Stop-IsolatedDaemon {
    param(
        [string] $Reason,
        [switch] $BestEffort
    )

    $closedSessions = New-Object System.Collections.ArrayList

    try {
        foreach ($activeSession in @($script:ActiveSessions.ToArray())) {
            [void] $closedSessions.Add((Close-RpcSession -Session $activeSession -TimeoutSeconds 0))
        }
        $arguments = @("stop", "--force")
        $result = Invoke-NativeProcess -FilePath $script:MuxBinary -Arguments $arguments -WorkingDirectory $script:SourceRootFull -EnvironmentOverrides (Get-ProductEnvironment) -TimeoutSeconds 15
        if ($result.exit_code -ne 0) {
            Stop-Verification "built mcp-mux stop --force failed during $Reason with exit code $($result.exit_code): $($result.stderr)"
        }
        $wait = Wait-ForIsolatedDaemonExit -Reason $Reason
        return [pscustomobject]@{
            reason = $Reason
            exit_code = $result.exit_code
            stdout = $result.stdout
            stderr = $result.stderr
            command = New-CommandEvidence -Label "stop-$Reason" -Executable $script:MuxBinary -Arguments $arguments
            wait = $wait
            closed_sessions = $closedSessions.ToArray()
        }
    }
    catch {
        if ($BestEffort) {
            return [pscustomobject]@{
                reason = $Reason
                exit_code = $null
                cleanup_error = $_.Exception.Message
                command = New-CommandEvidence -Label "stop-$Reason" -Executable $script:MuxBinary -Arguments @("stop", "--force")
            }
        }
        throw
    }
}

function Get-StatusSnapshot {
    param([string] $Label)

    $arguments = @("status")
    $result = Invoke-NativeProcess -FilePath $script:MuxBinary -Arguments $arguments -WorkingDirectory $script:SourceRootFull -EnvironmentOverrides (Get-ProductEnvironment) -TimeoutSeconds 15
    if ($result.exit_code -ne 0) {
        Stop-Verification "built mcp-mux status failed during $Label with exit code $($result.exit_code): $($result.stderr)"
    }

    $json = $null
    $trimmed = $result.stdout.Trim()
    if (-not [string]::IsNullOrWhiteSpace($trimmed)) {
        if ($trimmed.StartsWith("{") -or $trimmed.StartsWith("[")) {
            try {
                $json = $trimmed | ConvertFrom-Json -ErrorAction Stop
            }
            catch {
                Stop-Verification "mcp-mux status emitted invalid JSON during ${Label}: $trimmed"
            }
        }
    }

    return [pscustomobject]@{
        label = $Label
        stdout = $result.stdout
        stderr = $result.stderr
        json = $json
        command = New-CommandEvidence -Label "status-$Label" -Executable $script:MuxBinary -Arguments $arguments
    }
}

function Add-ObjectsWithProperty {
    param(
        [object] $Value,
        [string] $Name,
        [System.Collections.ArrayList] $Collector
    )

    if ($null -eq $Value -or $Value -is [string]) {
        return
    }
    if ($Value -is [System.Collections.IDictionary]) {
        if ($Value.Contains($Name)) {
            [void] $Collector.Add($Value)
        }
        foreach ($key in $Value.Keys) {
            Add-ObjectsWithProperty -Value $Value[$key] -Name $Name -Collector $Collector
        }
        return
    }
    if ($Value -is [System.Management.Automation.PSCustomObject]) {
        if (Test-JsonProperty -Object $Value -Name $Name) {
            [void] $Collector.Add($Value)
        }
        foreach ($property in $Value.PSObject.Properties) {
            Add-ObjectsWithProperty -Value $property.Value -Name $Name -Collector $Collector
        }
        return
    }
    if ($Value -is [System.Collections.IEnumerable]) {
        foreach ($item in $Value) {
            Add-ObjectsWithProperty -Value $item -Name $Name -Collector $Collector
        }
    }
}

function Get-ObjectsWithProperty {
    param(
        [object] $Value,
        [string] $Name
    )

    $collector = New-Object System.Collections.ArrayList
    Add-ObjectsWithProperty -Value $Value -Name $Name -Collector $collector
    return $collector.ToArray()
}

function Get-ModernStatusOwners {
    param([object] $StatusJson)

    $owners = New-Object System.Collections.ArrayList
    foreach ($candidate in @(Get-ObjectsWithProperty -Value $StatusJson -Name "protocol_era")) {
        if ((Get-JsonProperty -Object $candidate -Name "protocol_era") -ceq "2026-07-28") {
            [void] $owners.Add($candidate)
        }
    }
    return $owners.ToArray()
}

function Get-StatusOwnersForCommand {
    param(
        [object] $StatusJson,
        [string] $Command
    )

    $owners = New-Object System.Collections.ArrayList
    foreach ($candidate in @(Get-ObjectsWithProperty -Value $StatusJson -Name "command")) {
        if ((Get-JsonProperty -Object $candidate -Name "command") -ceq $Command) {
            [void] $owners.Add($candidate)
        }
    }
    return $owners.ToArray()
}

function Assert-ModernStatus {
    param(
        [object] $Snapshot,
        [string] $Label,
        [int] $ExpectedCount = 1
    )

    $owners = @(Get-ModernStatusOwners -StatusJson $Snapshot.json)
    Assert-Verification ($owners.Count -eq $ExpectedCount) "$Label reported $($owners.Count) modern owner(s), want $ExpectedCount"
    if ($ExpectedCount -eq 0) {
        return $null
    }

    $owner = $owners[0]
    $expectedFacts = [ordered]@{
        protocol_era = "2026-07-28"
        sharing_policy = "forced-isolated"
        cache_policy = "off"
        lifecycle_policy = "r1-quarantine"
    }
    foreach ($key in $expectedFacts.Keys) {
        Assert-Verification ((Get-JsonProperty -Object $owner -Name $key) -ceq $expectedFacts[$key]) "$Label owner $key does not equal $($expectedFacts[$key])"
    }

    # These are the R1 / sensitive fields explicitly excluded from a modern
    # OwnerInfo projection. Preserve the established readiness fields instead.
    $prohibited = @(
        "sessions", "inflight", "oldest_request_age_ms", "finalization_error",
        "owner_generation", "restored_from_owner_generation", "restore_source",
        "mux_engines", "topology", "registry", "registry_descriptor",
        "taxonomy", "counter", "counters", "logging", "logs"
    )
    foreach ($key in $prohibited) {
        Assert-Verification (-not (Test-JsonProperty -Object $owner -Name $key)) "$Label modern owner leaks prohibited R3/status key $key"
    }

    Assert-Verification (Test-JsonProperty -Object $owner -Name "upstream_live") "$Label modern owner omitted existing upstream_live readiness"
    Assert-Verification ([bool] (Get-JsonProperty -Object $owner -Name "upstream_live")) "$Label modern owner reports upstream_live=false"
    return $owner
}

function Assert-NoModernStatus {
    param(
        [object] $Snapshot,
        [string] $Label
    )

    $owners = @(Get-ModernStatusOwners -StatusJson $Snapshot.json)
    Assert-Verification ($owners.Count -eq 0) "$Label still reports $($owners.Count) modern owner(s)"
}

function Assert-NoModernPolicyFields {
    param(
        [object] $StatusJson,
        [string] $Label
    )

    foreach ($key in @("protocol_era", "sharing_policy", "cache_policy", "lifecycle_policy")) {
        $found = @(Get-ObjectsWithProperty -Value $StatusJson -Name $key)
        Assert-Verification ($found.Count -eq 0) "$Label unexpectedly exposes modern policy field $key"
    }
}

function Assert-TextExcludesSentinels {
    param(
        [string] $Text,
        [string[]] $Sentinels,
        [string] $Label
    )

    foreach ($sentinel in $Sentinels) {
        Assert-Verification ($Text.IndexOf($sentinel, [System.StringComparison]::Ordinal) -lt 0) "$Label leaks sentinel $sentinel"
    }
}

function Assert-RpcSuccess {
    param(
        [object] $Response,
        [object] $ExpectedID,
        [string] $Label
    )

    Assert-Verification ($null -eq (Get-JsonProperty -Object $Response -Name "error")) "$Label returned JSON-RPC error: $(ConvertTo-CompactJson $Response)"
    Assert-Verification ("$(Get-JsonProperty -Object $Response -Name 'id')" -ceq "$ExpectedID") "$Label response ID does not equal $ExpectedID"
    Assert-Verification ($null -ne (Get-JsonProperty -Object $Response -Name "result")) "$Label omitted JSON-RPC result"
}

function New-ModernFrame {
    param(
        [int] $ID,
        [string] $Method,
        [hashtable] $AdditionalParams = @{},
        [string] $LogLevel = ""
    )

    $meta = [ordered]@{
        "io.modelcontextprotocol/protocolVersion" = "2026-07-28"
        "io.modelcontextprotocol/clientCapabilities" = [ordered]@{}
    }
    if (-not [string]::IsNullOrWhiteSpace($LogLevel)) {
        $meta["io.modelcontextprotocol/logLevel"] = $LogLevel
    }

    $params = [ordered]@{ _meta = $meta }
    foreach ($key in $AdditionalParams.Keys) {
        $params[$key] = $AdditionalParams[$key]
    }

    return (ConvertTo-CompactJson ([ordered]@{
        jsonrpc = "2.0"
        id = $ID
        method = $Method
        params = $params
    }))
}

function New-LegacyFrame {
    param(
        [int] $ID,
        [string] $Method,
        [hashtable] $Params = @{}
    )

    return (ConvertTo-CompactJson ([ordered]@{
        jsonrpc = "2.0"
        id = $ID
        method = $Method
        params = $Params
    }))
}

function New-LegacyNotification {
    param(
        [string] $Method,
        [hashtable] $Params = @{}
    )

    return (ConvertTo-CompactJson ([ordered]@{
        jsonrpc = "2.0"
        method = $Method
        params = $Params
    }))
}

function Invoke-GoBuild {
    param(
        [string] $Output,
        [string] $Package
    )

    $arguments = @("build", "-trimpath", "-o", $Output, $Package)
    $result = Invoke-NativeProcess -FilePath $script:GoPath -Arguments $arguments -WorkingDirectory $script:SourceRootFull -EnvironmentOverrides (Get-BuildEnvironment) -TimeoutSeconds 120
    if ($result.exit_code -ne 0) {
        Stop-Setup "go build failed for $Package with exit code $($result.exit_code): $($result.stderr)"
    }
    if (-not (Test-NonEmptyFile $Output)) {
        Stop-Setup "go build for $Package did not produce a nonempty executable: $Output"
    }
    return New-CommandEvidence -Label "build-$Package" -Executable $script:GoPath -Arguments $arguments
}

function Save-Artifact {
    param(
        [string] $Name,
        [object] $Value
    )

    $relative = "artifacts/$Name.json"
    $path = Join-Path -Path $script:OutputDirFull -ChildPath $relative
    Write-JsonFile -Path $path -Value $Value
    Assert-Verification (Test-NonEmptyFile $path) "artifact $relative is missing or empty after write"
    return [pscustomobject]@{
        relative_path = $relative
        sha256 = Get-FileSha256 $path
    }
}

function Invoke-Scenario {
    param(
        [int] $ID,
        [string] $Name,
        [object] $Expected,
        [string] $ArtifactRef,
        [scriptblock] $Action
    )

    Write-Step "scenario ${ID}: $Name"
    try {
        $observed = & $Action
        $script:ScenarioResults[[string] $ID] = "PASS"
        Add-Transcript -ScenarioID $ID -Scenario $Name -Expected $Expected -Observed $observed -Verdict "PASS" -ArtifactRef $ArtifactRef -Commands (Get-JsonProperty -Object $observed -Name "commands")
        return $observed
    }
    catch {
        $script:ScenarioResults[[string] $ID] = "FAIL"
        $failure = [ordered]@{
            error = $_.Exception.Message
            exception_type = $_.Exception.GetType().FullName
        }
        Add-Transcript -ScenarioID $ID -Scenario $Name -Expected $Expected -Observed $failure -Verdict "FAIL" -ArtifactRef $ArtifactRef
        throw
    }
}

function Initialize-Runner {
    Assert-Setup ([System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT) "Windows runner requires Win32NT"
    Assert-Setup ($PSVersionTable.PSVersion -ge [version] "5.1") "Windows runner requires PowerShell 5.1 or newer"
    Assert-Setup (-not [string]::IsNullOrWhiteSpace($SourceRoot)) "SourceRoot must be nonempty"
    Assert-Setup (-not [string]::IsNullOrWhiteSpace($OutputDir)) "OutputDir must be nonempty"

    $script:SourceRootFull = [System.IO.Path]::GetFullPath($SourceRoot)
    $script:OutputDirFull = [System.IO.Path]::GetFullPath($OutputDir)
    Assert-Setup (Test-Path -LiteralPath $script:SourceRootFull -PathType Container) "SourceRoot does not exist: $script:SourceRootFull"
    Assert-Setup (Test-Path -LiteralPath $script:OutputDirFull -PathType Container) "OutputDir must already exist: $script:OutputDirFull"
    Assert-Setup (@(Get-ChildItem -LiteralPath $script:OutputDirFull -Force -ErrorAction Stop).Count -eq 0) "OutputDir must be empty before the runner creates evidence: $script:OutputDirFull"

    foreach ($relative in @(
        "go.mod",
        "cmd\mcp-mux\main.go",
        "testdata\mock_modern_server.go",
        "testdata\mock_server.go",
        "testdata\modern_opening_corpus.ndjson"
    )) {
        Assert-Setup (Test-Path -LiteralPath (Join-Path -Path $script:SourceRootFull -ChildPath $relative) -PathType Leaf) "SourceRoot is missing required runner input: $relative"
    }

    $script:GoPath = Get-RequiredApplication "go.exe"
    $script:GitPath = Get-RequiredApplication "git.exe"
    $script:BaseDir = Join-Path -Path $script:OutputDirFull -ChildPath "base"
    $script:RuntimeDir = Join-Path -Path $script:BaseDir -ChildPath "runtime"
    $script:BinDir = Join-Path -Path $script:BaseDir -ChildPath "bin"
    $script:ArtifactDir = Join-Path -Path $script:OutputDirFull -ChildPath "artifacts"
    $script:CaptureDir = Join-Path -Path $script:ArtifactDir -ChildPath "captures"
    foreach ($directory in @(
        $script:BaseDir,
        $script:RuntimeDir,
        $script:BinDir,
        (Join-Path -Path $script:BaseDir -ChildPath "build-temp"),
        (Join-Path -Path $script:BaseDir -ChildPath "gocache"),
        (Join-Path -Path $script:BaseDir -ChildPath "gotmp"),
        $script:ArtifactDir,
        $script:CaptureDir
    )) {
        New-Item -ItemType Directory -Path $directory -ErrorAction Stop | Out-Null
    }

    Initialize-Transcript
    Add-Transcript -ScenarioID 0 -Scenario "runner-setup" -Expected @{ output_dir_empty = $true; platform = "windows" } -Observed @{ source_root = $script:SourceRootFull; output_dir = $script:OutputDirFull; base_dir = $script:BaseDir; runtime_dir = $script:RuntimeDir } -Verdict "PASS"

    $git = Invoke-NativeProcess -FilePath $script:GitPath -Arguments @("-C", $script:SourceRootFull, "rev-parse", "HEAD") -WorkingDirectory $script:SourceRootFull -TimeoutSeconds 30
    if ($git.exit_code -ne 0) {
        Stop-Setup "git rev-parse HEAD failed: $($git.stderr)"
    }
    $script:SourceSHA = $git.stdout.Trim()
    Assert-Setup ($script:SourceSHA -match '^[0-9A-Fa-f]{40,64}$') "git source_sha is not hexadecimal: $script:SourceSHA"

    $goVersion = Invoke-NativeProcess -FilePath $script:GoPath -Arguments @("version") -WorkingDirectory $script:SourceRootFull -EnvironmentOverrides (Get-BuildEnvironment) -TimeoutSeconds 30
    if ($goVersion.exit_code -ne 0) {
        Stop-Setup "go version failed: $($goVersion.stderr)"
    }
    $script:GoVersion = $goVersion.stdout.Trim()

    $script:MuxBinary = Join-Path -Path $script:BinDir -ChildPath "mcp-mux.exe"
    $script:ModernFixture = Join-Path -Path $script:BinDir -ChildPath "mock_modern_server.exe"
    $script:LegacyFixture = Join-Path -Path $script:BinDir -ChildPath "mock_server.exe"
    $script:CorpusPath = Join-Path -Path $script:SourceRootFull -ChildPath "testdata\modern_opening_corpus.ndjson"

    $buildCommands = @(
        (Invoke-GoBuild -Output $script:MuxBinary -Package "./cmd/mcp-mux"),
        (Invoke-GoBuild -Output $script:ModernFixture -Package "./testdata/mock_modern_server.go"),
        (Invoke-GoBuild -Output $script:LegacyFixture -Package "./testdata/mock_server.go")
    )
    $script:BinarySHA = Get-FileSha256 $script:MuxBinary
    $script:ModernFixtureSHA = Get-FileSha256 $script:ModernFixture
    $script:LegacyFixtureSHA = Get-FileSha256 $script:LegacyFixture
    $script:CorpusSHA = Get-FileSha256 $script:CorpusPath

    Add-Transcript -ScenarioID 0 -Scenario "candidate-build" -Expected @{ exact_candidate_build = $true; binaries = @("mcp-mux", "mock_modern_server", "mock_server") } -Observed @{ binary_sha256 = $script:BinarySHA; fixture_sha256 = $script:ModernFixtureSHA; legacy_fixture_sha256 = $script:LegacyFixtureSHA; corpus_sha256 = $script:CorpusSHA } -Verdict "PASS" -Commands $buildCommands
}

function Get-ValidatedCorpusLines {
    $lines = New-Object 'System.Collections.Generic.List[string]'
    foreach ($line in [System.IO.File]::ReadAllLines($script:CorpusPath, $script:Utf8NoBom)) {
        if ($line.Length -eq 0) {
            continue
        }
        [void] $lines.Add($line)
    }
    Assert-Setup ($lines.Count -eq 100) "modern opening corpus must contain exactly 100 nonempty frames, found $($lines.Count)"
    return $lines.ToArray()
}

function Assert-CorpusNativeResponse {
    param(
        [object] $Response,
        [object] $ExpectedID,
        [string] $Method,
        [string] $Label
    )

    Assert-RpcSuccess -Response $Response -ExpectedID $ExpectedID -Label $Label
    $result = Get-JsonProperty -Object $Response -Name "result"
    if ($Method -eq "server/discover") {
        Assert-Verification ((Get-JsonProperty -Object $result -Name "resultType") -ceq "complete") "$Label discovery response is not the native fixture result"
        $supported = @(Get-JsonProperty -Object $result -Name "supportedVersions")
        Assert-Verification ($supported -contains "2026-07-28") "$Label discovery response omits the pinned protocol version"
    }
    elseif ($Method -eq "ping") {
        Assert-Verification ((ConvertTo-CompactJson $result) -ceq "{}") "$Label ping response is not the native fixture result"
    }
    else {
        $tools = @(Get-JsonProperty -Object $result -Name "tools")
        Assert-Verification ($tools.Count -gt 0) "$Label tools/list response has no native fixture tools"
        Assert-Verification ((Get-JsonProperty -Object $tools[0] -Name "name") -ceq "modern_echo") "$Label tools/list response is not the native fixture result"
    }
}

function Invoke-ModernCorpusScenarios {
    $lines = @(Get-ValidatedCorpusLines)
    $records = New-Object System.Collections.ArrayList
    $direct = 0
    $discover = 0
    $clientInfoPresent = 0
    $clientInfoAbsent = 0
    $resultSignatures = New-Object 'System.Collections.Generic.HashSet[string]'
    $firstDirect = $null
    $firstDiscover = $null

    for ($index = 0; $index -lt $lines.Count; $index++) {
        $raw = $lines[$index]
        try {
            $opening = $raw | ConvertFrom-Json -ErrorAction Stop
        }
        catch {
            Stop-Setup "corpus frame $($index + 1) is invalid JSON"
        }

        $method = [string] (Get-JsonProperty -Object $opening -Name "method")
        $id = Get-JsonProperty -Object $opening -Name "id"
        Assert-Verification (-not [string]::IsNullOrWhiteSpace($method)) "corpus frame $($index + 1) has no method"
        Assert-Verification ($null -ne $id) "corpus frame $($index + 1) has no request ID"
        $params = Get-JsonProperty -Object $opening -Name "params"
        $meta = Get-JsonProperty -Object $params -Name "_meta"
        $hasClientInfo = Test-JsonProperty -Object $meta -Name "io.modelcontextprotocol/clientInfo"
        if ($method -eq "server/discover") {
            $discover++
        }
        else {
            $direct++
        }
        if ($hasClientInfo) {
            $clientInfoPresent++
        }
        else {
            $clientInfoAbsent++
        }

        $captureRelative = "artifacts/captures/corpus-{0:D3}.ndjson" -f ($index + 1)
        $capturePath = Join-Path -Path $script:OutputDirFull -ChildPath $captureRelative
        $arguments = @($script:Policy, $script:ModernFixture)
        $session = $null
        $close = $null
        $stop = $null
        $status = $null
        try {
            # Keep stdin open only until the response and active-owner status
            # are observed. Force-stop then makes shim exit/stderr cleanup-only
            # instead of paying its probe-grace period for every corpus frame.
            $session = Start-RpcSession -Label ("corpus-{0:D3}" -f ($index + 1)) -Arguments $arguments -CapturePath $capturePath
            Send-RpcFrame -Session $session -Frame $raw
            $read = Read-RpcUntil -Session $session -ExpectedID $id -TimeoutSeconds 20
            $messages = @($read.messages)
            Assert-Verification ($messages.Count -eq 1) "corpus frame $($index + 1) produced $($messages.Count) stdout frame(s), want exactly one native response"
            $response = $read.response
            Assert-CorpusNativeResponse -Response $response -ExpectedID $id -Method $method -Label "corpus frame $($index + 1)"
            $captured = @(Assert-ExactCapture -Path $capturePath -ExpectedFrames @($raw) -Label "corpus frame $($index + 1)")
            [void] $resultSignatures.Add((ConvertTo-CompactJson (Get-JsonProperty -Object $response -Name "result")))

            $status = Get-StatusSnapshot -Label ("corpus-{0:D3}" -f ($index + 1))
            $owner = Assert-ModernStatus -Snapshot $status -Label "corpus frame $($index + 1) active owner"
            $stop = Stop-IsolatedDaemon -Reason ("corpus-frame-{0:D3}" -f ($index + 1))

            $frameRecord = [ordered]@{
                index = $index + 1
                opener_kind = if ($method -eq "server/discover") { "discover" } else { "direct" }
                client_info_present = $hasClientInfo
                expected_opening = $raw
                captured_first_upstream_line = $captured[0]
                response = $response
                response_raw = $read.response_raw
                owner_status = $owner
                command = $session.command
                status_command = $status.command
                stop = $stop
                capture = $captureRelative
            }
            [void] $records.Add($frameRecord)
            if ($method -eq "server/discover" -and $null -eq $firstDiscover) {
                $firstDiscover = $frameRecord
            }
            if ($method -ne "server/discover" -and $null -eq $firstDirect) {
                $firstDirect = $frameRecord
            }
        }
        finally {
            if ($null -ne $session) {
                try {
                    $close = Close-RpcSession -Session $session
                }
                catch {
                    $close = [ordered]@{ cleanup_error = $_.Exception.Message }
                }
            }
            if ($null -eq $stop) {
                [void] (Stop-IsolatedDaemon -Reason ("corpus-frame-failure-{0:D3}" -f ($index + 1)) -BestEffort)
            }
        }
        if ($null -ne $frameRecord) {
            $frameRecord["shim_cleanup"] = $close
        }
    }

    Assert-Verification ($direct -gt 0) "corpus has no direct modern opener"
    Assert-Verification ($discover -gt 0) "corpus has no host-sent server/discover opener"
    Assert-Verification ($clientInfoPresent -gt 0 -and $clientInfoAbsent -gt 0) "corpus does not cover present and absent clientInfo variants"
    Assert-Verification ($resultSignatures.Count -ge 2) "two distinct modern corpus inputs did not produce distinct result payloads"
    Assert-Verification ($null -ne $firstDirect -and $null -ne $firstDiscover) "corpus did not retain direct and discovery response evidence"
    Assert-Verification ("$(Get-JsonProperty -Object $firstDirect.response -Name 'id')" -cne "$(Get-JsonProperty -Object $firstDiscover.response -Name 'id')") "direct and discovery evidence did not retain distinct response IDs"

    $artifact = [ordered]@{
        schema_version = 1
        verdict = "PASS"
        corpus_total = $lines.Count
        corpus_passed = $records.Count
        direct_count = $direct
        discover_count = $discover
        client_info_present_count = $clientInfoPresent
        client_info_absent_count = $clientInfoAbsent
        distinct_result_count = $resultSignatures.Count
        direct_response_example = $firstDirect
        discovery_response_example = $firstDiscover
        frames = $records.ToArray()
    }
    $saved = Save-Artifact -Name "modern" -Value $artifact
    return [ordered]@{
        artifact = $saved.relative_path
        artifact_sha256 = $saved.sha256
        commands = @($records.ToArray() | ForEach-Object { $_.command })
        corpus_total = $lines.Count
        corpus_passed = $records.Count
        direct_count = $direct
        discover_count = $discover
        client_info_present_count = $clientInfoPresent
        client_info_absent_count = $clientInfoAbsent
    }
}

function Invoke-StrictAdmissionScenario {
    $cases = @(
        [ordered]@{
            name = "missing_meta"
            expected_code = -32602
            raw = '{"jsonrpc":"2.0","id":3001,"method":"tools/list","params":{}}'
        },
        [ordered]@{
            name = "null_meta"
            expected_code = -32602
            raw = '{"jsonrpc":"2.0","id":3002,"method":"tools/list","params":{"_meta":null}}'
        },
        [ordered]@{
            name = "non_object_capabilities"
            expected_code = -32602
            raw = '{"jsonrpc":"2.0","id":3003,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":"invalid"}}}'
        },
        [ordered]@{
            name = "missing_version"
            expected_code = -32602
            raw = '{"jsonrpc":"2.0","id":3004,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}'
        },
        [ordered]@{
            name = "malformed_client_info"
            expected_code = -32602
            raw = '{"jsonrpc":"2.0","id":3005,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":1,"version":"1"}}}}'
        },
        [ordered]@{
            name = "unsupported_version"
            expected_code = -32022
            raw = '{"jsonrpc":"2.0","id":3006,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-08-01","io.modelcontextprotocol/clientCapabilities":{}}}}'
        }
    )

    $records = New-Object System.Collections.ArrayList
    foreach ($case in $cases) {
        [void] (Stop-IsolatedDaemon -Reason ("admission-pre-{0}" -f $case.name))
        $captureRelative = "artifacts/captures/admission-$($case.name).ndjson"
        $capturePath = Join-Path -Path $script:OutputDirFull -ChildPath $captureRelative
        $arguments = @($script:Policy, $script:ModernFixture)
        $run = Invoke-NativeProcess -FilePath $script:MuxBinary -Arguments $arguments -WorkingDirectory $script:SourceRootFull -StdinText ($case.raw + "`n") -EnvironmentOverrides (Get-ProductEnvironment -CapturePath $capturePath) -TimeoutSeconds 20
        Assert-Verification ($run.exit_code -ne 0) "admission case $($case.name) unexpectedly succeeded"
        $messages = @(ConvertFrom-NdjsonText -Text $run.stdout -Label "admission case $($case.name)")
        Assert-Verification ($messages.Count -eq 1) "admission case $($case.name) emitted $($messages.Count) stdout frame(s), want one error"
        $message = $messages[0].message
        $error = Get-JsonProperty -Object $message -Name "error"
        Assert-Verification ($null -ne $error) "admission case $($case.name) returned no JSON-RPC error"
        Assert-Verification ([int] (Get-JsonProperty -Object $error -Name "code") -eq [int] $case.expected_code) "admission case $($case.name) error code is not $($case.expected_code)"
        Assert-Verification ("$(Get-JsonProperty -Object $message -Name 'id')" -ceq "$(($case.raw | ConvertFrom-Json).id)") "admission case $($case.name) did not preserve the request ID"
        if ($case.expected_code -eq -32022) {
            $data = Get-JsonProperty -Object $error -Name "data"
            Assert-Verification (@(Get-JsonProperty -Object $data -Name "supported") -contains "2026-07-28") "unsupported-version error omits supported 2026-07-28"
            Assert-Verification ((Get-JsonProperty -Object $data -Name "requested") -ceq "2026-08-01") "unsupported-version error omits requested version"
        }
        Assert-Verification (-not (Test-Path -LiteralPath $capturePath)) "admission case $($case.name) started the modern upstream fixture"

        $status = Get-StatusSnapshot -Label ("admission-$($case.name)")
        Assert-NoModernStatus -Snapshot $status -Label "admission case $($case.name) status"
        $stop = Stop-IsolatedDaemon -Reason ("admission-post-{0}" -f $case.name)
        [void] $records.Add([ordered]@{
            name = $case.name
            expected_code = $case.expected_code
            input = $case.raw
            exit_code = $run.exit_code
            error = $error
            fixture_capture_created = (Test-Path -LiteralPath $capturePath)
            status = $status
            stop = $stop
            command = New-CommandEvidence -Label ("admission-$($case.name)") -Executable $script:MuxBinary -Arguments $arguments
        })
    }

    $artifact = [ordered]@{
        schema_version = 1
        verdict = "PASS"
        cases = $records.ToArray()
    }
    $saved = Save-Artifact -Name "admission" -Value $artifact
    return [ordered]@{
        artifact = $saved.relative_path
        artifact_sha256 = $saved.sha256
        commands = @($records.ToArray() | ForEach-Object { $_.command })
        refusal_count = $records.Count
    }
}

function Invoke-NativeMRTRAndDirectionalityScenario {
    $subcases = New-Object System.Collections.ArrayList

    $inputCaptureRelative = "artifacts/captures/mrtr-input-required.ndjson"
    $inputCapture = Join-Path -Path $script:OutputDirFull -ChildPath $inputCaptureRelative
    $inputSession = $null
    $inputClose = $null
    $inputStop = $null
    try {
        $inputSession = Start-RpcSession -Label "mrtr-input-required" -Arguments @($script:Policy, $script:ModernFixture) -CapturePath $inputCapture -Mode "input_required"
        $first = New-ModernFrame -ID 4101 -Method "tools/call" -AdditionalParams @{ name = "modern_echo"; arguments = @{ message = "first request" } }
        Send-RpcFrame -Session $inputSession -Frame $first
        $firstRead = Read-RpcUntil -Session $inputSession -ExpectedID 4101 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $firstRead.response -ExpectedID 4101 -Label "input_required first request"
        $firstResult = Get-JsonProperty -Object $firstRead.response -Name "result"
        Assert-Verification ((Get-JsonProperty -Object $firstResult -Name "resultType") -ceq "input_required") "input_required resultType was not preserved"
        Assert-Verification ($null -ne (Get-JsonProperty -Object $firstResult -Name "inputRequests")) "input_required inputRequests were not preserved"
        $opaqueState = [string] (Get-JsonProperty -Object $firstResult -Name "requestState")
        Assert-Verification ($opaqueState -ceq "fixture-opaque-request-state-v1") "input_required opaque requestState was not preserved"

        $retry = New-ModernFrame -ID 4102 -Method "tools/call" -AdditionalParams @{
            name = "modern_echo"
            arguments = @{ message = "retry with host input" }
            requestState = $opaqueState
            inputResponses = @{ fixture_confirmation = @{ confirmed = $true } }
        }
        Send-RpcFrame -Session $inputSession -Frame $retry
        $retryRead = Read-RpcUntil -Session $inputSession -ExpectedID 4102 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $retryRead.response -ExpectedID 4102 -Label "input_required fresh retry"
        $retryResult = Get-JsonProperty -Object $retryRead.response -Name "result"
        Assert-Verification ((Get-JsonProperty -Object $retryResult -Name "resultType") -ceq "input_required") "new-ID retry did not remain native input_required traffic"
        [void] (Assert-ExactCapture -Path $inputCapture -ExpectedFrames @($first, $retry) -Label "input_required native retry")
        [void] $subcases.Add([ordered]@{
            name = "input_required_fresh_retry"
            first_response = $firstRead.response
            retry_response = $retryRead.response
            capture = $inputCaptureRelative
            command = $inputSession.command
        })
    }
    finally {
        try {
            $inputStop = Stop-IsolatedDaemon -Reason "mrtr-input-required"
        }
        finally {
            if ($null -ne $inputSession) {
                try {
                    $inputClose = Close-RpcSession -Session $inputSession
                }
                catch {
                    $inputClose = [ordered]@{ cleanup_error = $_.Exception.Message }
                }
            }
        }
    }

    $logCaptureRelative = "artifacts/captures/mrtr-request-log.ndjson"
    $logCapture = Join-Path -Path $script:OutputDirFull -ChildPath $logCaptureRelative
    $logSession = $null
    try {
        $logSession = Start-RpcSession -Label "mrtr-request-log" -Arguments @($script:Policy, $script:ModernFixture) -CapturePath $logCapture -Mode "request_log"
        $request = New-ModernFrame -ID 4201 -Method "tools/list" -LogLevel "info"
        Send-RpcFrame -Session $logSession -Frame $request
        $read = Read-RpcUntil -Session $logSession -ExpectedID 4201 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $read.response -ExpectedID 4201 -Label "request-scoped log response"
        $logs = @($read.messages | Where-Object { (Get-JsonProperty -Object $_.message -Name "method") -ceq "notifications/message" })
        Assert-Verification ($logs.Count -eq 1) "request-scoped fixture log reached the host $($logs.Count) times, want once"
        $logParams = Get-JsonProperty -Object $logs[0].message -Name "params"
        Assert-Verification ((Get-JsonProperty -Object $logParams -Name "level") -ceq "info") "request-scoped log level changed"
        Assert-Verification ((Get-JsonProperty -Object $logParams -Name "data") -ceq "request-scoped fixture log") "request-scoped log content changed or was synthesized"
        [void] (Assert-ExactCapture -Path $logCapture -ExpectedFrames @($request) -Label "request-scoped log")
        [void] $subcases.Add([ordered]@{
            name = "request_scoped_log_once"
            response = $read.response
            observed_messages = $read.messages
            capture = $logCaptureRelative
            command = $logSession.command
        })
    }
    finally {
        try {
            $logStop = Stop-IsolatedDaemon -Reason "mrtr-request-log"
        }
        finally {
            if ($null -ne $logSession) {
                try {
                    $logClose = Close-RpcSession -Session $logSession
                }
                catch {
                    $logClose = [ordered]@{ cleanup_error = $_.Exception.Message }
                }
            }
        }
    }

    $serverRequestCaptureRelative = "artifacts/captures/mrtr-server-request.ndjson"
    $serverRequestCapture = Join-Path -Path $script:OutputDirFull -ChildPath $serverRequestCaptureRelative
    $serverRequestSession = $null
    try {
        $serverRequestSession = Start-RpcSession -Label "mrtr-server-request" -Arguments @($script:Policy, $script:ModernFixture) -CapturePath $serverRequestCapture -Mode "server_request"
        $request = New-ModernFrame -ID 4301 -Method "tools/list"
        Send-RpcFrame -Session $serverRequestSession -Frame $request
        $read = Read-RpcUntil -Session $serverRequestSession -ExpectedID 4301 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $read.response -ExpectedID 4301 -Label "contained server request response"
        $escaped = @($read.messages | Where-Object {
            ((Get-JsonProperty -Object $_.message -Name "method") -ceq "sampling/createMessage") -or
            ("$(Get-JsonProperty -Object $_.message -Name 'id')" -ceq "fixture-server-request-1")
        })
        Assert-Verification ($escaped.Count -eq 0) "fixture JSON-RPC server request escaped to the host"
        [void] (Assert-ExactCapture -Path $serverRequestCapture -ExpectedFrames @($request) -Label "contained server request")
        [void] $subcases.Add([ordered]@{
            name = "server_request_contained"
            response = $read.response
            observed_messages = $read.messages
            capture = $serverRequestCaptureRelative
            command = $serverRequestSession.command
        })
    }
    finally {
        try {
            $serverRequestStop = Stop-IsolatedDaemon -Reason "mrtr-server-request"
        }
        finally {
            if ($null -ne $serverRequestSession) {
                try {
                    $serverRequestClose = Close-RpcSession -Session $serverRequestSession
                }
                catch {
                    $serverRequestClose = [ordered]@{ cleanup_error = $_.Exception.Message }
                }
            }
        }
    }

    return [ordered]@{
        subcases = $subcases.ToArray()
        cleanup = @{
            input_required = @{ session = $inputClose; stop = $inputStop }
            request_log = @{ session = $logClose; stop = $logStop }
            server_request = @{ session = $serverRequestClose; stop = $serverRequestStop }
        }
    }
}

function Invoke-LifecycleReplacementScenario {
    $captureRelative = "artifacts/captures/lifecycle-loss-after-result.ndjson"
    $capturePath = Join-Path -Path $script:OutputDirFull -ChildPath $captureRelative
    $session = $null
    $close = $null
    $stop = $null
    $replacementStatus = $null
    $observed = $null
    try {
        $session = Start-RpcSession -Label "lifecycle-loss-after-result" -Arguments @($script:Policy, $script:ModernFixture) -CapturePath $capturePath -Mode "loss_after_result"
        $terminal = New-ModernFrame -ID 5101 -Method "tools/list"
        Send-RpcFrame -Session $session -Frame $terminal
        $terminalRead = Read-RpcUntil -Session $session -ExpectedID 5101 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $terminalRead.response -ExpectedID 5101 -Label "loss-after-result terminal response"

        # The replacement fixture truncates its capture before it accepts a
        # frame. Seeing it remain empty is built-surface proof that no legacy
        # bootstrap/cache/replay reached generation two before fresh host input.
        [void] (Wait-ForFileLength -Path $capturePath -ExpectedLength 0 -TimeoutSeconds 15 -Label "same-era replacement capture reset")
        $quietUntil = [DateTime]::UtcNow.AddMilliseconds(250)
        while ([DateTime]::UtcNow -lt $quietUntil) {
            Assert-Verification ((Get-Item -LiteralPath $capturePath).Length -eq 0) "replacement received bootstrap/cache/replay traffic before fresh host input"
            Start-Sleep -Milliseconds 25
        }

        $replacementStatus = Get-StatusSnapshot -Label "lifecycle-replacement-before-fresh-request"
        $replacementOwner = Assert-ModernStatus -Snapshot $replacementStatus -Label "same-era replacement status"
        Assert-Verification ((Get-Item -LiteralPath $capturePath).Length -eq 0) "status inspection caused prohibited replacement upstream traffic"

        $fresh = New-ModernFrame -ID 5102 -Method "tools/list"
        Send-RpcFrame -Session $session -Frame $fresh
        $freshRead = Read-RpcUntil -Session $session -ExpectedID 5102 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $freshRead.response -ExpectedID 5102 -Label "same-era replacement fresh request"
        $observed = [ordered]@{
            terminal_response = $terminalRead.response
            terminal_response_raw = $terminalRead.response_raw
            replacement_capture_empty_before_fresh_request = $true
            replacement_status_owner = $replacementOwner
            status_command = $replacementStatus.command
            fresh_request = $fresh
            fresh_response = $freshRead.response
            fresh_response_raw = $freshRead.response_raw
            capture = $captureRelative
            command = $session.command
        }
    }
    finally {
        try {
            $stop = Stop-IsolatedDaemon -Reason "lifecycle-loss-after-result"
        }
        finally {
            if ($null -ne $session) {
                try {
                    $close = Close-RpcSession -Session $session
                }
                catch {
                    $close = [ordered]@{ cleanup_error = $_.Exception.Message }
                }
            }
        }
    }
    $observed["cleanup"] = [ordered]@{ stop = $stop; shim = $close }
    return $observed
}

function Invoke-MRTRScenario {
    $mrtr = Invoke-NativeMRTRAndDirectionalityScenario
    $artifact = [ordered]@{
        schema_version = 1
        verdict = "PASS"
        native_mrtr_directionality = $mrtr
        loss_after_result_same_era_replacement = $null
    }
    $saved = Save-Artifact -Name "lifecycle" -Value $artifact
    return [ordered]@{
        artifact = $saved.relative_path
        artifact_sha256 = $saved.sha256
        commands = @($mrtr.subcases | ForEach-Object { $_.command })
        native_mrtr_subcase_count = @($mrtr.subcases).Count
        mrtr = $mrtr
    }
}

function Invoke-LifecycleScenario {
    param([object] $MRTR)

    Assert-Verification ($null -ne $MRTR) "scenario 5 requires the completed native MRTR evidence from scenario 4"
    $replacement = Invoke-LifecycleReplacementScenario
    $artifact = [ordered]@{
        schema_version = 1
        verdict = "PASS"
        native_mrtr_directionality = $MRTR
        loss_after_result_same_era_replacement = $replacement
    }
    $saved = Save-Artifact -Name "lifecycle" -Value $artifact
    return [ordered]@{
        artifact = $saved.relative_path
        artifact_sha256 = $saved.sha256
        commands = @($MRTR.subcases | ForEach-Object { $_.command }) + @($replacement.command, $replacement.status_command, $replacement.cleanup.stop.command)
        native_mrtr_subcase_count = @($MRTR.subcases).Count
        replacement_observed = $true
    }
}

function Invoke-ReadbackScenario {
    $captureRelative = "artifacts/captures/readback-redaction.ndjson"
    $capturePath = Join-Path -Path $script:OutputDirFull -ChildPath $captureRelative
    $sentinels = @(
        "r1-request-payload-sentinel",
        "r1-opaque-state-sentinel",
        "r1-credential-sentinel",
        "r1-progress-token-sentinel",
        "r1-subscription-id-sentinel",
        "r1-compatibility-key-sentinel"
    )
    $session = $null
    $close = $null
    $stop = $null
    try {
        $session = Start-RpcSession -Label "readback-redaction" -Arguments @($script:Policy, $script:ModernFixture) -CapturePath $capturePath
        $request = New-ModernFrame -ID 6101 -Method "tools/call" -AdditionalParams @{
            name = "modern_echo"
            arguments = @{ message = $sentinels[0] }
            requestState = $sentinels[1]
            credentialHint = $sentinels[2]
            progressToken = $sentinels[3]
            subscriptionId = $sentinels[4]
            compatibilityKey = $sentinels[5]
        }
        Send-RpcFrame -Session $session -Frame $request
        $read = Read-RpcUntil -Session $session -ExpectedID 6101 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $read.response -ExpectedID 6101 -Label "readback setup request"
        [void] (Assert-ExactCapture -Path $capturePath -ExpectedFrames @($request) -Label "readback setup")

        $status = Get-StatusSnapshot -Label "active-modern-redaction"
        $owner = Assert-ModernStatus -Snapshot $status -Label "active modern status"
        Assert-TextExcludesSentinels -Text ($status.stdout + "`n" + $status.stderr) -Sentinels $sentinels -Label "active modern status"
        $artifact = [ordered]@{
            schema_version = 1
            verdict = "PASS"
            owner_info = $owner
            readiness = @{ upstream_live = (Get-JsonProperty -Object $owner -Name "upstream_live") }
            status_stdout = $status.stdout
            status_stderr = $status.stderr
            redaction_sentinels = $sentinels
            capture = $captureRelative
            request_response = $read.response
            command = $session.command
            status_command = $status.command
        }
        $saved = Save-Artifact -Name "readback" -Value $artifact
        return [ordered]@{
            artifact = $saved.relative_path
            artifact_sha256 = $saved.sha256
            commands = @($session.command, $status.command)
            readiness = (Get-JsonProperty -Object $owner -Name "upstream_live")
            redaction_sentinel_count = $sentinels.Count
        }
    }
    finally {
        try {
            $stop = Stop-IsolatedDaemon -Reason "readback-redaction"
        }
        finally {
            if ($null -ne $session) {
                try {
                    $close = Close-RpcSession -Session $session
                }
                catch {
                    $close = [ordered]@{ cleanup_error = $_.Exception.Message }
                }
            }
        }
    }
}

function Invoke-LegacyScenario {
    $session = $null
    $close = $null
    $stop = $null
    try {
        $session = Start-RpcSession -Label "legacy-parity" -Arguments @($script:LegacyFixture)
        $initialize = New-LegacyFrame -ID 7101 -Method "initialize" -Params @{
            protocolVersion = "2025-11-25"
            capabilities = @{}
            clientInfo = @{ name = "r1-windows-runner"; version = "1.0.0" }
        }
        Send-RpcFrame -Session $session -Frame $initialize
        $initializeRead = Read-RpcUntil -Session $session -ExpectedID 7101 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $initializeRead.response -ExpectedID 7101 -Label "legacy initialize"
        $initializeResult = Get-JsonProperty -Object $initializeRead.response -Name "result"
        Assert-Verification ((Get-JsonProperty -Object $initializeResult -Name "protocolVersion") -ceq "2025-11-25") "legacy initialize protocol changed"
        $serverInfo = Get-JsonProperty -Object $initializeResult -Name "serverInfo"
        Assert-Verification ((Get-JsonProperty -Object $serverInfo -Name "name") -ceq "mock-server") "legacy initialize server identity changed"

        Send-RpcFrame -Session $session -Frame (New-LegacyNotification -Method "notifications/initialized")
        $toolsList = New-LegacyFrame -ID 7102 -Method "tools/list"
        Send-RpcFrame -Session $session -Frame $toolsList
        $toolsRead = Read-RpcUntil -Session $session -ExpectedID 7102 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $toolsRead.response -ExpectedID 7102 -Label "legacy tools/list"
        $tools = @(Get-JsonProperty -Object (Get-JsonProperty -Object $toolsRead.response -Name "result") -Name "tools")
        Assert-Verification (($tools | ForEach-Object { Get-JsonProperty -Object $_ -Name "name" }) -contains "echo") "legacy tools/list no longer exposes echo"

        $toolCall = New-LegacyFrame -ID 7103 -Method "tools/call" -Params @{ name = "echo"; arguments = @{ message = "legacy deterministic" } }
        Send-RpcFrame -Session $session -Frame $toolCall
        $callRead = Read-RpcUntil -Session $session -ExpectedID 7103 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $callRead.response -ExpectedID 7103 -Label "legacy tools/call"
        $content = @(Get-JsonProperty -Object (Get-JsonProperty -Object $callRead.response -Name "result") -Name "content")
        Assert-Verification (($content.Count -gt 0) -and ((Get-JsonProperty -Object $content[0] -Name "text") -like "Tool echo called with args:*") ) "legacy tools/call output changed"

        $status = Get-StatusSnapshot -Label "legacy-parity"
        Assert-NoModernStatus -Snapshot $status -Label "legacy status"
        Assert-NoModernPolicyFields -StatusJson $status.json -Label "legacy status"
        $legacyOwners = @(Get-StatusOwnersForCommand -StatusJson $status.json -Command $script:LegacyFixture)
        Assert-Verification ($legacyOwners.Count -eq 1) "legacy status did not identify exactly one legacy fixture owner"
        $identity = [ordered]@{
            command = Get-JsonProperty -Object $legacyOwners[0] -Name "command"
            args = @(Get-JsonProperty -Object $legacyOwners[0] -Name "args")
            server_id = Get-JsonProperty -Object $legacyOwners[0] -Name "server_id"
            has_modern_policy_fields = $false
        }
        $deterministicOutput = $initializeRead.response_raw + "`n" + $toolsRead.response_raw + "`n" + $callRead.response_raw
        $artifact = [ordered]@{
            schema_version = 1
            verdict = "PASS"
            identity = $identity
            identity_sha256 = Get-TextSha256 (ConvertTo-CompactJson $identity)
            output_sha256 = Get-TextSha256 $deterministicOutput
            initialize_response = $initializeRead.response
            tools_list_response = $toolsRead.response
            tools_call_response = $callRead.response
            status_stdout = $status.stdout
            status_stderr = $status.stderr
            command = $session.command
            status_command = $status.command
        }
        $saved = Save-Artifact -Name "legacy" -Value $artifact
        return [ordered]@{
            artifact = $saved.relative_path
            artifact_sha256 = $saved.sha256
            commands = @($session.command, $status.command)
            identity_sha256 = $artifact.identity_sha256
            output_sha256 = $artifact.output_sha256
        }
    }
    finally {
        try {
            $stop = Stop-IsolatedDaemon -Reason "legacy-parity"
        }
        finally {
            if ($null -ne $session) {
                try {
                    $close = Close-RpcSession -Session $session
                }
                catch {
                    $close = [ordered]@{ cleanup_error = $_.Exception.Message }
                }
            }
        }
    }
}

function Invoke-RollbackScenario {
    $captureRelative = "artifacts/captures/rollback-modern.ndjson"
    $capturePath = Join-Path -Path $script:OutputDirFull -ChildPath $captureRelative
    $session = $null
    $close = $null
    try {
        $session = Start-RpcSession -Label "rollback-modern" -Arguments @($script:Policy, $script:ModernFixture) -CapturePath $capturePath
        $opening = New-ModernFrame -ID 8101 -Method "tools/list"
        Send-RpcFrame -Session $session -Frame $opening
        $read = Read-RpcUntil -Session $session -ExpectedID 8101 -TimeoutSeconds 20
        Assert-RpcSuccess -Response $read.response -ExpectedID 8101 -Label "rollback modern admission"
        [void] (Assert-ExactCapture -Path $capturePath -ExpectedFrames @($opening) -Label "rollback modern admission")

        # No new modern traffic is written after this active-owner snapshot.
        # Close only the input writer, force-stop the scoped daemon, then wait
        # on the owned shim as cleanup rather than as proof of behavior.
        $before = Get-StatusSnapshot -Label "rollback-before-stop"
        $ownerBefore = Assert-ModernStatus -Snapshot $before -Label "rollback status before stop"
        $sessionCommand = $session.command
        try {
            $session.process.StandardInput.Close()
        }
        catch {
        }
        $stop = Stop-IsolatedDaemon -Reason "rollback-force"
        $close = Close-RpcSession -Session $session
        $session = $null
        $after = Get-StatusSnapshot -Label "rollback-after-stop"
        Assert-NoModernStatus -Snapshot $after -Label "rollback status after stop"
        $remainingFixtureOwners = @(Get-StatusOwnersForCommand -StatusJson $after.json -Command $script:ModernFixture)
        Assert-Verification ($remainingFixtureOwners.Count -eq 0) "rollback left a legacy or downgraded modern fixture owner"

        $artifact = [ordered]@{
            schema_version = 1
            verdict = "PASS"
            admissions_stopped_after_status = $true
            owner_before_stop = $ownerBefore
            status_before_stop = $before
            status_after_stop = $after
            stop = $stop
            opening = $opening
            response = $read.response
            capture = $captureRelative
            command = $sessionCommand
            status_commands = @($before.command, $after.command)
            no_modern_owner_after_stop = $true
            no_downgrade_or_replay_observed = $true
        }
        $saved = Save-Artifact -Name "rollback" -Value $artifact
        return [ordered]@{
            artifact = $saved.relative_path
            artifact_sha256 = $saved.sha256
            commands = @($before.command, $stop.command, $after.command)
            owner_identified_before_stop = $true
            no_modern_owner_after_stop = $true
        }
    }
    finally {
        if ($null -ne $session) {
            $close = Close-RpcSession -Session $session
        }
        [void] (Stop-IsolatedDaemon -Reason "rollback-final")
    }
}

function Complete-ProductCleanup {
    foreach ($session in @($script:ActiveSessions.ToArray())) {
        [void] (Close-RpcSession -Session $session)
    }
    $stop = Stop-IsolatedDaemon -Reason "final-cleanup"
    $status = Get-StatusSnapshot -Label "final-cleanup"
    Assert-NoModernStatus -Snapshot $status -Label "final cleanup status"
    $script:CleanupCompleted = $true
    return [ordered]@{
        stop = $stop
        status = $status
    }
}

function Write-Summary {
    param(
        [object] $Cleanup
    )

    $artifactHashes = [ordered]@{}
    foreach ($name in $script:ArtifactRefs.Keys) {
        $path = Join-Path -Path $script:OutputDirFull -ChildPath $script:ArtifactRefs[$name]
        Assert-Verification (Test-NonEmptyFile $path) "required artifact is missing or empty: $($script:ArtifactRefs[$name])"
        $artifactHashes[$name] = Get-FileSha256 $path
    }
    Flush-Transcript
    Close-Transcript
    Assert-Verification (Test-NonEmptyFile $script:TranscriptPath) "transcript.ndjson is missing or empty"
    $transcriptHash = Get-FileSha256 $script:TranscriptPath

    $manifest = [ordered]@{
        schema_version = 1
        transcript_sha256 = $transcriptHash
        artifacts_sha256 = $artifactHashes
        cleanup = $Cleanup
    }
    [void] (Save-Artifact -Name "evidence-hashes" -Value $manifest)

    if (-not $KeepBaseDir) {
        Remove-Item -LiteralPath $script:BaseDir -Recurse -Force -ErrorAction Stop
        $script:BaseRemoved = $true
    }

    $platformArchitecture = if ([System.Environment]::Is64BitOperatingSystem) { "x64" } else { "x86" }
    $summary = [ordered]@{
        schema_version = 1
        result = "PASS"
        platform_id = "windows-$([System.Environment]::OSVersion.Version)-$platformArchitecture-powershell-$($PSVersionTable.PSVersion)"
        source_sha = $script:SourceSHA
        binary_sha256 = $script:BinarySHA
        fixture_sha256 = $script:ModernFixtureSHA
        corpus_sha256 = $script:CorpusSHA
        corpus_total = 100
        corpus_passed = 100
        policy = $script:Policy
        scenario_results = $script:ScenarioResults
        artifacts = $script:ArtifactRefs
        base_dir = "base"
        base_dir_preserved = [bool] $KeepBaseDir
        base_dir_removed = $script:BaseRemoved
        binary_path = "base/bin/mcp-mux.exe"
        fixture_path = "base/bin/mock_modern_server.exe"
        legacy_fixture_path = "base/bin/mock_server.exe"
        legacy_fixture_sha256 = $script:LegacyFixtureSHA
        transcript_sha256 = $transcriptHash
        evidence_hash_manifest = "artifacts/evidence-hashes.json"
        operating_system = [ordered]@{
            version = [System.Environment]::OSVersion.Version.ToString()
            is_64_bit = [System.Environment]::Is64BitOperatingSystem
            powershell_edition = $PSVersionTable.PSEdition
            powershell_version = $PSVersionTable.PSVersion.ToString()
        }
        go = @{ version = $script:GoVersion; executable = $script:GoPath }
    }
    Write-JsonFile -Path (Join-Path -Path $script:OutputDirFull -ChildPath "summary.json") -Value $summary
    Assert-Verification (Test-NonEmptyFile (Join-Path -Path $script:OutputDirFull -ChildPath "summary.json")) "summary.json is missing or empty after write"
}

$exitCode = 0
$exitMessage = ""
try {
    Initialize-Runner

    $modernCorpus = Invoke-Scenario -ID 1 -Name "direct corpus openers" -Expected @{ every_direct_frame = "native response and byte-identical first upstream line" } -ArtifactRef $script:ArtifactRefs.modern -Action {
        # Scenario 1 and 2 share one deterministic 100-frame artifact. The
        # action runs every frame; the next transcript record names its exact
        # host-sent discovery partition and command evidence.
        Invoke-ModernCorpusScenarios
    }
    $script:ScenarioResults["2"] = "PASS"
    Add-Transcript -ScenarioID 2 -Scenario "host-sent server/discover corpus openers" -Expected @{ every_discovery_frame = "forwarded unchanged without manufactured discovery" } -Observed @{ artifact = $script:ArtifactRefs.modern; corpus_total = $modernCorpus.corpus_total; discover_count = $modernCorpus.discover_count; direct_count = $modernCorpus.direct_count; client_info_present_count = $modernCorpus.client_info_present_count; client_info_absent_count = $modernCorpus.client_info_absent_count } -Verdict "PASS" -ArtifactRef $script:ArtifactRefs.modern -Commands $modernCorpus.commands

    [void] (Invoke-Scenario -ID 3 -Name "strict modern admission" -Expected @{ malformed = -32602; unsupported_version = -32022; upstream = "not started" } -ArtifactRef $script:ArtifactRefs.admission -Action {
        Invoke-StrictAdmissionScenario
    })

    $scenarioFour = Invoke-Scenario -ID 4 -Name "native MRTR directionality and logging" -Expected @{ input_required = "native"; retry = "fresh ID"; log = "once"; server_request = "contained" } -ArtifactRef $script:ArtifactRefs.lifecycle -Action {
        Invoke-MRTRScenario
    }
    $script:MRTRScenarioEvidence = Get-JsonProperty -Object $scenarioFour -Name "mrtr"

    [void] (Invoke-Scenario -ID 5 -Name "lifecycle quarantine" -Expected @{ loss_after_result = "same-era fresh request"; bootstrap_replay = "absent"; cleanup = "built stop --force" } -ArtifactRef $script:ArtifactRefs.lifecycle -Action {
        Invoke-LifecycleScenario -MRTR $script:MRTRScenarioEvidence
    })

    [void] (Invoke-Scenario -ID 6 -Name "minimal R1 readback and redaction" -Expected @{ policy_facts = @("protocol_era", "sharing_policy", "cache_policy", "lifecycle_policy"); readiness = "upstream_live"; redaction = "sentinels absent" } -ArtifactRef $script:ArtifactRefs.readback -Action {
        Invoke-ReadbackScenario
    })

    [void] (Invoke-Scenario -ID 7 -Name "legacy parity" -Expected @{ selector = "omitted"; initialize = "legacy"; modern_policy_fields = "absent" } -ArtifactRef $script:ArtifactRefs.legacy -Action {
        Invoke-LegacyScenario
    })

    [void] (Invoke-Scenario -ID 8 -Name "operator rollback" -Expected @{ status_identifies_modern_owner = $true; stop_force = "built artifact"; remaining_modern_owners = 0 } -ArtifactRef $script:ArtifactRefs.rollback -Action {
        Invoke-RollbackScenario
    })

    $cleanup = Complete-ProductCleanup
    Add-Transcript -ScenarioID 0 -Scenario "final-cleanup" -Expected @{ daemon = "stopped"; modern_owners = 0 } -Observed $cleanup -Verdict "PASS" -Commands @($cleanup.stop.command, $cleanup.status.command)
    Write-Summary -Cleanup $cleanup
}
catch {
    $exitCode = 1
    if ($_.Exception.Data.Contains("runner_exit_code")) {
        $exitCode = [int] $_.Exception.Data["runner_exit_code"]
    }
    $exitMessage = $_.Exception.Message
    try {
        Add-Transcript -ScenarioID 0 -Scenario "runner-failure" -Expected @{ result = "PASS" } -Observed @{ error = $exitMessage; exit_code = $exitCode } -Verdict "FAIL"
    }
    catch {
    }
}
finally {
    try {
        foreach ($session in @($script:ActiveSessions.ToArray())) {
            [void] (Close-RpcSession -Session $session)
        }
        if (-not $script:CleanupCompleted -and $null -ne $script:MuxBinary -and (Test-Path -LiteralPath $script:MuxBinary -PathType Leaf)) {
            $cleanup = Stop-IsolatedDaemon -Reason "failure-cleanup" -BestEffort
            Add-Transcript -ScenarioID 0 -Scenario "failure-cleanup" -Expected @{ daemon = "stopped" } -Observed $cleanup -Verdict "PASS" -Commands @($cleanup.command)
        }
    }
    catch {
        if ($exitCode -eq 0) {
            $exitCode = 1
            $exitMessage = "cleanup failed: $($_.Exception.Message)"
        }
    }
    finally {
        Close-Transcript
    }
}

if ($exitCode -eq 0) {
    Write-Step "PASS: customer evidence written to $script:OutputDirFull"
}
else {
    $label = if ($exitCode -eq 2) { "SETUP ERROR" } else { "FAIL" }
    Write-Host "[$script:RunnerName] ${label}: $exitMessage"
}

exit $exitCode
