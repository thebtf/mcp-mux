#Requires -Version 5.1
<#
.SYNOPSIS
    Validates the Windows R1 native-isolation customer-proof runner contract.

.DESCRIPTION
    The contract creates a fresh output directory, first proves that its own
    evidence validator rejects an intentionally incomplete fixture, then runs
    verify-r1-native-isolation.ps1 against the requested source tree. The
    runner is accepted only when it produces the shared R1 evidence schema.

    Exit codes:
      0 = PASS: the runner completed and all required evidence validates.
      1 = FAIL: the runner or its evidence does not satisfy the contract.
      2 = SETUP ERROR: source, runner, or fresh output setup is unavailable.

.PARAMETER SourceRoot
    Candidate source root. Defaults to the repository root containing this
    script.

.PARAMETER OutputDir
    Fresh directory for runner evidence. When omitted, the script creates a
    unique directory below the system temporary directory. A supplied path may
    already exist only when it is an empty directory.

.PARAMETER KeepBaseDir
    Forwarded to the runner to retain its fresh BaseDir after a successful run.

.EXAMPLE
    powershell -NoProfile -File scripts/verify-r1-native-isolation.contract.ps1

.EXAMPLE
    powershell -NoProfile -File scripts/verify-r1-native-isolation.contract.ps1 `
      -SourceRoot C:\src\mcp-mux -OutputDir C:\temp\mcp-r1-evidence -KeepBaseDir
#>

[CmdletBinding()]
param(
    [string] $SourceRoot = (Split-Path -Parent $PSScriptRoot),
    [string] $OutputDir = "",
    [switch] $KeepBaseDir
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$script:ContractTempRoot = $null
$script:ProbeDirectory = $null
$script:UseTemporaryOutput = $false

function Write-Step {
    param([string] $Message)
    Write-Host "[verify-r1-native-isolation.contract] $Message"
}

function Stop-Contract {
    param(
        [int] $ExitCode,
        [string] $Message
    )

    $exception = New-Object System.InvalidOperationException -ArgumentList $Message
    $exception.Data["contract_exit_code"] = $ExitCode
    throw $exception
}

function Stop-Setup {
    param([string] $Message)
    Stop-Contract -ExitCode 2 -Message $Message
}

function Stop-Verification {
    param([string] $Message)
    Stop-Contract -ExitCode 1 -Message $Message
}

function Get-RequiredJsonProperty {
    param(
        [object] $Object,
        [string] $Name
    )

    if ($null -eq $Object) {
        return $null
    }

    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }

    return $property.Value
}

function Test-JsonObject {
    param([object] $Value)
    return ($Value -is [System.Management.Automation.PSCustomObject])
}

function Test-NonEmptyString {
    param([object] $Value)
    return ($Value -is [string] -and -not [string]::IsNullOrWhiteSpace($Value))
}

function Test-Sha256 {
    param([object] $Value)
    return ((Test-NonEmptyString $Value) -and $Value -match '^[0-9A-Fa-f]{64}$')
}

function Test-ExpectedInteger {
    param(
        [object] $Value,
        [int] $Expected
    )

    if ($null -eq $Value -or $Value -is [string] -or $Value -is [bool]) {
        return $false
    }

    try {
        return ([Convert]::ToInt64($Value) -eq $Expected)
    }
    catch {
        return $false
    }
}

function Test-NonEmptyFile {
    param([string] $Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $false
    }

    try {
        $item = Get-Item -LiteralPath $Path -ErrorAction Stop
        return (-not $item.PSIsContainer -and $item.Length -gt 0)
    }
    catch {
        return $false
    }
}

function Resolve-EvidencePath {
    param(
        [string] $EvidenceRoot,
        [string] $RawPath
    )

    try {
        if ([System.IO.Path]::IsPathRooted($RawPath)) {
            return [System.IO.Path]::GetFullPath($RawPath)
        }

        return [System.IO.Path]::GetFullPath((Join-Path -Path $EvidenceRoot -ChildPath $RawPath))
    }
    catch {
        return $null
    }
}

function Test-R1Evidence {
    param([string] $EvidenceRoot)

    $errors = New-Object System.Collections.ArrayList
    $summaryPath = Join-Path -Path $EvidenceRoot -ChildPath "summary.json"
    $transcriptPath = Join-Path -Path $EvidenceRoot -ChildPath "transcript.ndjson"

    if (-not (Test-NonEmptyFile $summaryPath)) {
        [void] $errors.Add("missing or empty summary.json")
        return [pscustomobject]@{
            Valid = $false
            Errors = @($errors.ToArray())
        }
    }

    try {
        $summary = Get-Content -LiteralPath $summaryPath -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
    }
    catch {
        [void] $errors.Add("summary.json is not valid JSON")
        return [pscustomobject]@{
            Valid = $false
            Errors = @($errors.ToArray())
        }
    }

    if (-not (Test-JsonObject $summary)) {
        [void] $errors.Add("summary.json must contain an object")
        return [pscustomobject]@{
            Valid = $false
            Errors = @($errors.ToArray())
        }
    }

    $schemaVersion = Get-RequiredJsonProperty -Object $summary -Name "schema_version"
    if (-not (Test-ExpectedInteger -Value $schemaVersion -Expected 1)) {
        [void] $errors.Add("summary.schema_version must equal integer 1")
    }

    $result = Get-RequiredJsonProperty -Object $summary -Name "result"
    if ($result -cne "PASS") {
        [void] $errors.Add("summary.result must equal PASS")
    }

    $platformID = Get-RequiredJsonProperty -Object $summary -Name "platform_id"
    if (-not (Test-NonEmptyString $platformID)) {
        [void] $errors.Add("summary.platform_id must be nonempty")
    }

    $sourceSHA = Get-RequiredJsonProperty -Object $summary -Name "source_sha"
    if (-not (Test-NonEmptyString $sourceSHA)) {
        [void] $errors.Add("summary.source_sha must be nonempty")
    }

    foreach ($hashField in @("binary_sha256", "fixture_sha256", "corpus_sha256")) {
        $hash = Get-RequiredJsonProperty -Object $summary -Name $hashField
        if (-not (Test-Sha256 $hash)) {
            [void] $errors.Add("summary.$hashField must be a SHA-256 hex string")
        }
    }

    foreach ($countField in @("corpus_total", "corpus_passed")) {
        $count = Get-RequiredJsonProperty -Object $summary -Name $countField
        if (-not (Test-ExpectedInteger -Value $count -Expected 100)) {
            [void] $errors.Add("summary.$countField must equal integer 100")
        }
    }

    $policy = Get-RequiredJsonProperty -Object $summary -Name "policy"
    if ($policy -cne "--mcp-protocol=2026-07-28") {
        [void] $errors.Add("summary.policy must equal --mcp-protocol=2026-07-28")
    }

    $scenarioResults = Get-RequiredJsonProperty -Object $summary -Name "scenario_results"
    if (-not (Test-JsonObject $scenarioResults)) {
        [void] $errors.Add("summary.scenario_results must be an object")
    }
    else {
        foreach ($scenarioID in 1..8) {
            $scenarioResult = Get-RequiredJsonProperty -Object $scenarioResults -Name ([string] $scenarioID)
            if ($null -eq $scenarioResult) {
                [void] $errors.Add("summary.scenario_results is missing ID $scenarioID")
            }
            elseif ($scenarioResult -cne "PASS") {
                [void] $errors.Add("summary.scenario_results.$scenarioID must equal PASS")
            }
        }
    }

    if (-not (Test-NonEmptyFile $transcriptPath)) {
        [void] $errors.Add("missing or empty transcript.ndjson")
    }

    $artifacts = Get-RequiredJsonProperty -Object $summary -Name "artifacts"
    if (-not (Test-JsonObject $artifacts)) {
        [void] $errors.Add("summary.artifacts must be an object")
    }
    else {
        foreach ($artifactName in @("modern", "admission", "lifecycle", "readback", "legacy", "rollback")) {
            $artifactValue = Get-RequiredJsonProperty -Object $artifacts -Name $artifactName
            if (-not (Test-NonEmptyString $artifactValue)) {
                [void] $errors.Add("missing artifacts.$artifactName")
                continue
            }

            $artifactPath = Resolve-EvidencePath -EvidenceRoot $EvidenceRoot -RawPath $artifactValue
            if ($null -eq $artifactPath -or -not (Test-NonEmptyFile $artifactPath)) {
                [void] $errors.Add("artifacts.$artifactName must reference an existing nonempty file")
            }
        }
    }

    return [pscustomobject]@{
        Valid = ($errors.Count -eq 0)
        Errors = @($errors.ToArray())
    }
}

function Assert-ValidatorRejectsIncompleteFixture {
    param([string] $ProbeRoot)

    New-Item -ItemType Directory -Path $ProbeRoot -ErrorAction Stop | Out-Null
    Set-Content -LiteralPath (Join-Path -Path $ProbeRoot -ChildPath "transcript.ndjson") -Value '{"event":"probe"}' -Encoding UTF8

    $scenarioResults = [ordered]@{}
    foreach ($scenarioID in 1..8) {
        $scenarioResults[[string] $scenarioID] = "PASS"
    }

    $artifacts = [ordered]@{}
    foreach ($artifactName in @("modern", "admission", "lifecycle", "readback", "legacy")) {
        $artifactFile = "$artifactName.json"
        Set-Content -LiteralPath (Join-Path -Path $ProbeRoot -ChildPath $artifactFile) -Value '{"probe":true}' -Encoding UTF8
        $artifacts[$artifactName] = $artifactFile
    }

    $sha256 = (('a' * 64) -join '')
    $fixture = [ordered]@{
        schema_version = 1
        result = "PASS"
        platform_id = "validator-probe"
        source_sha = "validator-probe-source"
        binary_sha256 = $sha256
        fixture_sha256 = $sha256
        corpus_sha256 = $sha256
        corpus_total = 100
        corpus_passed = 100
        policy = "--mcp-protocol=2026-07-28"
        scenario_results = $scenarioResults
        artifacts = $artifacts
    }
    $fixture | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path -Path $ProbeRoot -ChildPath "summary.json") -Encoding UTF8

    $probeResult = Test-R1Evidence -EvidenceRoot $ProbeRoot
    if ($probeResult.Valid) {
        Stop-Setup "validator accepted intentionally incomplete evidence"
    }
    if (-not ($probeResult.Errors -contains "missing artifacts.rollback")) {
        Stop-Setup "validator did not reject the intentionally missing rollback artifact"
    }
}

function Assert-FreshOutputDirectory {
    param([string] $Path)

    if (Test-Path -LiteralPath $Path) {
        if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
            Stop-Setup "OutputDir exists but is not a directory: $Path"
        }
        if (@(Get-ChildItem -LiteralPath $Path -Force -ErrorAction Stop).Count -ne 0) {
            Stop-Setup "OutputDir must be empty: $Path"
        }
        return
    }

    $parent = Split-Path -Parent $Path
    if ([string]::IsNullOrWhiteSpace($parent) -or -not (Test-Path -LiteralPath $parent -PathType Container)) {
        Stop-Setup "OutputDir parent directory does not exist: $parent"
    }

    New-Item -ItemType Directory -Path $Path -ErrorAction Stop | Out-Null
    if (@(Get-ChildItem -LiteralPath $Path -Force -ErrorAction Stop).Count -ne 0) {
        Stop-Setup "OutputDir is not empty after creation: $Path"
    }
}

$exitCode = 0
$exitMessage = ""

try {
    if ([string]::IsNullOrWhiteSpace($SourceRoot)) {
        Stop-Setup "SourceRoot must be nonempty"
    }
    $SourceRoot = [System.IO.Path]::GetFullPath($SourceRoot)
    if (-not (Test-Path -LiteralPath $SourceRoot -PathType Container)) {
        Stop-Setup "SourceRoot does not exist: $SourceRoot"
    }

    foreach ($sourceFile in @(
        "cmd\mcp-mux\main.go",
        "testdata\mock_modern_server.go",
        "testdata\modern_opening_corpus.ndjson"
    )) {
        $sourcePath = Join-Path -Path $SourceRoot -ChildPath $sourceFile
        if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf)) {
            Stop-Setup "SourceRoot is missing required runner input: $sourceFile"
        }
    }

    $script:ContractTempRoot = Join-Path -Path ([System.IO.Path]::GetTempPath()) -ChildPath ("mcp-mux-r1-contract-{0}-{1}" -f $PID, [guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $script:ContractTempRoot -ErrorAction Stop | Out-Null
    $script:ProbeDirectory = Join-Path -Path $script:ContractTempRoot -ChildPath "validator-probe"

    if ([string]::IsNullOrWhiteSpace($OutputDir)) {
        $OutputDir = Join-Path -Path $script:ContractTempRoot -ChildPath "output"
        $script:UseTemporaryOutput = $true
    }
    else {
        $OutputDir = [System.IO.Path]::GetFullPath($OutputDir)
    }

    Assert-FreshOutputDirectory -Path $OutputDir

    Write-Step "proving validator rejects intentionally incomplete evidence"
    Assert-ValidatorRejectsIncompleteFixture -ProbeRoot $script:ProbeDirectory

    $runnerPath = Join-Path -Path $SourceRoot -ChildPath "scripts\verify-r1-native-isolation.ps1"
    if (-not (Test-Path -LiteralPath $runnerPath -PathType Leaf)) {
        Stop-Setup "runner does not exist: $runnerPath"
    }

    $powershellPath = Join-Path -Path $env:SystemRoot -ChildPath "System32\WindowsPowerShell\v1.0\powershell.exe"
    if (-not (Test-Path -LiteralPath $powershellPath -PathType Leaf)) {
        Stop-Setup "Windows PowerShell host does not exist: $powershellPath"
    }

    $runnerArguments = @(
        "-NoLogo",
        "-NoProfile",
        "-NonInteractive",
        "-ExecutionPolicy", "Bypass",
        "-File", $runnerPath,
        "-SourceRoot", $SourceRoot,
        "-OutputDir", $OutputDir
    )
    if ($KeepBaseDir) {
        $runnerArguments += "-KeepBaseDir"
    }

    Write-Step "running candidate runner with fresh output directory"
    Push-Location -LiteralPath $SourceRoot
    try {
        & $powershellPath @runnerArguments
        $runnerExitCode = $LASTEXITCODE
    }
    finally {
        Pop-Location
    }
    if ($runnerExitCode -eq 2) {
        Stop-Setup "runner reported setup error"
    }
    if ($runnerExitCode -ne 0) {
        Stop-Verification "runner exited with code $runnerExitCode"
    }

    Write-Step "validating runner evidence"
    $validation = Test-R1Evidence -EvidenceRoot $OutputDir
    if (-not $validation.Valid) {
        Stop-Verification ("runner evidence is invalid: " + ($validation.Errors -join "; "))
    }
}
catch {
    $exitCode = 2
    if ($_.Exception.Data.Contains("contract_exit_code")) {
        $exitCode = [int] $_.Exception.Data["contract_exit_code"]
    }
    $exitMessage = $_.Exception.Message
}
finally {
    if ($null -ne $script:ProbeDirectory -and (Test-Path -LiteralPath $script:ProbeDirectory)) {
        Remove-Item -LiteralPath $script:ProbeDirectory -Recurse -Force -ErrorAction SilentlyContinue
    }
    if (-not $script:UseTemporaryOutput -and $null -ne $script:ContractTempRoot -and (Test-Path -LiteralPath $script:ContractTempRoot)) {
        Remove-Item -LiteralPath $script:ContractTempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

if ($exitCode -eq 0) {
    Write-Step "PASS: runner evidence satisfies the R1 native-isolation contract"
}
else {
    $label = if ($exitCode -eq 1) { "FAIL" } else { "SETUP ERROR" }
    Write-Host "[verify-r1-native-isolation.contract] ${label}: $exitMessage"
}

exit $exitCode
