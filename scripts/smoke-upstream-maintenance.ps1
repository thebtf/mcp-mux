#Requires -Version 7.0
[CmdletBinding()]
param(
    [string]$SourceRoot = ".",
    [Parameter(Mandatory = $true)][string]$CandidateBinary,
    [Parameter(Mandatory = $true)][string]$OutputDir,
    [int]$TimeoutSeconds = 180,
    [Parameter(Mandatory = $true)][string]$ScratchRoot,
    [Parameter(Mandatory = $true)][string]$EvidencePath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
$Utf8 = [System.Text.UTF8Encoding]::new($false)
$Contexts = [System.Collections.Generic.List[object]]::new()
$Sessions = [System.Collections.Generic.List[object]]::new()
$Identities = [System.Collections.Generic.List[object]]::new()
$Checks = [System.Collections.Generic.List[object]]::new()
$Commands = [System.Collections.Generic.List[object]]::new()
$Secrets = @([guid]::NewGuid().ToString("N"))
$PrivateMarkers = [System.Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
$Transcript = $null
$ExitCode = 1
$Evidence = [ordered]@{
    verdict = "NOT_RUN"; platform = [System.Runtime.InteropServices.RuntimeInformation]::OSDescription
    source_root = ""; source_sha = ""; candidate_sha256 = ""; fixture_hashes = @{}
    output_dir = ""; checks = @(); commands = @(); host_pipes = @(); process_identities = @()
    cleanup = @(); error = ""
    scope = "Live managed upstream replacement, not full feature/release acceptance."
    required_focused_proof = @(
        "Deterministic queue/reconnect disposition, start-through-install, and hold-versus-activation races",
        "Blocked retirement never TTL-clears; corrupt/incomplete ledger and persistence failures",
        "Unsupported optional maintenance handler and old daemon without fallback",
        "Library update-helper terminal restart/handoff/shutdown refusal",
        "Finite CWD/era/security/namespace context sets and stale generation callbacks",
        "Read-only status/startup checks; locked persisted-clear offline/old-endpoint activation; duration boundaries"
    )
}

function Assert-Observation([bool]$Condition, [string]$Name, $Observed) {
    $row = [ordered]@{ utc = [DateTime]::UtcNow.ToString("o"); name = $Name; passed = $Condition; observed = $Observed }
    $Checks.Add($row)
    Write-Trace "observation" $row
    if (-not $Condition) { throw "Observation failed: $Name" }
}
function Protect-Text([string]$Text) {
    foreach ($secret in $Secrets) { $Text = $Text.Replace($secret, "[REDACTED]") }
    return $Text
}
function Write-Trace([string]$Kind, $Data) {
    if ($null -ne $Transcript) {
        $line = @{ utc = [DateTime]::UtcNow.ToString("o"); kind = $Kind; data = $Data } | ConvertTo-Json -Depth 40 -Compress
        $Transcript.WriteLine((Protect-Text $line))
        $Transcript.Flush()
    }
}
function Assert-PrivateReadback([string]$Text) {
    foreach ($secret in $Secrets) {
        Assert-Observation (-not $Text.Contains($secret)) "Readback excludes private fixture credential" "value withheld"
    }
    foreach ($marker in $PrivateMarkers) {
        Assert-Observation (-not $Text.Contains($marker)) "Readback excludes request marker $marker" "request body withheld"
    }
}
function Test-Beneath([string]$Path, [string]$Root) {
    $comparison = if ($IsWindows) { [StringComparison]::OrdinalIgnoreCase } else { [StringComparison]::Ordinal }
    return $Path.StartsWith($Root.TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar, $comparison)
}
function Assert-NoLinks([string]$Path, [string]$Root) {
    $current = $Path
    while ($current.Length -ge $Root.Length) {
        if (Test-Path -LiteralPath $current) {
            $item = Get-Item -LiteralPath $current -Force
            if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw "Linked scratch path is not allowed: $current" }
        }
        if ($current -eq $Root) { break }
        $current = Split-Path -Parent $current
    }
}
function New-StartInfo([string]$Executable, [string[]]$Arguments, $Context, [hashtable]$Overrides = @{}) {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $Executable
    $info.WorkingDirectory = $SourceRoot
    $info.UseShellExecute = $false
    $info.RedirectStandardInput = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    foreach ($argument in $Arguments) { $info.ArgumentList.Add($argument) }
    foreach ($name in @(
        "MCP_MUX_NO_DAEMON", "MCP_MUX_DAEMON", "MCP_MUX_ISOLATED", "MCP_MUX_STATELESS", "MCP_MUX_DEFAULT_MODE",
        "MCPMUX_ENGINE", "MCPMUX_DISABLE_LAUNCHER", "MCPMUX_LAUNCHER_EXE", "MCPMUX_LAUNCHER_OWNS_DAEMON",
        "MCPMUX_LAUNCHER_PROTOCOL", "MCPMUX_LAUNCHER_ATTESTATION", "MCPMUX_ACTIVE_ENGINE_FILE", "MCPMUX_SUCCESSOR_EXE",
        "MCPMUX_HANDOFF_SOCKET", "MCPMUX_HANDOFF_TOKEN_PATH", "MCPMUX_SNAPSHOT_RESTART", "MCP_MUX_MODERN_MODE"
    )) { [void]$info.Environment.Remove($name) }
    $environment = @{
        TEMP = $Context.runtime; TMP = $Context.runtime; TMPDIR = $Context.runtime
        APPDATA = $Context.config; LOCALAPPDATA = $Context.config; XDG_CONFIG_HOME = $Context.config
        HOME = $Context.home; USERPROFILE = $Context.home
        GOCACHE = (Join-Path $OutputDir "go-cache"); GOTMPDIR = (Join-Path $OutputDir "go-tmp")
        GOMODCACHE = (Join-Path $OutputDir "go-modcache"); GO111MODULE = "off"
        GOPATH = (Join-Path $OutputDir "go-path")
        MCP_MUX_LIFECYCLE_FIXTURE_ROOT = $Context.records
        MCP_MUX_LIFECYCLE_FIXTURE_SHARED = "1"; MCP_MUX_LIFECYCLE_FIXTURE_TRACE = "1"
        MCP_MUX_MODERN_CAPTURE_FILE = $Context.modern_capture
        MCP_MUX_OWNER_IDLE = "10m"; MCP_MUX_IDLE_TIMEOUT = "10m"
        MCPMUX_SHIM_IDLE_TIMEOUT = "0s"; MCPMUX_SHIM_DORMANT_GRACE = "0s"
        GITHUB_TOKEN = $Secrets[0]
    }
    foreach ($name in $Overrides.Keys) { $environment[$name] = $Overrides[$name] }
    foreach ($name in $environment.Keys) { $info.Environment[$name] = $environment[$name] }
    return $info
}
function Start-Native([string]$Executable, [string[]]$Arguments, $Context, [hashtable]$Overrides = @{}) {
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = New-StartInfo $Executable $Arguments $Context $Overrides
    if (-not $process.Start()) { throw "Cannot start $Executable" }
    $command = [ordered]@{ executable = $Executable; arguments = $Arguments; namespace = $Context.name; started_utc = [DateTime]::UtcNow.ToString("o"); pid = $process.Id }
    $Commands.Add($command)
    Write-Trace "command-start" $command
    return @{ process = $process; command = $command; stdout = $process.StandardOutput.ReadToEndAsync(); stderr = $process.StandardError.ReadToEndAsync() }
}
function Finish-Native($Job, [int]$WaitSeconds = 15, [bool]$KillOnTimeout = $true, [DateTime]$WaitDeadline = $Deadline) {
    $remaining = [Math]::Min($WaitSeconds * 1000, [Math]::Max(1, ([Math]::Min($Deadline.Ticks, $WaitDeadline.Ticks) - [DateTime]::UtcNow.Ticks) / [TimeSpan]::TicksPerMillisecond))
    if (-not $Job.process.WaitForExit([int]$remaining)) {
        if ($KillOnTimeout) {
            $Job.process.Kill($true)
            [void]$Job.process.WaitForExit(5000)
        }
        $Job.command["timed_out"] = $true
        Write-Trace "command-timeout" $Job.command
        throw "Timed out: $($Job.command.executable) $($Job.command.arguments -join ' ')"
    }
    $result = @{ exit_code = $Job.process.ExitCode; stdout = $Job.stdout.Result; stderr = $Job.stderr.Result }
    $Job.command["exit_code"] = $result.exit_code
    $Job.command["finished_utc"] = [DateTime]::UtcNow.ToString("o")
    $Job.command["stdout"] = Protect-Text $result.stdout
    $Job.command["stderr"] = Protect-Text $result.stderr
    Write-Trace "command-result" $Job.command
    $Job.process.Dispose()
    return $result
}
function Invoke-Native([string]$Executable, [string[]]$Arguments, $Context, [int]$WaitSeconds = 15) {
    return Finish-Native (Start-Native $Executable $Arguments $Context) $WaitSeconds
}
function Remember-Lease($Context, $Lease) {
    if ($null -eq $Lease -or -not $Lease.ContainsKey("hold_id") -or -not $Lease.ContainsKey("server_id") -or [string]::IsNullOrWhiteSpace($Lease.hold_id) -or [string]::IsNullOrWhiteSpace($Lease.server_id)) { return }
    for ($i = 0; $i -lt $Context.holds.Count; $i++) {
        if ($Context.holds[$i].hold_id -ceq $Lease.hold_id) { $Context.holds[$i] = $Lease; return }
    }
    $Context.holds.Add($Lease)
    Write-Trace "lease-owned" @{ namespace = $Context.name; control_path = $Context.control_path; lease = $Lease }
}
function Invoke-CLI($Context, [string[]]$Arguments, [switch]$Refusal) {
    $result = Invoke-Native $Launcher $Arguments $Context
    if (-not $result.stdout.Trim().StartsWith("{")) { throw "CLI $($Arguments[0]) did not return one JSON object" }
    $decoded = $result.stdout | ConvertFrom-Json -AsHashtable
    if ($Arguments[0] -in @("hold", "renew") -and $decoded.ContainsKey("maintenance")) { Remember-Lease $Context $decoded.maintenance }
    Assert-PrivateReadback ($result.stdout + $result.stderr)
    Assert-Observation (($result.exit_code -ne 0) -eq $Refusal.IsPresent) "CLI exit: $($Arguments[0])" $result.exit_code
    return $decoded
}
function New-Context([string]$Name) {
    $root = Join-Path $OutputDir $Name
    $ctx = @{ name = $Name; runtime = (Join-Path $root "r"); config = (Join-Path $root "cfg"); home = (Join-Path $root "home"); records = (Join-Path $root "records"); modern_capture = (Join-Path $root "modern.ndjson"); holds = [System.Collections.Generic.List[object]]::new() }
    foreach ($directory in @($ctx.runtime, $ctx.config, $ctx.home, $ctx.records)) { [void][IO.Directory]::CreateDirectory($directory) }
    $ctx["control_path"] = Join-Path $ctx.runtime "mcp-mux-muxd.ctl.sock"
    if (-not $IsWindows -and $Utf8.GetByteCount($ctx.control_path) -gt 103) { throw "Scratch path is too long for a Unix control socket; choose a shorter primary scratch directory" }
    if (-not $IsWindows) {
        $result = Invoke-Native $Chmod (@("700", $root, $ctx.runtime, $ctx.config, $ctx.home, $ctx.records)) $ctx
        if ($result.exit_code -ne 0) { throw "Cannot restrict owned Unix runtime/config directories" }
    }
    $Contexts.Add($ctx)
    return $ctx
}
function Invoke-Control($Context, [hashtable]$Request) {
    $stream = $null
    $socket = $null
    try {
        if ($IsWindows) {
            $algorithm = [Security.Cryptography.SHA256]::Create()
            try { $hash = $algorithm.ComputeHash($Utf8.GetBytes($Context.control_path.ToLowerInvariant())) } finally { $algorithm.Dispose() }
            $pipe = "mcp-mux-" + ([BitConverter]::ToString($hash[0..15])).Replace("-", "").ToLowerInvariant()
            $stream = [IO.Pipes.NamedPipeClientStream]::new(".", $pipe, [IO.Pipes.PipeDirection]::InOut, [IO.Pipes.PipeOptions]::Asynchronous)
            $stream.Connect(5000)
        } else {
            $socket = [Net.Sockets.Socket]::new([Net.Sockets.AddressFamily]::Unix, [Net.Sockets.SocketType]::Stream, [Net.Sockets.ProtocolType]::Unspecified)
            $task = $socket.ConnectAsync([Net.Sockets.UnixDomainSocketEndPoint]::new($Context.control_path))
            if (-not $task.Wait(5000)) { throw "Private control connection timed out" }
            $stream = [Net.Sockets.NetworkStream]::new($socket, $true)
        }
        $writer = [IO.StreamWriter]::new($stream, $Utf8, 4096, $true)
        $reader = [IO.StreamReader]::new($stream, $Utf8, $false, 4096, $true)
        try {
            $writer.WriteLine(($Request | ConvertTo-Json -Depth 16 -Compress)); $writer.Flush()
            $readTask = $reader.ReadLineAsync()
            if (-not $readTask.Wait(10000)) { throw "Private control reply timed out" }
            $raw = $readTask.Result
            if ($null -eq $raw) { throw "Private control closed without a reply" }
            Assert-PrivateReadback $raw
            Write-Trace "control" @{ namespace = $Context.name; request = $Request; response = (Protect-Text $raw) }
            return ($raw | ConvertFrom-Json -AsHashtable)
        } finally { $reader.Dispose(); $writer.Dispose() }
    } finally {
        if ($null -ne $stream) { $stream.Dispose() }
        if ($null -ne $socket) { $socket.Dispose() }
    }
}
function Get-Status($Context) {
    $status = Invoke-CLI $Context @("status")
    Assert-Observation ($status["daemon"] -eq $true -and [int]$status["pid"] -gt 0 -and -not [string]::IsNullOrWhiteSpace($status["daemon_generation"])) "Private daemon status is authoritative" @{ namespace = $Context.name; pid = $status["pid"]; daemon_generation = $status["daemon_generation"] }
    return $status
}
function Get-Owner($Context, [string]$Command) {
    $status = Get-Status $Context
    $owners = @($status["servers"] | Where-Object { $_["command"] -eq $Command })
    Assert-Observation ($owners.Count -eq 1 -and -not [string]::IsNullOrWhiteSpace($owners[0]["server_id"])) "One exact managed target" @{ command = $Command; matches = $owners.Count }
    return $owners[0]
}
function Read-ProcessIdentity([Diagnostics.Process]$Process, [string]$Label, [switch]$Startup, [long]$ExpectedStartTicks = 0, [string]$ExpectedExecutable = "") {
    $until = [DateTime]::UtcNow.AddSeconds(1)
    $waiting = $false
    while ($true) {
        $Process.Refresh()
        if ($Process.HasExited) { return $null }
        try {
            $ticks = $Process.StartTime.ToUniversalTime().Ticks
            if ($ExpectedStartTicks -ne 0 -and $ticks -ne $ExpectedStartTicks) {
                if ($Startup) { throw "Started process changed its original start time during identity capture" }
                return $null
            }
            if ($Startup -and $ExpectedStartTicks -eq 0) { $ExpectedStartTicks = $ticks }
            $sampleExecutable = $null
            $module = $Process.MainModule
            if ($null -ne $module) {
                $sampleExecutable = [IO.Path]::GetFullPath($module.FileName)
                if ($Process.HasExited) { return $null }
                if (-not $Startup -or $ExpectedExecutable -eq "" -or $sampleExecutable -eq $ExpectedExecutable) {
                    return @{ pid = $Process.Id; label = $Label; executable = $sampleExecutable; start_ticks = $ticks }
                }
            }
        } catch {
            if ($Process.HasExited) { return $null }
            throw "OS identity read failed for $Label (PID $($Process.Id), type $($Process.GetType().FullName)): $($_.Exception.GetType().FullName): $($_.Exception.Message)"
        }
        if (-not $waiting) {
            $kind = if ($null -eq $sampleExecutable) { "process-module-unavailable" } else { "process-module-mismatch" }
            Write-Trace $kind @{ label = $Label; pid = $Process.Id; start_ticks = $ticks; executable = $sampleExecutable; expected_executable = $ExpectedExecutable; process_type = $Process.GetType().FullName; has_exited = $Process.HasExited; startup = $Startup.IsPresent }
            $waiting = $true
        }
        # A just-started host must expose its exact OS executable within the same one-second window.
        if (-not $Startup -or [DateTime]::UtcNow -ge $until -or [DateTime]::UtcNow -ge $Deadline) {
            if ($Process.HasExited) { return $null }
            throw "OS executable identity unavailable for live $Label (PID $($Process.Id)): last observed module executable '$sampleExecutable', expected '$ExpectedExecutable'; no expected-path fallback used"
        }
        Start-Sleep -Milliseconds 25
    }
}
function Get-Identity([int]$ProcessId, [string]$Label, [string]$ExpectedPath = "", [Diagnostics.Process]$StartedProcess = $null) {
    $process = if ($null -ne $StartedProcess) { $StartedProcess } else { [Diagnostics.Process]::GetProcessById($ProcessId) }
    try {
        $identity = Read-ProcessIdentity $process $Label -Startup:($null -ne $StartedProcess) -ExpectedExecutable $ExpectedPath
        if ($null -eq $identity) { throw "Process exited before OS identity capture: $Label (PID $ProcessId)" }
        if ($ExpectedPath -ne "") { Assert-Observation ($identity.executable -eq $ExpectedPath) "Exact executable identity: $Label" $identity }
        $Identities.Add($identity)
        Write-Trace "process-identity" $identity
        return $identity
    } finally { if ($null -eq $StartedProcess) { $process.Dispose() } }
}
function Test-IdentityAlive($Identity) {
    try { $process = [Diagnostics.Process]::GetProcessById([int]$Identity.pid) } catch [ArgumentException] { return $false }
    try {
        $current = Read-ProcessIdentity $process $Identity.label -ExpectedStartTicks $Identity.start_ticks
        if ($null -eq $current) { return $false }
        if ($current.executable -ne $Identity.executable) { throw "Captured process changed executable without exiting" }
        return $true
    } finally { $process.Dispose() }
}
function Get-Generations($Context, [string]$Executable) {
    return @(Get-ChildItem -LiteralPath $Context.records -Filter "generation-*.json" | ForEach-Object { Get-Content -Raw -LiteralPath $_.FullName | ConvertFrom-Json -AsHashtable } | Where-Object { $_["executable"] -eq $Executable } | Sort-Object { $_["started_utc"] })
}
function Read-CaptureLines([string]$Path) {
    $stream = [IO.File]::Open($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite)
    try {
        $reader = [IO.StreamReader]::new($stream, $Utf8)
        try { $text = $reader.ReadToEnd() } finally { $reader.Dispose() }
    } finally { $stream.Dispose() }
    # An encoder may be midway through its last write; only complete NDJSON is evidence.
    $lines = $text.Split("`n")
    return @($lines | Select-Object -SkipLast 1 | ForEach-Object { $_.TrimEnd("`r") } | Where-Object { $_ -ne "" })
}
function Get-FrameMarker($Frame) {
    if (-not $Frame.ContainsKey("params") -or $null -eq $Frame["params"]) { return "" }
    $params = $Frame["params"]
    if ($params.ContainsKey("marker")) { return [string]$params["marker"] }
    if ($params.ContainsKey("arguments") -and $null -ne $params["arguments"] -and $params["arguments"].ContainsKey("marker")) { return [string]$params["arguments"]["marker"] }
    return ""
}
function Get-PipeHandle($Stream) {
    foreach ($name in @("SafePipeHandle", "SafeFileHandle")) {
        $property = $Stream.PSObject.Properties[$name]
        if ($null -ne $property) { return $property.Value.DangerousGetHandle().ToInt64() }
    }
    throw "Native pipe handle is unavailable; cannot prove original pipe identity"
}
function Get-Capture($Context) {
    $frames = [System.Collections.Generic.List[object]]::new()
    foreach ($file in @(Get-ChildItem -LiteralPath $Context.records -Filter "frames-*.ndjson")) {
        foreach ($line in @(Read-CaptureLines $file.FullName)) {
            if ($line -ne "") { $frames.Add(($line | ConvertFrom-Json -AsHashtable)) }
        }
    }
    return $frames.ToArray()
}
function Wait-CapturedMarker($Context, [string]$Marker) {
    $until = [DateTime]::UtcNow.AddSeconds(10)
    while ([DateTime]::UtcNow -lt $until -and [DateTime]::UtcNow -lt $Deadline) {
        $matches = @(Get-Capture $Context | Where-Object { $_["kind"] -eq "received" -and (Get-FrameMarker $_["frame"]) -ceq $Marker })
        if ($matches.Count -eq 1) { Write-Trace "upstream-received" $matches[0]; return $matches[0] }
        if ($matches.Count -gt 1) { throw "Upstream received marker more than once: $Marker" }
        Start-Sleep -Milliseconds 25
    }
    throw "Upstream never received marker: $Marker"
}
function Start-Host($Context, [string]$Fixture, [string]$Name, [switch]$Modern) {
    $process = [Diagnostics.Process]::new()
    $arguments = if ($Modern) { @("--mcp-protocol=2026-07-28", $Fixture) } else { @($Fixture) }
    $process.StartInfo = New-StartInfo $Launcher $arguments $Context
    if (-not $process.Start()) { throw "Host failed to start" }
    $session = @{ name = $Name; process = $process; context = $Context; pending = $null; frames = [System.Collections.Generic.List[object]]::new(); sent = @{}; received = @{}; stderr = $process.StandardError.ReadToEndAsync(); input = $process.StandardInput; output = $process.StandardOutput; modern = $Modern.IsPresent; identity = $null; input_handle = $null; output_handle = $null }
    $Sessions.Add($session)
    try {
        $session["identity"] = Get-Identity $process.Id $Name $Launcher $process
    } catch {
        $failure = Protect-Text $_.Exception.Message
        $session["identity_failure"] = $failure
        $session["failure_stdout"] = $session.output.ReadToEndAsync()
        $exited = $process.HasExited
        if ($exited) {
            [void]$session.failure_stdout.Wait(1000)
            [void]$session.stderr.Wait(1000)
        }
        $diagnostic = @{ host = $Name; pid = $process.Id; error = $failure; has_exited = $exited; exit_code = if ($exited) { $process.ExitCode } else { $null }; stdout_complete = $session.failure_stdout.IsCompleted; stderr_complete = $session.stderr.IsCompleted; stdout = if ($session.failure_stdout.IsCompletedSuccessfully) { Protect-Text $session.failure_stdout.Result } else { $null }; stderr = if ($session.stderr.IsCompletedSuccessfully) { Protect-Text $session.stderr.Result } else { $null } }
        $Evidence["host_identity_failure"] = $diagnostic
        Write-Trace "host-identity-failure" $diagnostic
        throw
    }
    $session["input_handle"] = Get-PipeHandle $session.input.BaseStream
    $session["output_handle"] = Get-PipeHandle $session.output.BaseStream
    Write-Trace "host-open" @{ name = $Name; pid = $process.Id; arguments = $arguments; stdin_handle = $session.input_handle; stdout_handle = $session.output_handle }
    return $session
}
function Send-Frame($Session, [hashtable]$Frame) {
    $marker = Get-FrameMarker $Frame
    if ($marker -ne "") { [void]$PrivateMarkers.Add($marker) }
    $raw = $Frame | ConvertTo-Json -Depth 20 -Compress
    if ($Frame.ContainsKey("id")) { $Session.sent[(Get-IDKey $Frame["id"])] = [DateTime]::UtcNow }
    Write-Trace "host-send" @{ host = $Session.name; raw = $raw }
    $Session.input.WriteLine($raw); $Session.input.Flush()
    return $raw
}
function Read-Available($Session, [int]$WaitMs = 0) {
    if ($null -eq $Session.pending) { $Session.pending = $Session.output.ReadLineAsync() }
    if (-not $Session.pending.Wait($WaitMs)) { return }
    $line = $Session.pending.Result
    $Session.pending = $null
    if ($null -eq $line) { throw "Original host output pipe closed: $($Session.name)" }
    $frame = $line | ConvertFrom-Json -AsHashtable
    if ($frame.ContainsKey("id")) { $Session.received[(Get-IDKey $frame["id"])] = [DateTime]::UtcNow }
    $Session.frames.Add($frame)
    Write-Trace "host-receive" @{ host = $Session.name; raw = $line }
}
function Get-IDKey($ID) { return ($ID | ConvertTo-Json -Compress) }
function Wait-Reply($Session, $ID, [int]$WaitSeconds = 15) {
    $until = [DateTime]::UtcNow.AddSeconds($WaitSeconds)
    $key = Get-IDKey $ID
    while ([DateTime]::UtcNow -lt $until -and [DateTime]::UtcNow -lt $Deadline) {
        $matches = @($Session.frames | Where-Object { $_.ContainsKey("id") -and (Get-IDKey $_["id"]) -ceq $key })
        if ($matches.Count -gt 0) {
            Assert-Observation ($matches.Count -eq 1) "Exactly one original-ID reply: $($Session.name) $key" $matches.Count
            return $matches[0]
        }
        Read-Available $Session 25
    }
    throw "Timed out waiting for original ID $key on $($Session.name)"
}
function Assert-OpenPipes($Session) {
    Assert-Observation ((Test-IdentityAlive $Session.identity) -and [object]::ReferenceEquals($Session.input, $Session.process.StandardInput) -and [object]::ReferenceEquals($Session.output, $Session.process.StandardOutput) -and $Session.input.BaseStream.CanWrite -and $Session.output.BaseStream.CanRead -and (Get-PipeHandle $Session.input.BaseStream) -eq $Session.input_handle -and (Get-PipeHandle $Session.output.BaseStream) -eq $Session.output_handle) "Original host and pipes remain open: $($Session.name)" @{ identity = $Session.identity; stdin_handle = $Session.input_handle; stdout_handle = $Session.output_handle }
}
function New-Probe($ID, [string]$Marker, [int]$DelayMS = 0) {
    return @{ jsonrpc = "2.0"; id = $ID; method = "tools/call"; params = @{ name = "lifecycle_probe"; arguments = @{ marker = $Marker; delay_ms = $DelayMS } } }
}
function Invoke-Probe($Session, $ID, [string]$Marker, [string]$Version) {
    [void](Send-Frame $Session (New-Probe $ID $Marker))
    $reply = Wait-Reply $Session $ID
    Assert-Observation ($reply.ContainsKey("result") -and -not $reply.ContainsKey("error")) "Fresh probe succeeds: $Marker" $reply
    $payload = $reply["result"]["content"][0]["text"] | ConvertFrom-Json -AsHashtable
    Assert-Observation ($payload["version"] -ceq $Version -and $payload["marker"] -ceq $Marker) "Executable version: $Marker" $payload
    Assert-OpenPipes $Session
    return $payload
}
function Initialize-Legacy($Session) {
    [void](Send-Frame $Session @{ jsonrpc = "2.0"; id = 1; method = "initialize"; params = @{ protocolVersion = "2025-11-25"; capabilities = @{}; clientInfo = @{ name = "maintenance-smoke"; version = "1" } } })
    $reply = Wait-Reply $Session 1
    Assert-Observation ($reply.ContainsKey("result") -and $reply["result"]["protocolVersion"] -eq "2025-11-25") "Legacy initialize succeeds" $reply
    [void](Send-Frame $Session @{ jsonrpc = "2.0"; method = "notifications/initialized"; params = @{} })
    [void](Send-Frame $Session @{ jsonrpc = "2.0"; id = 2; method = "tools/list"; params = @{} })
    $discovery = Wait-Reply $Session 2
    Assert-Observation ($discovery.ContainsKey("result") -and $discovery["result"]["tools"][0]["name"] -ceq "lifecycle_probe") "Prime cacheable discovery before maintenance" $discovery
}
function Assert-HeldReply($Session, $ID) {
    $reply = Wait-Reply $Session $ID 3
    $key = Get-IDKey $ID
    $elapsed = ($Session.received[$key] - $Session.sent[$key]).TotalMilliseconds
    Assert-Observation ($elapsed -le 3000) "Held error arrives without ordinary reconnect grace" @{ host = $Session.name; id = $key; elapsed_ms = $elapsed }
    Assert-Observation ($reply.ContainsKey("error") -and $reply["error"]["code"] -eq -32005 -and $reply["error"]["message"] -ceq "upstream held for update" -and $reply["error"]["data"]["error_code"] -ceq "maintenance_held") "Immediate typed maintenance reply" $reply
    Assert-OpenPipes $Session
}
function Get-UtcInstant($Value) {
    if ($Value -is [DateTimeOffset]) { return $Value.ToUniversalTime() }
    if ($Value -is [DateTime]) {
        if ($Value.Kind -eq [DateTimeKind]::Unspecified) { throw "Timestamp has no authoritative timezone" }
        return [DateTimeOffset]::new($Value).ToUniversalTime()
    }
    if ($Value -is [string] -and $Value -match '(?:[zZ]|[+-]\d{2}:\d{2})$') {
        return [DateTimeOffset]::Parse($Value, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AdjustToUniversal)
    }
    throw "Timestamp must carry an explicit timezone as a string, DateTime, or DateTimeOffset"
}
function Assert-Held($Result, [string]$ServerID) {
    Assert-Observation ($Result["ok"] -eq $true -and $Result.ContainsKey("maintenance")) "Hold succeeds with a maintenance result" $Result
    $lease = $Result["maintenance"]
    $expiry = Get-UtcInstant $lease["expires_at"]
    $now = [DateTimeOffset]::UtcNow
    $predicate = @{ state_held = ($lease["state"] -ceq "HELD"); trees_retired = ($lease["trees_retired"] -eq $true); server_matches = ($lease["server_id"] -ceq $ServerID); hold_id_present = (-not [string]::IsNullOrWhiteSpace($lease["hold_id"])); expiry_future = ($expiry -gt $now) }
    $observed = @{ lease = $lease; predicate = $predicate; expected_server_id = $ServerID; state_type = $lease["state"].GetType().FullName; trees_retired_type = $lease["trees_retired"].GetType().FullName; server_id_type = $lease["server_id"].GetType().FullName; hold_id_type = $lease["hold_id"].GetType().FullName; expiry_type = $lease["expires_at"].GetType().FullName; expiry_utc = $expiry.ToString("o"); expiry_utc_ticks = $expiry.UtcTicks; now_utc = $now.ToString("o"); now_utc_ticks = $now.UtcTicks }
    Assert-Observation ($predicate.state_held -and $predicate.trees_retired -and $predicate.server_matches -and $predicate.hold_id_present -and $predicate.expiry_future) "Usable HELD requires retired trees and future expiry" $observed
    return $lease
}
function Wait-Fence($Context, $Job) {
    $until = [DateTime]::UtcNow.AddSeconds(10)
    while ([DateTime]::UtcNow -lt $until) {
        $status = Get-Status $Context
        $leases = @($status["maintenance"] | Where-Object { $_["state"] -in @("HOLDING", "HELD") })
        if ($leases.Count -eq 1) { Remember-Lease $Context $leases[0]; Write-Trace "fence-observed" $leases[0]; return $leases[0] }
        if ($Job.process.HasExited) { throw "Hold command exited before its fence could be observed" }
        Start-Sleep -Milliseconds 25
    }
    throw "Durable maintenance fence was not observable"
}
function Assert-TreeRetired($Context, [string]$Fixture, [int]$Count) {
    $generations = @(Get-Generations $Context $Fixture)
    Assert-Observation ($Count -gt 0 -and $generations.Count -eq $Count) "No scoped generation starts during hold" @{ expected = $Count; generations = $generations }
    foreach ($generation in $generations) {
        foreach ($processID in @([int]$generation.leader_pid, [int]$generation.descendant_pid)) {
            $known = @($Identities | Where-Object { $_.pid -eq $processID -and $_.executable -eq $Fixture })
            Assert-Observation ($known.Count -gt 0) "Scoped process identity was observed before retirement" $processID
            Assert-Observation (-not (Test-IdentityAlive $known[-1])) "Scoped process is dead" $known[-1]
        }
    }
}
function Observe-Tree($Payload, [string]$Fixture, $Context) {
    $leader = Get-Identity ([int]$Payload.leader_pid) "fixture-leader" $Fixture
    $child = Get-Identity ([int]$Payload.descendant_pid) "fixture-descendant" $Fixture
    if ($IsWindows) {
        $row = Get-CimInstance Win32_Process -Filter "ProcessId = $($child.pid)"
        $parentID = [int]$row.ParentProcessId
    } else {
        $result = Invoke-Native $Ps @("-p", [string]$child.pid, "-o", "ppid=") $Context
        Assert-Observation ($result.exit_code -eq 0 -and $result.stdout.Trim() -match '^\d+$') "Unix descendant parent is observable" $result
        $parentID = [int]$result.stdout.Trim()
    }
    Assert-Observation ($parentID -eq $leader.pid) "Managed descendant belongs to the selected tree" @{ leader = $leader; descendant = $child; observed_parent = $parentID }
}
function Assert-NoReplay($Context, [string[]]$HeldMarkers, [string[]]$ForwardedMarkers) {
    $frames = @(Get-Capture $Context)
    Assert-Observation ($frames.Count -gt 0) "Upstream capture has a positive denominator" $frames.Count
    foreach ($marker in $HeldMarkers) {
        $hits = @($frames | Where-Object { (Get-FrameMarker $_["frame"]) -ceq $marker })
        Assert-Observation ($hits.Count -eq 0) "Held frame never reached or replayed upstream: $marker" $hits.Count
    }
    foreach ($marker in $ForwardedMarkers) {
        $hits = @($frames | Where-Object { $_["kind"] -eq "received" -and (Get-FrameMarker $_["frame"]) -ceq $marker })
        Assert-Observation ($hits.Count -eq 1) "Forwarded request was not replayed: $marker" $hits.Count
    }
    foreach ($frame in $frames) { Write-Trace "upstream-capture" $frame }
}
function Overwrite-Executable([string]$Destination, [string]$Replacement) {
    $old = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash
    # Truncate/write the executed path itself; a rename does not prove Unix tree death.
    $input = [IO.File]::OpenRead($Replacement)
    try {
        $output = [IO.File]::Open($Destination, [IO.FileMode]::Create, [IO.FileAccess]::Write, [IO.FileShare]::None)
        try { $input.CopyTo($output); $output.Flush($true) } finally { $output.Dispose() }
    } finally { $input.Dispose() }
    $new = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash
    Assert-Observation ($old -ne $new -and $new -eq (Get-FileHash -LiteralPath $Replacement -Algorithm SHA256).Hash) "Actual overwrite changed the executed file bytes" @{ path = $Destination; before = $old; after = $new }
}
function Resume-Lease($Context, $Lease) {
    $result = Invoke-CLI $Context @("resume", $Lease.hold_id, "--json")
    Assert-Observation ($result["ok"] -eq $true -and $result["maintenance"]["state"] -ceq "RELEASED" -and $result["maintenance"]["hold_id"] -ceq $Lease.hold_id) "Exact lease released durably" $result
}
function New-ModernFrame($ID, [string]$Method, [string]$Marker) {
    return @{ jsonrpc = "2.0"; id = $ID; method = $Method; params = @{ name = "modern_echo"; arguments = @{ marker = $Marker }; _meta = @{ "io.modelcontextprotocol/protocolVersion" = "2026-07-28"; "io.modelcontextprotocol/clientCapabilities" = @{}; "io.modelcontextprotocol/clientInfo" = @{ name = "maintenance-smoke"; version = "1" } } } }
}

try {
    if ($TimeoutSeconds -lt 60) { throw "TimeoutSeconds must be at least 60" }
    $SourceRoot = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $SourceRoot).Path)
    $CandidateBinary = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $CandidateBinary).Path)
    $ScratchRoot = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $ScratchRoot).Path)
    if (Test-Beneath $ScratchRoot ([IO.Path]::GetFullPath([IO.Path]::GetTempPath()))) { throw "OS temporary storage is not an authorized primary scratch root" }
    $OutputDir = [IO.Path]::GetFullPath($OutputDir)
    $EvidencePath = [IO.Path]::GetFullPath($EvidencePath)
    $Git = (Get-Command git -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
    $Go = (Get-Command go -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
    if (-not $IsWindows) {
        $Ps = (Get-Command ps -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
        $Chmod = (Get-Command chmod -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
    }
    $common = & $Git -C $SourceRoot rev-parse --path-format=absolute --git-common-dir
    if ($LASTEXITCODE -ne 0) { throw "Cannot resolve primary repository identity" }
    $PrimaryRoot = Split-Path -Parent ([IO.Path]::GetFullPath($common.Trim()))
    $PrimaryAgent = Join-Path $PrimaryRoot ".agent"
    if (-not (Test-Beneath $ScratchRoot $PrimaryAgent) -or (Test-Beneath $ScratchRoot (Join-Path $PrimaryAgent "worktrees"))) { throw "ScratchRoot must be explicitly owned scratch beneath PRIMARY .agent, outside linked worktrees" }
    foreach ($path in @($OutputDir, $EvidencePath)) {
        if (-not (Test-Beneath $path $ScratchRoot)) { throw "Output and evidence must stay beneath supplied PRIMARY ScratchRoot" }
        if (Test-Path -LiteralPath $path) { throw "Output and evidence must be fresh paths: $path" }
        Assert-NoLinks $path $PrimaryAgent
    }
    Assert-NoLinks $ScratchRoot $PrimaryAgent
    if (Test-Beneath $CandidateBinary $SourceRoot) { $Evidence["candidate_source_relation"] = "candidate-root binary" } else { $Evidence["candidate_source_relation"] = "explicit externally built candidate; root must bind binary to source SHA" }
    [void][IO.Directory]::CreateDirectory($OutputDir)
    [void][IO.Directory]::CreateDirectory((Split-Path -Parent $EvidencePath))
    foreach ($directory in @("bin", "go-cache", "go-tmp", "go-modcache")) { [void][IO.Directory]::CreateDirectory((Join-Path $OutputDir $directory)) }
    if (-not $IsWindows) {
        & $Chmod 700 $OutputDir
        if ($LASTEXITCODE -ne 0) { throw "Cannot restrict owned output directory" }
    }
    $Transcript = [IO.StreamWriter]::new((Join-Path $OutputDir "transcript.ndjson"), $false, $Utf8)
    $Deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $Evidence["source_root"] = $SourceRoot
    $Evidence["source_sha"] = (& $Git -C $SourceRoot rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0) { throw "Cannot capture source SHA" }
    $Evidence["candidate_sha256"] = (Get-FileHash -LiteralPath $CandidateBinary -Algorithm SHA256).Hash
    $Evidence["output_dir"] = $OutputDir
    $suffix = if ($IsWindows) { ".exe" } else { "" }
    $Launcher = Join-Path $OutputDir "bin/mcp-mux$suffix"
    $V1 = Join-Path $OutputDir "bin/lifecycle-v1$suffix"
    $V2 = Join-Path $OutputDir "bin/lifecycle-v2$suffix"
    $Modern = Join-Path $OutputDir "bin/modern$suffix"
    Copy-Item -LiteralPath $CandidateBinary -Destination $Launcher
    $ctx = New-Context "legacy"
    $fixtureSource = Join-Path $SourceRoot "scripts/lifecycle-smoke-upstream/main.go"
    foreach ($build in @(@($V1, "1"), @($V2, "2"))) {
        $result = Invoke-Native $Go @("build", "-trimpath", "-ldflags", "-X main.version=$($build[1])", "-o", $build[0], $fixtureSource) $ctx 90
        Assert-Observation ($result.exit_code -eq 0) "Build distinguishable lifecycle fixture" @{ version = $build[1]; result = $result }
    }
    $result = Invoke-Native $Go @("build", "-trimpath", "-o", $Modern, (Join-Path $SourceRoot "testdata/mock_modern_server.go")) $ctx 90
    Assert-Observation ($result.exit_code -eq 0) "Build existing native modern fixture unchanged" $result
    foreach ($path in @($V1, $V2, $Modern)) { $Evidence.fixture_hashes[$path] = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash }
    $Fixture = Join-Path $OutputDir "bin/live-upstream$suffix"
    $OtherFixture = Join-Path $OutputDir "bin/unrelated-upstream$suffix"
    Copy-Item -LiteralPath $V1 -Destination $Fixture
    Copy-Item -LiteralPath $V1 -Destination $OtherFixture
    $a = Start-Host $ctx $Fixture "legacy-a"
    Initialize-Legacy $a
    $b = Start-Host $ctx $Fixture "legacy-b"
    Initialize-Legacy $b
    $p1 = Invoke-Probe $a 10 "initial-numeric" "1"
    $p2 = Invoke-Probe $b "initial-string" "initial-string" "1"
    Assert-Observation ($p1.leader_pid -eq $p2.leader_pid -and $p1.descendant_pid -eq $p2.descendant_pid) "Two legacy hosts share one real process tree" @($p1, $p2)
    Observe-Tree $p1 $Fixture $ctx
    Assert-Observation (@(Get-Generations $ctx $Fixture).Count -eq 1) "One initial shared generation" @(Get-Generations $ctx $Fixture)
    $other = Start-Host $ctx $OtherFixture "unrelated"
    Initialize-Legacy $other
    $otherBefore = Invoke-Probe $other 20 "unrelated-before" "1"
    Observe-Tree $otherBefore $OtherFixture $ctx
    $owner = Get-Owner $ctx $Fixture
    $beforeStatus = Get-Status $ctx
    $daemon = Get-Identity ([int]$beforeStatus.pid) "legacy-daemon"

    [void](Send-Frame $a (New-Probe 100 "drain-finish" 1200))
    [void](Wait-CapturedMarker $ctx "drain-finish")
    $holdJob = Start-Native $Launcher @("hold", $owner.server_id, "--ttl", "30s", "--drain-timeout", "2s", "--json") $ctx
    $fence = Wait-Fence $ctx $holdJob
    [void](Send-Frame $a (New-Probe 200 "held-numeric"))
    [void](Send-Frame $b (New-Probe "held-string-id" "held-string"))
    [void](Send-Frame $a @{ jsonrpc = "2.0"; method = "notifications/maintenance-smoke"; params = @{ marker = "held-notification" } })
    Assert-HeldReply $a 200
    Assert-HeldReply $b "held-string-id"
    $drained = Wait-Reply $a 100
    Assert-Observation ($drained.ContainsKey("result")) "Already-forwarded short work completes during drain" $drained
    $holdResult = Finish-Native $holdJob
    $holdResponse = $holdResult.stdout | ConvertFrom-Json -AsHashtable
    if ($holdResponse.ContainsKey("maintenance")) { Remember-Lease $ctx $holdResponse.maintenance }
    Assert-PrivateReadback ($holdResult.stdout + $holdResult.stderr)
    Assert-Observation ($holdResult.exit_code -eq 0) "Actual hold CLI succeeds" $holdResult.exit_code
    $lease = Assert-Held $holdResponse $owner.server_id
    $completion = @(Get-Capture $ctx | Where-Object { $_["kind"] -eq "completed" -and (Get-FrameMarker $_["frame"]) -ceq "drain-finish" })
    Assert-Observation ($completion.Count -eq 1 -and (Get-UtcInstant $completion[0].utc) -le (Get-UtcInstant $lease.drain_deadline)) "Drain completion precedes the accepted single deadline" $completion
    Assert-Observation ([Math]::Abs(((Get-UtcInstant $lease.drain_deadline) - (Get-UtcInstant $lease.expires_at).AddSeconds(-30)).TotalSeconds - 2) -lt 0.1) "Hold reports the requested single two-second drain bound" $lease
    Assert-TreeRetired $ctx $Fixture 1
    Overwrite-Executable $Fixture $V2
    Assert-TreeRetired $ctx $Fixture 1
    $otherHeld = Invoke-Probe $other 21 "unrelated-during" "1"
    Assert-Observation ($otherHeld.leader_pid -eq $otherBefore.leader_pid) "Unrelated context remains unchanged during hold" $otherHeld
    # Also demand a normally cached method; maintenance must win over cached success.
    [void](Send-Frame $a @{ jsonrpc = "2.0"; id = 201; method = "tools/list"; params = @{} })
    Assert-HeldReply $a 201
    $renewBefore = [DateTimeOffset]::UtcNow
    $renew = Invoke-CLI $ctx @("renew", $lease.hold_id, "--ttl", "30s", "--json")
    $renewAfter = [DateTimeOffset]::UtcNow
    $renewed = Assert-Held $renew $owner.server_id
    $expiry = Get-UtcInstant $renewed.expires_at
    Assert-Observation ($renewed.hold_id -ceq $lease.hold_id -and $expiry -gt (Get-UtcInstant $lease.expires_at) -and $expiry -ge $renewBefore.AddSeconds(30).AddMilliseconds(-100) -and $expiry -le $renewAfter.AddSeconds(30).AddMilliseconds(100)) "Renew expiry is calculated from acceptance" $renewed
    $lease = $renewed
    $conflict = Invoke-CLI $ctx @("hold", $owner.server_id, "--ttl", "30s", "--drain-timeout", "0s", "--json") -Refusal
    Assert-Observation ($conflict["error_code"] -ceq "maintenance_conflict") "Competing acquisition cannot replace the lease" $conflict
    $stale = Invoke-CLI $ctx @("resume", ([guid]::NewGuid().ToString("N")), "--json") -Refusal
    Assert-Observation ($stale["error_code"] -in @("maintenance_conflict", "maintenance_not_found")) "Stale identity cannot release current suppression" $stale
    $Pending = $Launcher + "~"
    Copy-Item -LiteralPath $CandidateBinary -Destination $Pending
    $activationBefore = @(Get-ChildItem -LiteralPath (Join-Path $OutputDir "bin") -Recurse -Force | Sort-Object FullName | ForEach-Object { if ($_.PSIsContainer) { "directory:" + $_.FullName } else { "file:" + $_.FullName + ":" + (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash } })
    Assert-Observation ($activationBefore.Count -gt 0 -and (Test-Path -LiteralPath $Pending)) "Staged private activation has real file evidence" $activationBefore
    foreach ($request in @(
        @{ cmd = "restart_owner"; server_id = $owner.server_id },
        @{ cmd = "graceful-restart"; drain_timeout_ms = 0; successor_exe = $Launcher },
        @{ cmd = "shutdown"; drain_timeout_ms = 0 }
    )) {
        $refused = Invoke-Control $ctx $request
        Assert-Observation ($refused["ok"] -eq $false -and $refused["error_code"] -ceq "maintenance_held") "Terminal controlled lifecycle refusal: $($request.cmd)" $refused
    }
    foreach ($arguments in @(@("stop", "--force"), @("upgrade", "--restart-active", $Launcher, "--force-daemon-restart"), @("upgrade", "--restart", "--force-daemon-restart"))) {
        $refused = Invoke-Native $Launcher $arguments $ctx
        Assert-PrivateReadback ($refused.stdout + $refused.stderr)
        Assert-Observation ($refused.exit_code -eq 1 -and ($refused.stdout + $refused.stderr).Contains("maintenance_held: upstream held for update") -and -not ($refused.stdout + $refused.stderr).Contains("falling back to shutdown")) "Classified held CLI refusal without fallback" $refused
        $after = Get-Status $ctx
        Assert-Observation ($after.pid -eq $beforeStatus.pid -and $after.daemon_generation -ceq $beforeStatus.daemon_generation -and (Test-IdentityAlive $daemon)) "Refusal retains the original daemon" @{ pid = $after.pid; generation = $after.daemon_generation }
        $active = @($after["maintenance"] | Where-Object { $_.hold_id -ceq $lease.hold_id })
        Assert-Observation ($active.Count -eq 1 -and $active[0].state -ceq "HELD" -and (Get-UtcInstant $active[0].expires_at) -eq (Get-UtcInstant $lease.expires_at)) "Refusal leaves the current lease unchanged" $active
        Assert-TreeRetired $ctx $Fixture 1
        $activationAfter = @(Get-ChildItem -LiteralPath (Join-Path $OutputDir "bin") -Recurse -Force | Sort-Object FullName | ForEach-Object { if ($_.PSIsContainer) { "directory:" + $_.FullName } else { "file:" + $_.FullName + ":" + (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash } })
        Assert-Observation ($activationAfter.Count -gt 0 -and @(Compare-Object $activationBefore $activationAfter).Count -eq 0) "Refusal leaves launcher, pending binary, layout, and active pointer untouched" $activationAfter
    }
    Assert-NoReplay $ctx @("held-numeric", "held-string", "held-notification") @("initial-numeric", "initial-string", "drain-finish")
    Resume-Lease $ctx $lease
    $newA = Invoke-Probe $a 300 "resumed-a" "2"
    $newB = Invoke-Probe $b "resumed-b-id" "resumed-b" "2"
    Assert-Observation ($newA.leader_pid -eq $newB.leader_pid -and $newA.leader_pid -ne $p1.leader_pid -and @(Get-Generations $ctx $Fixture).Count -eq 2) "Resume serves both unchanged host pipes from one new generation" @($newA, $newB)
    Observe-Tree $newA $Fixture $ctx
    Assert-NoReplay $ctx @("held-numeric", "held-string", "held-notification") @("drain-finish", "resumed-a", "resumed-b")

    [void](Send-Frame $a (New-Probe 400 "drain-terminated" 10000))
    [void](Wait-CapturedMarker $ctx "drain-terminated")
    $ttlServerID = (Get-Owner $ctx $Fixture).server_id
    $holdJob = Start-Native $Launcher @("hold", $ttlServerID, "--ttl", "5s", "--drain-timeout", "2s", "--json") $ctx
    [void](Wait-Fence $ctx $holdJob)
    [void](Send-Frame $a (New-Probe 401 "ttl-held-numeric"))
    [void](Send-Frame $b (New-Probe "ttl-held-string-id" "ttl-held-string"))
    Assert-HeldReply $a 401
    Assert-HeldReply $b "ttl-held-string-id"
    $terminated = Wait-Reply $a 400
    Assert-Observation ($terminated.ContainsKey("error") -and -not $terminated.ContainsKey("result")) "Unfinished work ends once with a terminal error" $terminated
    $heldResult = Finish-Native $holdJob
    $heldResponse = $heldResult.stdout | ConvertFrom-Json -AsHashtable
    if ($heldResponse.ContainsKey("maintenance")) { Remember-Lease $ctx $heldResponse.maintenance }
    Assert-Observation ($heldResult.exit_code -eq 0) "Short TTL hold succeeds before expiry" $heldResult.exit_code
    $ttlLease = Assert-Held $heldResponse $ttlServerID
    Assert-TreeRetired $ctx $Fixture 2
    $until = Get-UtcInstant $ttlLease.expires_at
    while ([DateTimeOffset]::UtcNow -lt $until) {
        Assert-TreeRetired $ctx $Fixture 2
        Start-Sleep -Milliseconds 100
    }
    $ttlA = Invoke-Probe $a 500 "ttl-recovered-a" "2"
    $ttlB = Invoke-Probe $b "ttl-recovered-b-id" "ttl-recovered-b" "2"
    Assert-Observation ($ttlA.leader_pid -eq $ttlB.leader_pid -and $ttlA.leader_pid -ne $newA.leader_pid -and @(Get-Generations $ctx $Fixture).Count -eq 3) "Safe TTL recovery creates one replacement on unchanged pipes" @($ttlA, $ttlB)
    Observe-Tree $ttlA $Fixture $ctx
    Assert-NoReplay $ctx @("held-numeric", "held-string", "held-notification", "ttl-held-numeric", "ttl-held-string") @("drain-finish", "drain-terminated", "ttl-recovered-a", "ttl-recovered-b")
    $unfinishedCompletions = @(Get-Capture $ctx | Where-Object { $_["kind"] -eq "completed" -and (Get-FrameMarker $_["frame"]) -ceq "drain-terminated" })
    Assert-Observation ($unfinishedCompletions.Count -eq 0) "Retired unfinished operation was never completed or replayed" $unfinishedCompletions.Count

    # Unplanned loss uses a separate owned daemon with no live unrelated trees.
    $recovery = New-Context "recovery"
    $RecoveryFixture = Join-Path $OutputDir "bin/recovery-upstream$suffix"
    Copy-Item -LiteralPath $V1 -Destination $RecoveryFixture
    $r = Start-Host $recovery $RecoveryFixture "recovery-host"
    Initialize-Legacy $r
    $rp = Invoke-Probe $r 600 "recovery-initial" "1"
    Observe-Tree $rp $RecoveryFixture $recovery
    $ro = Get-Owner $recovery $RecoveryFixture
    $rl = Assert-Held (Invoke-CLI $recovery @("hold", $ro.server_id, "--ttl", "30s", "--drain-timeout", "0s", "--json")) $ro.server_id
    Assert-TreeRetired $recovery $RecoveryFixture 1
    Overwrite-Executable $RecoveryFixture $V2
    $oldStatus = Get-Status $recovery
    $oldDaemon = Get-Identity ([int]$oldStatus.pid) "recovery-predecessor"
    $victim = [Diagnostics.Process]::GetProcessById([int]$oldDaemon.pid)
    try {
        if (-not (Test-IdentityAlive $oldDaemon)) { throw "Lost exact daemon identity before controlled crash" }
        $victim.Kill(); [void]$victim.WaitForExit(5000)
    } finally { $victim.Dispose() }
    Write-Trace "owned-unplanned-daemon-loss" $oldDaemon
    [void](Send-Frame $r (New-Probe 601 "recovery-buffered-numeric"))
    [void](Send-Frame $r (New-Probe "recovery-buffered-string-id" "recovery-buffered-string"))
    $restart = Start-Native $Launcher @("daemon") $recovery
    $recovery["daemon_job"] = $restart
    Assert-HeldReply $r 601
    Assert-HeldReply $r "recovery-buffered-string-id"
    # This process may lose election to the still-open shim's aware recovery.
    $until = [DateTime]::UtcNow.AddSeconds(15)
    $reloaded = $null
    while ([DateTime]::UtcNow -lt $until) {
        try {
            $candidateStatus = Get-Status $recovery
            if ($candidateStatus.pid -ne $oldStatus.pid -and $candidateStatus.daemon_generation -cne $oldStatus.daemon_generation) { $reloaded = $candidateStatus; break }
        } catch { Write-Trace "recovery-status-retry" @{ error = Protect-Text $_.Exception.Message } }
        Start-Sleep -Milliseconds 100
    }
    Assert-Observation ($null -ne $reloaded) "Aware replacement daemon comes up after unplanned loss" $reloaded
    [void](Get-Identity ([int]$reloaded.pid) "recovery-successor")
    $saved = @($reloaded["maintenance"] | Where-Object { $_.hold_id -ceq $rl.hold_id })
    Assert-Observation ($saved.Count -eq 1 -and $saved[0].state -ceq "HELD" -and $saved[0].trees_retired -eq $true -and (Get-UtcInstant $saved[0].expires_at) -eq (Get-UtcInstant $rl.expires_at)) "Durable HELD was restored before fresh admission" $saved
    Assert-TreeRetired $recovery $RecoveryFixture 1
    Resume-Lease $recovery $rl
    $recovered = Invoke-Probe $r 602 "aware-recovered" "2"
    Observe-Tree $recovered $RecoveryFixture $recovery
    Assert-NoReplay $recovery @("recovery-buffered-numeric", "recovery-buffered-string") @("recovery-initial", "aware-recovered")

    $modernCtx = New-Context "modern"
    $m = Start-Host $modernCtx $Modern "modern-host" -Modern
    $opening = Send-Frame $m (New-ModernFrame "modern-opening" "server/discover" "modern-initial")
    $modernReply = Wait-Reply $m "modern-opening"
    Assert-Observation ($modernReply.ContainsKey("result") -and "2026-07-28" -in $modernReply["result"]["supportedVersions"]) "Known same-era modern discovery succeeds" $modernReply
    $mo = Get-Owner $modernCtx $Modern
    $modernStatus = Get-Status $modernCtx
    [void](Get-Identity ([int]$modernStatus.pid) "modern-daemon")
    foreach ($fact in (@{ protocol_era = "2026-07-28"; sharing_policy = "forced-isolated"; cache_policy = "off"; lifecycle_policy = "r1-quarantine" }).GetEnumerator()) {
        Assert-Observation ($mo[$fact.Key] -ceq $fact.Value) "Modern admission policy: $($fact.Key)" $mo[$fact.Key]
    }
    $modernPID = [int]$mo["upstream_pid"]
    Assert-Observation ($modernPID -gt 0) "Modern real upstream PID is observable" $modernPID
    $modernIdentity = Get-Identity $modernPID "modern-upstream" $Modern
    $ml = Assert-Held (Invoke-CLI $modernCtx @("hold", $mo.server_id, "--ttl", "30s", "--drain-timeout", "0s", "--json")) $mo.server_id
    Assert-Observation (-not (Test-IdentityAlive $modernIdentity)) "Modern managed upstream is dead while held" $modernIdentity
    $preholdCapture = @(Read-CaptureLines $modernCtx.modern_capture)
    Assert-Observation ($preholdCapture.Count -eq 1 -and $preholdCapture[0] -ceq $opening) "Modern opening was forwarded unchanged without legacy bootstrap" $preholdCapture
    [IO.File]::WriteAllLines((Join-Path $modernCtx.records "modern-prehold.ndjson"), $preholdCapture, $Utf8)
    [void](Send-Frame $m (New-ModernFrame 701 "tools/call" "modern-held-numeric"))
    [void](Send-Frame $m (New-ModernFrame "modern-held-string-id" "tools/call" "modern-held-string"))
    Assert-HeldReply $m 701
    Assert-HeldReply $m "modern-held-string-id"
    $heldCapture = @(Read-CaptureLines $modernCtx.modern_capture)
    Assert-Observation ($heldCapture.Count -eq 1 -and $heldCapture[0] -ceq $opening) "Modern held requests do not reach upstream" $heldCapture
    Resume-Lease $modernCtx $ml
    $fresh = Send-Frame $m (New-ModernFrame "modern-fresh" "tools/call" "modern-fresh")
    $continuation = Wait-Reply $m "modern-fresh"
    Assert-Observation ($continuation.ContainsKey("result") -and -not $continuation.ContainsKey("error") -and $continuation["result"]["content"][0]["text"] -ceq "modern fixture tool result") "Fresh modern request succeeds after resume" $continuation
    $admitted = Get-Owner $modernCtx $Modern
    Assert-Observation ($admitted.protocol_era -ceq "2026-07-28" -and $admitted.sharing_policy -ceq "forced-isolated" -and $admitted.cache_policy -ceq "off" -and [int]$admitted.upstream_pid -ne $modernPID) "Modern continuation uses fresh exact-era admission" $admitted
    [void](Get-Identity ([int]$admitted.upstream_pid) "modern-replacement" $Modern)
    $postCapture = @(Read-CaptureLines $modernCtx.modern_capture)
    Assert-Observation ($postCapture.Count -eq 1 -and $postCapture[0] -ceq $fresh) "Modern replacement has no bootstrap or held replay" $postCapture
    Assert-OpenPipes $m
    $Evidence["modern_continuation"] = "fresh same-era admission on original pipe"
    Assert-NoReplay $ctx @("held-numeric", "held-string", "held-notification", "ttl-held-numeric", "ttl-held-string") @("drain-terminated", "resumed-a", "resumed-b", "ttl-recovered-a", "ttl-recovered-b")
    Assert-NoReplay $recovery @("recovery-buffered-numeric", "recovery-buffered-string") @("aware-recovered")
    foreach ($session in $Sessions) {
        for ($i = 0; $i -lt 20; $i++) { Read-Available $session 0 }
        $responses = @($session.frames | Where-Object { $_.ContainsKey("id") })
        Assert-Observation ($responses.Count -gt 0) "Host terminal replies have a positive denominator" @{ host = $session.name; count = $responses.Count }
        foreach ($group in @($responses | Group-Object { Get-IDKey $_["id"] })) {
            Assert-Observation ($group.Count -eq 1) "Host has no duplicate terminal IDs" @{ host = $session.name; id = $group.Name; count = $group.Count }
        }
        foreach ($response in $responses) {
            $key = Get-IDKey $response["id"]
            Assert-Observation ($response["jsonrpc"] -ceq "2.0" -and $null -ne $response["id"] -and $session.sent.ContainsKey($key) -and -not $response.ContainsKey("method") -and ($response.ContainsKey("result") -xor $response.ContainsKey("error"))) "Every terminal response uses an original sent request ID; notifications invent none" @{ host = $session.name; id = $key }
        }
        Assert-OpenPipes $session
    }
    Assert-Observation ($Checks.Count -gt 0 -and @($Checks | Where-Object { -not $_.passed }).Count -eq 0) "All recorded live observations passed with a positive denominator" $Checks.Count
    $Evidence["verdict"] = "PASS"
    $ExitCode = 0
} catch {
    $Evidence["verdict"] = "FAIL"
    $Evidence["error"] = Protect-Text $_.Exception.Message
    Write-Trace "failure" @{ error = $Evidence.error; position = Protect-Text $_.InvocationInfo.PositionMessage }
} finally {
    $cleanup = [System.Collections.Generic.List[object]]::new()
    foreach ($context in $Contexts) {
        foreach ($lease in $context.holds) {
            try {
                $status = Get-Status $context
                $current = @($status["maintenance"] | Where-Object { $_.hold_id -ceq $lease.hold_id -and $_.server_id -ceq $lease.server_id -and $_.state -ne "RELEASED" })
                if ($current.Count -gt 0) {
                    if ($current[0].state -ne "HELD" -or $current[0].trees_retired -ne $true) { throw "Retirement is blocked; retain authority and owned resources for root recovery" }
                    Resume-Lease $context $current[0]
                }
            } catch { $cleanup.Add(@{ namespace = $context.name; control_path = $context.control_path; hold_id = $lease.hold_id; server_id = $lease.server_id; error = Protect-Text $_.Exception.Message }); $ExitCode = 1 }
        }
    }
    foreach ($session in $Sessions) {
        try { $session.input.Close() } catch { $cleanup.Add(@{ host = $session.name; error = Protect-Text $_.Exception.Message }); $ExitCode = 1 }
    }
    $shutdownAccepted = $Contexts.Count -gt 0
    foreach ($context in $Contexts) {
        try {
            $response = Invoke-Control $context @{ cmd = "shutdown"; drain_timeout_ms = 0 }
            if ($response["ok"] -ne $true) { throw "Owned daemon cleanup refused; retain resources, no PID cleanup" }
            $cleanup.Add(@{ namespace = $context.name; shutdown = $response })
        } catch { $shutdownAccepted = $false; $cleanup.Add(@{ namespace = $context.name; error = Protect-Text $_.Exception.Message }); $ExitCode = 1 }
    }
    $closeDeadline = [DateTime]::UtcNow.AddSeconds(10)
    foreach ($session in $Sessions) {
        try {
            $remaining = [int][Math]::Max(1, ($closeDeadline - [DateTime]::UtcNow).TotalMilliseconds)
            if (-not $session.process.WaitForExit($remaining)) { throw "Owned host did not exit after stdin closure; no PID cleanup attempted" }
            $closed = @{ host = $session.name; exit_code = $session.process.ExitCode; stderr = Protect-Text $session.stderr.Result }
            if ($session.ContainsKey("identity_failure")) {
                [void]$session.failure_stdout.Wait(1000)
                $closed["identity_failure"] = $session.identity_failure
                $closed["stdout_complete"] = $session.failure_stdout.IsCompleted
                $closed["stdout"] = if ($session.failure_stdout.IsCompletedSuccessfully) { Protect-Text $session.failure_stdout.Result } else { $null }
                $Evidence["host_identity_failure_cleanup"] = $closed
            }
            Write-Trace "host-closed" $closed
        } catch { $cleanup.Add(@{ host = $session.name; error = Protect-Text $_.Exception.Message }); $ExitCode = 1 }
    }
    foreach ($context in $Contexts) {
        if ($context.ContainsKey("daemon_job")) {
            try { [void](Finish-Native $context.daemon_job 10 $false $closeDeadline) } catch { $cleanup.Add(@{ namespace = $context.name; error = Protect-Text $_.Exception.Message }); $ExitCode = 1 }
        }
    }
    foreach ($identity in $Identities) {
        try {
            while (Test-IdentityAlive $identity) {
                if (-not $shutdownAccepted -or [DateTime]::UtcNow -ge $closeDeadline) { throw "Captured owned process survived lifecycle cleanup: $($identity.label)" }
                $remaining = [int][Math]::Max(1, ($closeDeadline - [DateTime]::UtcNow).TotalMilliseconds)
                Start-Sleep -Milliseconds ([Math]::Min(25, $remaining))
            }
            Write-Trace "owned-process-closed" @{ identity = $identity; captured_identity_alive = $false; observed_utc = [DateTime]::UtcNow.ToString("o"); close_deadline_utc = $closeDeadline.ToString("o") }
        } catch { $cleanup.Add(@{ identity = $identity; error = Protect-Text $_.Exception.Message }); $ExitCode = 1 }
    }
    if ($ExitCode -ne 0) { $Evidence["verdict"] = "FAIL" }
    $Evidence["cleanup"] = $cleanup.ToArray()
    $Evidence["checks"] = $Checks.ToArray()
    $Evidence["commands"] = $Commands.ToArray()
    $Evidence["process_identities"] = $Identities.ToArray()
    $Evidence["host_pipes"] = @($Sessions | ForEach-Object { @{ host = $_.name; original_pid = $_.process.Id; original_start_ticks = if ($null -ne $_.identity) { $_.identity.start_ticks } else { $null }; stdin_handle = $_.input_handle; stdout_handle = $_.output_handle } })
    $Evidence["finished_utc"] = [DateTime]::UtcNow.ToString("o")
    if ($null -ne $Transcript) {
        Write-Trace "summary" $Evidence
        $Transcript.Dispose()
        $json = Protect-Text ($Evidence | ConvertTo-Json -Depth 40)
        [IO.File]::WriteAllText((Join-Path $OutputDir "summary.json"), $json, $Utf8)
        [IO.File]::WriteAllText($EvidencePath, $json, $Utf8)
    }
    Write-Output (Protect-Text ($Evidence | ConvertTo-Json -Depth 40))
}
exit $ExitCode
