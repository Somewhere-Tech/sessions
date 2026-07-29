[CmdletBinding()]
param(
    [string]$RuntimeDirectory,

    [string]$AppPath,

    [string]$InstallerPath,

    [string]$OutputPath = "windows-host-evidence.json",

    [string]$SourceCommit = "unknown",

    [string]$ExpectedSignerThumbprint,

    [switch]$RequireSigned,

    [switch]$LiveHost,

    [switch]$RequireRunner,

    [string]$CompareBaseline,

    [switch]$AllowAdministrator
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Fail {
    param([string]$Message)
    throw "collect-windows-host-evidence: $Message"
}

function Normalize-Thumbprint {
    param([string]$Thumbprint)
    return ($Thumbprint -replace '[^0-9A-Fa-f]', '').ToUpperInvariant()
}

function Normalize-Path {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    return [IO.Path]::GetFullPath($Path).TrimEnd([IO.Path]::DirectorySeparatorChar)
}

function Test-PathWithin {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Candidate,

        [Parameter(Mandatory = $true)]
        [string]$Root
    )

    $NormalizedCandidate = Normalize-Path $Candidate
    $NormalizedRoot = Normalize-Path $Root
    if ($NormalizedCandidate.Equals($NormalizedRoot, [StringComparison]::OrdinalIgnoreCase)) {
        return $true
    }
    return $NormalizedCandidate.StartsWith(
        "$NormalizedRoot$([IO.Path]::DirectorySeparatorChar)",
        [StringComparison]::OrdinalIgnoreCase
    )
}

function Assert-File {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$Description
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        Fail "$Description is missing at $Path"
    }
}

function Get-Sha256Hex {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    $Stream = [System.IO.File]::OpenRead((Normalize-Path $Path))
    $Sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $Digest = $Sha.ComputeHash($Stream)
        return ([System.BitConverter]::ToString($Digest)).Replace("-", "").ToLowerInvariant()
    }
    finally {
        $Sha.Dispose()
        $Stream.Dispose()
    }
}

function Get-ArtifactEvidence {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$Label
    )

    Assert-File -Path $Path -Description $Label
    $ResolvedPath = Normalize-Path $Path
    $Signature = Get-AuthenticodeSignature -LiteralPath $ResolvedPath
    $Thumbprint = $null
    $Subject = $null
    if ($Signature.SignerCertificate) {
        $Thumbprint = Normalize-Thumbprint $Signature.SignerCertificate.Thumbprint
        $Subject = $Signature.SignerCertificate.Subject
    }

    if ($RequireSigned) {
        if ($Signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
            Fail "$Label is not Authenticode-valid: $($Signature.Status)"
        }
        if (-not $ExpectedSignerThumbprint) {
            Fail "-ExpectedSignerThumbprint is required with -RequireSigned"
        }
    }
    if ($ExpectedSignerThumbprint) {
        $Expected = Normalize-Thumbprint $ExpectedSignerThumbprint
        if ($Thumbprint -ne $Expected) {
            Fail "$Label signer thumbprint does not match the expected publisher"
        }
    }

    return [ordered]@{
        label = $Label
        fileName = [IO.Path]::GetFileName($ResolvedPath)
        path = $ResolvedPath
        sha256 = Get-Sha256Hex $ResolvedPath
        authenticode = [ordered]@{
            status = $Signature.Status.ToString()
            statusMessage = $Signature.StatusMessage
            signerSubject = $Subject
            signerThumbprint = $Thumbprint
        }
    }
}

function Get-ProcessOwnerSid {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Process
    )

    $Owner = Invoke-CimMethod -InputObject $Process -MethodName GetOwnerSid
    if ($Owner.ReturnValue -ne 0 -or -not $Owner.Sid) {
        Fail "could not resolve the owner SID for process $($Process.ProcessId) ($($Process.Name))"
    }
    return $Owner.Sid
}

