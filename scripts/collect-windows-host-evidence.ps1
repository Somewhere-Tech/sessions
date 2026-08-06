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

    [switch]$RequirePinnedRunner,

    [switch]$RequireRunnerLogs,

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

# Windows narrows the Sessions runtime root to the signed-in user and
# LocalSystem, with a protected (non-inheriting) DACL, because macOS narrows the
# equivalent root to 0700. A directory created with a bare create_dir_all simply
# inherits whatever the profile grants, and anyone who can rewrite sessionsd.exe
# owns the daemon. Nothing else in CI can see the on-disk policy, so record it
# here. Identities are reported as SIDs classified against the current user
# rather than as account names: the question is "is anyone else granted", and a
# SID answers it without adding a name to the evidence file.
function Get-AccessPolicyEvidence {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$Label,

        [Parameter(Mandatory = $true)]
        [string]$CurrentUserSid,

        [switch]$RequireOwnerScoped
    )

    if (-not (Test-Path -LiteralPath $Path)) {
        if ($RequireOwnerScoped) {
            Fail "$Label is missing at $Path, so its access policy cannot be recorded"
        }
        return [ordered]@{
            label = $Label
            path = $Path
            present = $false
            protectedFromInheritance = $null
            rules = @()
            ownerScoped = $null
        }
    }

    $Acl = Get-Acl -LiteralPath $Path
    $Rules = @($Acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    $Rows = [Collections.Generic.List[object]]::new()
    $Foreign = [Collections.Generic.List[string]]::new()
    foreach ($Rule in $Rules) {
        $Sid = [string]$Rule.IdentityReference.Value
        $Principal = switch ($Sid) {
            $CurrentUserSid { "signed-in user" }
            "S-1-5-18" { "LocalSystem" }
            "S-1-5-32-544" { "Administrators" }
            default { "other" }
        }
        if ($Principal -eq "other" -or $Principal -eq "Administrators") {
            $Foreign.Add("$Principal ($Sid)")
        }
        $Rows.Add([ordered]@{
            principal = $Principal
            sid = $Sid
            rights = $Rule.FileSystemRights.ToString()
            access = $Rule.AccessControlType.ToString()
            inherited = [bool]$Rule.IsInherited
            inheritanceFlags = $Rule.InheritanceFlags.ToString()
        })
    }

    $OwnerScoped = ($Foreign.Count -eq 0)
    if ($RequireOwnerScoped) {
        if (-not $Acl.AreAccessRulesProtected) {
            Fail "$Label at $Path inherits its access policy instead of keeping a protected owner-scoped DACL"
        }
        if (-not $OwnerScoped) {
            Fail "$Label at $Path grants principals beyond the signed-in user and LocalSystem: $($Foreign -join ', ')"
        }
    }

    return [ordered]@{
        label = $Label
        path = $Path
        present = $true
        protectedFromInheritance = [bool]$Acl.AreAccessRulesProtected
        rules = @($Rows)
        ownerScoped = $OwnerScoped
    }
}

# The ledger, uploads, machine credentials, the backup key and hooks all used to
# be written to hardcoded Unix locations on Windows. The most visible fingerprint
# of that class of bug is a directory named literally "~", created because a
# path beginning "~/" was joined rather than expanded. Look for it in the places
# a Sessions process would have been standing.
function Get-LiteralTildeEvidence {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$Roots
    )

    $Found = [Collections.Generic.List[string]]::new()
    $Checked = [Collections.Generic.List[string]]::new()
    foreach ($Root in $Roots) {
        if (-not $Root -or -not (Test-Path -LiteralPath $Root -PathType Container)) {
            continue
        }
        $Candidate = Join-Path $Root "~"
        $Checked.Add($Candidate)
        if (Test-Path -LiteralPath $Candidate) {
            $Found.Add($Candidate)
        }
    }
    if ($Found.Count -ne 0) {
        Fail "a literal '~' path exists, which is what an unexpanded Unix home path looks like on Windows: $($Found -join ', ')"
    }
    return [ordered]@{
        checkedPaths = @($Checked)
        literalTildePaths = @()
    }
}