function Get-LiveProcessEvidence {
    param(
        [Parameter(Mandatory = $true)]
        [string]$CurrentUserSid,

        [Parameter(Mandatory = $true)]
        [string]$CurrentRuntime,

        [Parameter(Mandatory = $true)]
        [string]$ManagedRuntimeRoot
    )

    $Processes = @(Get-CimInstance Win32_Process)
    $ByParent = @{}
    foreach ($Process in $Processes) {
        $ParentKey = [string]$Process.ParentProcessId
        if (-not $ByParent.ContainsKey($ParentKey)) {
            $ByParent[$ParentKey] = [Collections.Generic.List[object]]::new()
        }
        $ByParent[$ParentKey].Add($Process)
    }

    $Rows = [Collections.Generic.List[object]]::new()
    $Seen = @{}
    $CoreNames = @(
        "sessions.exe",
        "sessions-app.exe",
        "sessionsd.exe",
        "sessions-runner.exe"
    )
    $Core = @($Processes | Where-Object { $CoreNames -contains $_.Name.ToLowerInvariant() })
    $CurrentUserCore = [Collections.Generic.List[object]]::new()
    foreach ($Process in $Core) {
        $Name = $Process.Name.ToLowerInvariant()
        $OwnerSid = Get-ProcessOwnerSid $Process
        if ($OwnerSid -eq "S-1-5-18") {
            Fail "$Name process $($Process.ProcessId) is running as LocalSystem"
        }
        if ($OwnerSid -ne $CurrentUserSid) {
            # Another signed-in ordinary user may legitimately run a separate
            # per-user Sessions host. It is outside this evidence boundary.
            continue
        }
        $CurrentUserCore.Add($Process)
        $Role = switch ($Name) {
            "sessionsd.exe" { "daemon" }
            "sessions-runner.exe" { "runner" }
            default { "viewer-or-cli" }
        }
        $ExecutablePath = $Process.ExecutablePath
        if ($Role -eq "daemon") {
            if (-not $ExecutablePath -or -not (Test-PathWithin -Candidate $ExecutablePath -Root $CurrentRuntime)) {
                Fail "daemon process $($Process.ProcessId) is not running from the selected immutable runtime"
            }
        }
        elseif ($Role -eq "runner") {
            if (-not $ExecutablePath -or -not (Test-PathWithin -Candidate $ExecutablePath -Root $ManagedRuntimeRoot)) {
                Fail "runner process $($Process.ProcessId) is not running from a Sessions-managed immutable runtime"
            }
        }

        $Row = [ordered]@{
            role = $Role
            name = $Process.Name
            pid = [int]$Process.ProcessId
            parentPid = [int]$Process.ParentProcessId
            executablePath = $ExecutablePath
            ownerSid = $OwnerSid
            ownerMatchesCurrentUser = $true
        }
        $Rows.Add($Row)
        $Seen[[string]$Process.ProcessId] = $true
    }

    $Runners = @($CurrentUserCore | Where-Object { $_.Name.ToLowerInvariant() -eq "sessions-runner.exe" })
    foreach ($Runner in $Runners) {
        $RunnerKey = [string]$Runner.ProcessId
        $DirectChildren = @()
        if ($ByParent.ContainsKey($RunnerKey)) {
            $DirectChildren = @($ByParent[$RunnerKey])
        }
        foreach ($Child in $DirectChildren) {
            if ($Seen.ContainsKey([string]$Child.ProcessId)) {
                continue
            }
            $OwnerSid = Get-ProcessOwnerSid $Child
            if ($OwnerSid -ne $CurrentUserSid) {
                Fail "provider child $($Child.ProcessId) ($($Child.Name)) does not belong to the signed-in user"
            }
            $Rows.Add([ordered]@{
                role = "provider-child"
                name = $Child.Name
                pid = [int]$Child.ProcessId
                parentPid = [int]$Child.ParentProcessId
                executablePath = $null
                ownerSid = $OwnerSid
                ownerMatchesCurrentUser = $true
            })
            $Seen[[string]$Child.ProcessId] = $true
        }
    }

    $DaemonCount = @($Rows | Where-Object { $_.role -eq "daemon" }).Count
    $RunnerCount = @($Rows | Where-Object { $_.role -eq "runner" }).Count
    if ($DaemonCount -lt 2) {
        Fail "expected a per-user supervisor and serving daemon, found $DaemonCount sessionsd processes"
    }
    if ($RequireRunner -and $RunnerCount -lt 1) {
        Fail "-RequireRunner was supplied but no sessions-runner process is live"
    }

    return @($Rows | Sort-Object role, pid)
}

if ($env:OS -ne "Windows_NT") {
    Fail "this evidence collector must run on Windows"
}
if ($SourceCommit -ne "unknown" -and $SourceCommit -notmatch '^[0-9A-Fa-f]{7,40}$') {
    Fail "-SourceCommit must be unknown or a 7-to-40-character Git commit"
}
if ($CompareBaseline -and -not $LiveHost) {
    Fail "-CompareBaseline requires -LiveHost"
}
if ($RequireRunner -and -not $LiveHost) {
    Fail "-RequireRunner requires -LiveHost"
}

$Identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$Principal = [Security.Principal.WindowsPrincipal]::new($Identity)
$IsAdministrator = $Principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if ($Identity.IsSystem) {
    Fail "run as the signed-in user, not LocalSystem"
}
if ($LiveHost -and $IsAdministrator -and -not $AllowAdministrator) {
    Fail "run the live-host matrix unelevated as a normal user, or pass -AllowAdministrator only for an explicitly reviewed exception"
}

$CliPath = $null
if ($LiveHost) {
    $Cli = Get-Command sessions.exe -CommandType Application -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if (-not $Cli) {
        Fail "sessions.exe is not available on the signed-in user's PATH; open a fresh terminal after Sessions stages the runtime"
    }
    $CliPath = Normalize-Path $Cli.Source
    if (-not $RuntimeDirectory) {
        $RuntimeDirectory = Split-Path -Parent $CliPath
    }
}
if (-not $RuntimeDirectory) {
    Fail "-RuntimeDirectory is required unless -LiveHost can resolve sessions.exe from PATH"
}
$RuntimeDirectory = Normalize-Path $RuntimeDirectory
if (-not (Test-Path -LiteralPath $RuntimeDirectory -PathType Container)) {
    Fail "runtime directory is missing at $RuntimeDirectory"
}

$ManifestPath = Join-Path $RuntimeDirectory "runtime-manifest.json"
Assert-File -Path $ManifestPath -Description "runtime manifest"
$Manifest = Get-Content -Raw -LiteralPath $ManifestPath | ConvertFrom-Json
if ($Manifest.schemaVersion -ne 1) {
    Fail "runtime manifest schema is $($Manifest.schemaVersion), expected 1"
}
if ($Manifest.target -ne "windows-amd64") {
    Fail "runtime manifest target is $($Manifest.target), expected windows-amd64"
}
if (-not $Manifest.runtimeVersion -or $Manifest.runtimeVersion -notmatch '^[A-Za-z0-9._-]{1,128}$') {
    Fail "runtime manifest has an unsafe runtimeVersion"
}
$RequiredBinaries = @("sessions.exe", "sessionsd.exe", "sessions-runner.exe")
$ManifestBinaries = @($Manifest.binaries.PSObject.Properties.Name | Sort-Object)
if (($ManifestBinaries -join "`n") -ne (($RequiredBinaries | Sort-Object) -join "`n")) {
    Fail "runtime manifest must contain exactly sessions.exe, sessionsd.exe, and sessions-runner.exe"
}

$RuntimeBinaries = [Collections.Generic.List[object]]::new()
foreach ($Name in $RequiredBinaries) {
    $Path = Join-Path $RuntimeDirectory $Name
    $Artifact = Get-ArtifactEvidence -Path $Path -Label "runtime/$Name"
    $ExpectedHash = [string]$Manifest.binaries.$Name
    if ($ExpectedHash -notmatch '^[0-9A-Fa-f]{64}$') {
        Fail "runtime manifest digest for $Name is invalid"
    }
    if ($Artifact.sha256 -ne $ExpectedHash.ToLowerInvariant()) {
        Fail "runtime manifest digest mismatch for $Name"
    }
    $RuntimeBinaries.Add($Artifact)
}

$Artifacts = [Collections.Generic.List[object]]::new()
if ($AppPath) {
    $Artifacts.Add((Get-ArtifactEvidence -Path $AppPath -Label "viewer"))
}
if ($InstallerPath) {
    $Artifacts.Add((Get-ArtifactEvidence -Path $InstallerPath -Label "installer"))
}