# Directory shape only: whether the expected roots exist and how many artifacts
# of each kind they hold. File names are session identifiers and are not
# recorded here, and no file is opened.
function Get-DirectoryShape {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$Label
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
        return [ordered]@{ label = $Label; path = $Path; present = $false; entryCount = 0; countsBySuffix = [ordered]@{} }
    }
    $Entries = @(Get-ChildItem -LiteralPath $Path -File -Force -ErrorAction SilentlyContinue)
    $Counts = @{}
    foreach ($Entry in $Entries) {
        $Suffix = [IO.Path]::GetExtension($Entry.Name)
        if (-not $Suffix) {
            $Suffix = "(none)"
        }
        if (-not $Counts.ContainsKey($Suffix)) {
            $Counts[$Suffix] = 0
        }
        $Counts[$Suffix] = $Counts[$Suffix] + 1
    }
    $Ordered = [ordered]@{}
    foreach ($Key in @($Counts.Keys | Sort-Object)) {
        $Ordered[$Key] = $Counts[$Key]
    }
    return [ordered]@{
        label = $Label
        path = $Path
        present = $true
        entryCount = $Entries.Count
        countsBySuffix = $Ordered
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
            # Windows reuses process ids. The launcher pairs a pid with the
            # creation time captured while it still held the start handle, and
            # refuses to reap anything whose creation time has changed. The
            # comparison below applies the same rule, so a preserved-looking pid
            # that is really a different process is reported as a loss.
            creationTimeUtc = if ($Process.CreationDate) {
                ([datetime]$Process.CreationDate).ToUniversalTime().ToString("o")
            }
            else {
                $null
            }
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
                creationTimeUtc = if ($Child.CreationDate) {
                    ([datetime]$Child.CreationDate).ToUniversalTime().ToString("o")
                }
                else {
                    $null
                }
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
if ($RequirePinnedRunner -and -not $LiveHost) {
    Fail "-RequirePinnedRunner requires -LiveHost"
}
if ($RequireRunnerLogs -and -not $LiveHost) {
    Fail "-RequireRunnerLogs requires -LiveHost"
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
        "--supervise",
        "--runner"
    )) {
        if ($RunEntry.IndexOf($RequiredText, [StringComparison]::OrdinalIgnoreCase) -lt 0) {
            Fail "the signed-in user's logon definition does not contain the reviewed immutable runtime and supervisor arguments"
        }
    }

    # The daemon is versioned; the runner is not. The logon definition carries
    # the runner path the daemon hands to every new session, so naming the
    # versioned copy there ties every future session to a directory the next
    # update replaces — and Windows will not delete an image a live runner is
    # executing. The definition therefore names one pinned path directly under
    # the managed root, and the bytes behind it are swapped by rename.
    #
    # This collector used to require the versioned runner path here and would
    # now fail on a correct host. Parse the path instead, record which shape it
    # has, and require the pinned shape only when the caller asks for it, so an
    # older installation can still be captured as an update baseline.
    $RunnerMarker = ' --runner "'
    $RunnerMarkerIndex = $RunEntry.IndexOf($RunnerMarker, [StringComparison]::OrdinalIgnoreCase)
    if ($RunnerMarkerIndex -lt 0 -or -not $RunEntry.EndsWith('"')) {
        Fail "the signed-in user's logon definition does not carry a quoted runner path"
    }
    $DefinedRunner = $RunEntry.Substring(
        $RunnerMarkerIndex + $RunnerMarker.Length,
        $RunEntry.Length - $RunnerMarkerIndex - $RunnerMarker.Length - 1
    )
    if (-not $DefinedRunner -or -not (Test-PathWithin -Candidate $DefinedRunner -Root $ManagedRuntimeRoot)) {
        Fail "the signed-in user's logon definition names a runner outside the Sessions-managed runtime root"
    }
    $DefinedRunner = Normalize-Path $DefinedRunner
    $PinnedRunnerPath = Normalize-Path (Join-Path $ManagedRuntimeRoot "sessions-runner.exe")
    $RunnerShape = if ($DefinedRunner.Equals($PinnedRunnerPath, [StringComparison]::OrdinalIgnoreCase)) {
        "pinned"
    }
    elseif ($DefinedRunner.Equals((Normalize-Path (Join-Path $RuntimeDirectory "sessions-runner.exe")), [StringComparison]::OrdinalIgnoreCase)) {
        "versioned"
    }
    else {
        "unrecognized"
    }
    if ($RunnerShape -eq "unrecognized") {
        Fail "the signed-in user's logon definition names an unrecognized runner path inside the managed runtime root"
    }
    if ($RequirePinnedRunner -and $RunnerShape -ne "pinned") {
        Fail "-RequirePinnedRunner was supplied but the logon definition still names the versioned runner at $DefinedRunner"
    }

    # A pinned runner must carry the bytes the current manifest describes,
    # because that is the whole claim the indirection makes: an update swaps the
    # file behind one unchanging path. The aside copies a swap leaves behind are
    # recorded rather than treated as faults — a retired copy that could not be
    # deleted means a live runner is still executing it, which is the outcome
    # this design wants.
    $ExpectedRunnerDigest = ([string]$Manifest.binaries."sessions-runner.exe").ToLowerInvariant()
    $StableRunnerPresent = Test-Path -LiteralPath $PinnedRunnerPath -PathType Leaf
    $StableRunnerDigest = $null
    if ($StableRunnerPresent) {
        $StableRunnerDigest = Get-Sha256Hex $PinnedRunnerPath
        if ($RunnerShape -eq "pinned" -and $StableRunnerDigest -ne $ExpectedRunnerDigest) {
            Fail "the pinned runner at $PinnedRunnerPath does not match the runtime manifest digest for sessions-runner.exe"
        }
    }
    elseif ($RunnerShape -eq "pinned") {
        Fail "the logon definition names the pinned runner at $PinnedRunnerPath, which does not exist"
    }
    $RunnerAsideCopies = [Collections.Generic.List[object]]::new()
    foreach ($Aside in @(Get-ChildItem -LiteralPath $ManagedRuntimeRoot -File -Force -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -like ".sessions-runner-retired-*" -or $_.Name -like ".sessions-runner-staged-*" })) {
        $RunnerAsideCopies.Add([ordered]@{
            fileName = $Aside.Name
            kind = if ($Aside.Name -like ".sessions-runner-retired-*") { "retired" } else { "staged" }
            sizeBytes = [long]$Aside.Length
            ageHours = [math]::Round(((Get-Date) - $Aside.LastWriteTime).TotalHours, 2)
        })
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

    # `sessions doctor` used to run a Unix PTY preflight against /usr/bin/true on
    # Windows and exit 2 telling the user to run xcode-select. Exit 2 is that
    # failure and nothing else, so it is the assertion. A non-zero exit of 1 is
    # a real finding about the fleet, not a collector failure, and is recorded.
    # Only counts and the distinct probe values are kept: the rows carry session
    # identifiers and tool names, which are session content.
    $DoctorOutput = (& $CliPath --json doctor 2>&1 | Out-String).Trim()
    $DoctorExit = $LASTEXITCODE
    if ($DoctorExit -eq 2) {
        Fail "sessions doctor exited 2, which on Windows means its terminal preflight ran a Unix probe that cannot exist here"
    }
    if (-not $DoctorOutput) {
        Fail "sessions --json doctor produced no output"
    }
    try {
        $Doctor = $DoctorOutput | ConvertFrom-Json
    }
    catch {
        Fail "sessions --json doctor did not return JSON"
    }
    $DoctorRows = @()
    if ($Doctor -and $Doctor.PSObject.Properties["sessions"] -and $Doctor.sessions) {
        $DoctorRows = @($Doctor.sessions)
    }
    $DoctorEvidence = [ordered]@{
        exitCode = $DoctorExit
        sessionCount = $DoctorRows.Count
        needsRecreate = @($DoctorRows | Where-Object { -not $_.ok }).Count
        # Windows has no launchd service QoS and this contract does not read
        # process command lines, so both probes must report "n/a" rather than
        # inventing a fault the host cannot have.
        distinctQos = @($DoctorRows | ForEach-Object { [string]$_.qos } | Sort-Object -Unique)
        distinctSpawn = @($DoctorRows | ForEach-Object { [string]$_.spawn } | Sort-Object -Unique)
    }

    # Session identifiers, and nothing else from the record. PIDs prove a
    # process survived; only the identifiers prove the session did. `list`
    # rather than `ls` because a headless lane is also a runner this evidence
    # has to account for, and `ls` deliberately excludes lanes.
    $SessionsOutput = (& $CliPath --json list 2>&1 | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $SessionsOutput) {
        Fail "sessions --json list failed"
    }
    try {
        $SessionRecords = @($SessionsOutput | ConvertFrom-Json)
    }
    catch {
        Fail "sessions --json list did not return JSON"
    }
    $SessionIDs = @(
        $SessionRecords |
            ForEach-Object {
                $IDProperty = $_.PSObject.Properties["id"]
                if ($IDProperty) { [string]$IDProperty.Value } else { "" }
            } |
            Where-Object { $_ } |
            Sort-Object
    )
    if ($RequireRunner -and $SessionIDs.Count -lt 1) {
        Fail "-RequireRunner was supplied but the daemon reports no live session"
    }

    # State locations. The ledger, uploads, machine credentials, the backup key
    # and hooks were all written to hardcoded Unix paths on Windows. The Go
    # runtime keeps state under %LOCALAPPDATA%\Sessions; the packaged runtime is
    # a separate root under the bundle identifier. Record the shape of both, and
    # never the file names inside them.
    $LocalAppData = [string]$env:LOCALAPPDATA
    if (-not $LocalAppData) {
        Fail "LOCALAPPDATA is unset, so the Windows state locations cannot be resolved"
    }
    $SessionsStateRoot = Normalize-Path (Join-Path $LocalAppData "Sessions\state")
    $SessionsConfigRoot = Normalize-Path (Join-Path $LocalAppData "Sessions\config")
    $StateLocations = @(
        (Get-DirectoryShape -Path $SessionsStateRoot -Label "state root"),
        (Get-DirectoryShape -Path (Join-Path $SessionsStateRoot "runners") -Label "runner artifacts"),
        (Get-DirectoryShape -Path (Join-Path $SessionsStateRoot "ledger") -Label "ledger"),
        (Get-DirectoryShape -Path (Join-Path $SessionsStateRoot "uploads") -Label "uploads"),
        (Get-DirectoryShape -Path $SessionsConfigRoot -Label "config root (backup key, hooks)"),
        (Get-DirectoryShape -Path $ManagedRuntimeRoot -Label "managed runtime root"),
        (Get-DirectoryShape -Path $RuntimeDirectory -Label "active immutable runtime")
    )
    $TildeEvidence = Get-LiteralTildeEvidence -Roots @(
        [string]$env:USERPROFILE,
        $LocalAppData,
        $SessionsStateRoot,
        $SessionsConfigRoot,
        $ManagedRuntimeRoot,
        $RuntimeDirectory
    )

    # A Windows runner's output went nowhere at all before this batch, which is
    # exactly the case where the user most needs to know where a session went.
    # The launcher opens <state>\runners\<id>.log before it starts the process,
    # so the file existing is the evidence. An empty log is normal for a healthy
    # runner and is not a failure.
    #
    # This is asserted only under -RequireRunnerLogs, because a session adopted
    # from a build that predates the fix legitimately has no log, and the update
    # comparison is exactly the run that carries such sessions across.
    $RunnerLogDir = Join-Path $SessionsStateRoot "runners"
    $RunnerLogs = [Collections.Generic.List[object]]::new()
    foreach ($SessionID in $SessionIDs) {
        $LogPath = Join-Path $RunnerLogDir ("{0}.log" -f $SessionID)
        $Present = Test-Path -LiteralPath $LogPath -PathType Leaf
        $RunnerLogs.Add([ordered]@{
            sessionId = $SessionID
            present = $Present
            sizeBytes = if ($Present) { [long](Get-Item -LiteralPath $LogPath -Force).Length } else { $null }
        })
    }
    if ($RequireRunnerLogs) {
        $MissingLogs = @($RunnerLogs | Where-Object { -not $_.present })
        if ($SessionIDs.Count -lt 1) {
            Fail "-RequireRunnerLogs was supplied but the daemon reports no live session to have logged anything"
        }
        if ($MissingLogs.Count -ne 0) {
            Fail "$($MissingLogs.Count) live sessions have no runner log under $RunnerLogDir, so Windows runner output is going nowhere"
        }
    }

    # The runtime root and every versioned directory under it must stay
    # owner-scoped. The Go state root is reported but not asserted: it is
    # created with a Unix mode that Windows ignores, so it inherits whatever the
    # profile grants. That asymmetry is a finding to read, not a gate.
    $CurrentUserSid = $Identity.User.Value
    $AccessPolicy = @(
        (Get-AccessPolicyEvidence -Path $ManagedRuntimeRoot -Label "managed runtime root" -CurrentUserSid $CurrentUserSid -RequireOwnerScoped),
        (Get-AccessPolicyEvidence -Path $RuntimeDirectory -Label "active immutable runtime" -CurrentUserSid $CurrentUserSid -RequireOwnerScoped),
        (Get-AccessPolicyEvidence -Path (Join-Path $RuntimeDirectory "sessionsd.exe") -Label "daemon binary" -CurrentUserSid $CurrentUserSid),
        (Get-AccessPolicyEvidence -Path $PinnedRunnerPath -Label "pinned runner" -CurrentUserSid $CurrentUserSid),
        (Get-AccessPolicyEvidence -Path $SessionsStateRoot -Label "Go state root (inherited by design)" -CurrentUserSid $CurrentUserSid),
        (Get-AccessPolicyEvidence -Path $SessionsConfigRoot -Label "Go config root (inherited by design)" -CurrentUserSid $CurrentUserSid)
    )

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
        # The key carries the creation time as well as the pid. Windows recycles
        # pids, so a matching pid alone would let an unrelated process stand in
        # for a runner that actually died and report the update as safe. A
        # baseline recorded before creation times were captured has an empty
        # component here and still compares on pid, which is the old behavior
        # and is reported as such rather than silently downgraded.
        $BaselineCarriesCreationTimes = $true
        $CurrentKeys = @{}
        foreach ($Process in @($Processes | Where-Object { $_.role -in @("runner", "provider-child") })) {
            $CurrentKeys["$($Process.role):$($Process.pid):$($Process.name):$($Process.creationTimeUtc)"] = $true
            $CurrentKeys["$($Process.role):$($Process.pid):$($Process.name):"] = $true
        }
        $ExpectedKeys = @(
            $Baseline.liveHost.processes |
                Where-Object { $_.role -in @("runner", "provider-child") } |
                ForEach-Object {
                    $CreationProperty = $_.PSObject.Properties["creationTimeUtc"]
                    $Creation = if ($CreationProperty) { [string]$CreationProperty.Value } else { "" }
                    if (-not $Creation) {
                        $script:BaselineCarriesCreationTimes = $false
                    }
                    "$($_.role):$($_.pid):$($_.name):$Creation"
                }
        )
        if ($ExpectedKeys.Count -eq 0) {
            Fail "comparison baseline contains no runner/provider PID evidence"
        }
        $Missing = @($ExpectedKeys | Where-Object { -not $CurrentKeys.ContainsKey($_) })
        if ($Missing.Count -ne 0) {
            Fail "runner/provider PID preservation failed; missing baseline processes: $($Missing -join ', ')"
        }

        # A preserved process is not a preserved session. The daemon can hold a
        # runner alive and still lose the record that makes it reachable, which
        # is exactly what a reaping bug looks like from the user's side, so the
        # identifiers are compared too.
        $BaselineSessionIDs = @()
        $SessionProperty = $Baseline.liveHost.PSObject.Properties["sessionIds"]
        if ($SessionProperty -and $SessionProperty.Value) {
            $BaselineSessionIDs = @($SessionProperty.Value | ForEach-Object { [string]$_ })
        }
        $CurrentSessionSet = @{}
        foreach ($SessionID in $SessionIDs) {
            $CurrentSessionSet[$SessionID] = $true
        }
        $MissingSessions = @($BaselineSessionIDs | Where-Object { -not $CurrentSessionSet.ContainsKey($_) })
        if ($MissingSessions.Count -ne 0) {
            Fail "session preservation failed; the daemon no longer reports baseline sessions: $($MissingSessions -join ', ')"
        }

        $Comparison = [ordered]@{
            baselineFile = [IO.Path]::GetFileName($CompareBaseline)
            expectedProcessCount = $ExpectedKeys.Count
            missingProcesses = @()
            baselineCarriesCreationTimes = $script:BaselineCarriesCreationTimes
            expectedSessionCount = $BaselineSessionIDs.Count
            missingSessions = @()
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
        supervisorRunner = [ordered]@{
            definedPath = $DefinedRunner
            shape = $RunnerShape
            pinnedPath = $PinnedRunnerPath
            pinnedRunnerPresent = $StableRunnerPresent
            pinnedRunnerSha256 = $StableRunnerDigest
            manifestRunnerSha256 = $ExpectedRunnerDigest
            asideCopies = @($RunnerAsideCopies)
        }
        accessPolicy = @($AccessPolicy)
        stateLocations = @($StateLocations)
        unixHomeLeakCheck = $TildeEvidence
        runnerLogs = @($RunnerLogs)
        doctor = $DoctorEvidence
        sessionIds = @($SessionIDs)
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
        # State directories are recorded by shape only: whether the expected
        # roots exist and how many artifacts of each suffix they hold. The file
        # names inside them are session identifiers and are not collected, and
        # no state file is opened.
        stateFileNamesCollected = $false
        stateFileContentsCollected = $false
        # Access-policy identities are SIDs classified against the current user,
        # not account names.
        accountNamesCollected = $false
        uploaded = $false
        # The evidence file does contain absolute paths under the signed-in
        # user's profile, because the runtime, PATH, and state assertions are
        # about those exact locations. Review it before sharing it outside the
        # release record.
        userProfilePathsPresent = $true
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