$OperatingSystem = Get-CimInstance Win32_OperatingSystem
$ComputerSystem = Get-CimInstance Win32_ComputerSystem
$LiveEvidence = $null
if ($LiveHost) {
    if (-not $CliPath.Equals((Normalize-Path (Join-Path $RuntimeDirectory "sessions.exe")), [StringComparison]::OrdinalIgnoreCase)) {
        Fail "sessions.exe on PATH does not resolve to the selected immutable runtime"
    }
    $ManagedRuntimeRoot = Normalize-Path (Split-Path -Parent $RuntimeDirectory)

    $UserPath = [string](Get-ItemPropertyValue -Path "HKCU:\Environment" -Name "Path" -ErrorAction SilentlyContinue)
    $ManagedPathEntries = [Collections.Generic.List[string]]::new()
    foreach ($Entry in @($UserPath -split ';')) {
        $Expanded = [Environment]::ExpandEnvironmentVariables($Entry.Trim())
        if (-not $Expanded) {
            continue
        }
        try {
            if (Test-PathWithin -Candidate $Expanded -Root $ManagedRuntimeRoot) {
                $ManagedPathEntries.Add((Normalize-Path $Expanded))
            }
        }
        catch {
            # Ignore malformed unrelated PATH entries; report only Sessions-managed entries.
        }
    }
    if ($ManagedPathEntries.Count -ne 1) {
        Fail "the persistent user PATH contains $($ManagedPathEntries.Count) Sessions-managed runtime entries, expected exactly one"
    }
    if (-not $ManagedPathEntries[0].Equals($RuntimeDirectory, [StringComparison]::OrdinalIgnoreCase)) {
        Fail "the persistent user PATH does not select the same immutable runtime as sessions.exe"
    }

    $RunKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
    $RunEntry = [string](Get-ItemPropertyValue -Path $RunKey -Name "Somewhere Sessions" -ErrorAction SilentlyContinue)
    if (-not $RunEntry) {
        Fail "the signed-in user's Somewhere Sessions logon definition is missing"
    }
    foreach ($RequiredText in @(
        (Join-Path $RuntimeDirectory "sessionsd.exe"),
        (Join-Path $RuntimeDirectory "sessions-runner.exe"),
        "--supervise",
        "--runner"
    )) {
        if ($RunEntry.IndexOf($RequiredText, [StringComparison]::OrdinalIgnoreCase) -lt 0) {
            Fail "the signed-in user's logon definition does not contain the reviewed immutable runtime and supervisor arguments"
        }
    }

    $VersionOutput = (& $CliPath --version 2>&1 | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $VersionOutput) {
        Fail "sessions --version failed"
    }
    $SupportOutput = (& $CliPath --json support --diagnostics 2>&1 | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $SupportOutput) {
        Fail "sessions --json support --diagnostics failed"
    }
    try {
        $Support = $SupportOutput | ConvertFrom-Json
    }
    catch {
        Fail "sessions --json support --diagnostics did not return JSON"
    }
    if ($Support.schema_version -ne 1 -or $Support.uploaded -ne $false -or -not $Support.diagnostics) {
        Fail "sessions support diagnostics did not preserve the local-only schema"
    }
    if (-not $Support.diagnostics.daemon.reachable -or -not $Support.diagnostics.daemon.ok) {
        Fail "the local daemon is not healthy"
    }
    $DiscoveringProperty = $Support.diagnostics.daemon.PSObject.Properties["discovering"]
    if ($DiscoveringProperty -and $DiscoveringProperty.Value) {
        Fail "runner discovery is still active; wait for a complete baseline before recording evidence"
    }

    $Processes = Get-LiveProcessEvidence `
        -CurrentUserSid $Identity.User.Value `
        -CurrentRuntime $RuntimeDirectory `
        -ManagedRuntimeRoot $ManagedRuntimeRoot
    $Comparison = $null
    if ($CompareBaseline) {
        Assert-File -Path $CompareBaseline -Description "comparison baseline"
        $Baseline = Get-Content -Raw -LiteralPath $CompareBaseline | ConvertFrom-Json
        if ($Baseline.schemaVersion -ne 1 -or -not $Baseline.liveHost -or -not $Baseline.liveHost.processes) {
            Fail "comparison baseline is not a Windows host evidence file with live process data"
        }
        $CurrentKeys = @{}
        foreach ($Process in @($Processes | Where-Object { $_.role -in @("runner", "provider-child") })) {
            $CurrentKeys["$($Process.role):$($Process.pid):$($Process.name)"] = $true
        }
        $ExpectedKeys = @(
            $Baseline.liveHost.processes |
                Where-Object { $_.role -in @("runner", "provider-child") } |
                ForEach-Object { "$($_.role):$($_.pid):$($_.name)" }
        )
        if ($ExpectedKeys.Count -eq 0) {
            Fail "comparison baseline contains no runner/provider PID evidence"
        }
        $Missing = @($ExpectedKeys | Where-Object { -not $CurrentKeys.ContainsKey($_) })
        if ($Missing.Count -ne 0) {
            Fail "runner/provider PID preservation failed; missing baseline processes: $($Missing -join ', ')"
        }
        $Comparison = [ordered]@{
            baselineFile = [IO.Path]::GetFileName($CompareBaseline)
            expectedProcessCount = $ExpectedKeys.Count
            missingProcesses = @()
            preserved = $true
        }
    }

    $LiveEvidence = [ordered]@{
        cli = [ordered]@{
            path = $CliPath
            versionOutput = $VersionOutput
            sanitizedDiagnostics = $Support.diagnostics
        }
        persistentUserPath = [ordered]@{
            managedRuntimeRoot = $ManagedRuntimeRoot
            sessionsEntries = @($ManagedPathEntries)
        }
        logonDefinition = $RunEntry
        processes = @($Processes)
        comparison = $Comparison
    }
}

$Evidence = [ordered]@{
    schemaVersion = 1
    generatedAtUtc = (Get-Date).ToUniversalTime().ToString("o")
    sourceCommit = $SourceCommit.ToLowerInvariant()
    mode = if ($LiveHost) { "live-host" } else { "package" }
    host = [ordered]@{
        windowsVersion = $OperatingSystem.Version
        windowsBuild = $OperatingSystem.BuildNumber
        osArchitecture = $OperatingSystem.OSArchitecture
        processArchitecture = [Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString()
        powerShellVersion = $PSVersionTable.PSVersion.ToString()
        currentUserSid = $Identity.User.Value
        currentUserIsSystem = $Identity.IsSystem
        currentProcessIsAdministrator = $IsAdministrator
        domainRole = [int]$ComputerSystem.DomainRole
    }
    runtime = [ordered]@{
        directory = $RuntimeDirectory
        manifest = [ordered]@{
            schemaVersion = [int]$Manifest.schemaVersion
            runtimeVersion = [string]$Manifest.runtimeVersion
            target = [string]$Manifest.target
        }
        binaries = @($RuntimeBinaries)
    }
    artifacts = @($Artifacts)
    liveHost = $LiveEvidence
    safety = [ordered]@{
        readOnlySystemInspection = $true
        sessionContentCollected = $false
        credentialsCollected = $false
        generalEnvironmentCollected = $false
        processCommandLinesCollected = $false
        uploaded = $false
    }
    result = [ordered]@{
        passed = $true
        summary = if ($LiveHost) {
            "Windows live-host evidence passed without mutating Sessions or session state"
        }
        else {
            "Windows package evidence passed without installation, signing, or runtime mutation"
        }
    }
}

$OutputPath = Normalize-Path $OutputPath
$OutputParent = Split-Path -Parent $OutputPath
if ($OutputParent -and -not (Test-Path -LiteralPath $OutputParent -PathType Container)) {
    [IO.Directory]::CreateDirectory($OutputParent) | Out-Null
}
$Evidence | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $OutputPath -Encoding utf8
Write-Host "Windows host evidence: PASS"
Write-Host "Mode: $($Evidence.mode)"
Write-Host "Runtime: $($Manifest.runtimeVersion)"
Write-Host "Evidence: $OutputPath"
Write-Host "No session lifecycle, credential, network, install, update, or upload action was performed."
