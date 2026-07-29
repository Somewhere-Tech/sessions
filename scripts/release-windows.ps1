[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Version,

    [Parameter(Mandatory = $true)]
    [string]$NotesFile,

    [string]$OutputDirectory,

    [string]$BaseManifest,

    [switch]$DryRun
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Split-Path -Parent $ScriptDir
$ExpectedPublicKey = Join-Path $RepoRoot "release\updater.pub"
$ReleaseConfig = Join-Path $RepoRoot "src-tauri\tauri.windows.release.conf.json"
$TimestampUrl = "http://timestamp.digicert.com"

function Fail {
    param([string]$Message)
    throw "release-windows: $Message"
}

function Normalize-Thumbprint {
    param([string]$Thumbprint)
    return ($Thumbprint -replace '[^0-9A-Fa-f]', '').ToUpperInvariant()
}

function Get-Sha256Hex {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    $Stream = [System.IO.File]::OpenRead([IO.Path]::GetFullPath($Path))
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

function Assert-ReleaseVersion {
    if ($Version -notmatch '^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$') {
        Fail "-Version must be a semantic version without a leading v"
    }
    if (-not (Test-Path -LiteralPath $NotesFile -PathType Leaf)) {
        Fail "-NotesFile must name an existing file"
    }

    $ConfigVersion = (Get-Content -Raw (Join-Path $RepoRoot "src-tauri\tauri.conf.json") | ConvertFrom-Json).version
    $FrontendVersion = (Get-Content -Raw (Join-Path $RepoRoot "frontend\package.json") | ConvertFrom-Json).version
    $CargoVersion = Select-String -Path (Join-Path $RepoRoot "src-tauri\Cargo.toml") -Pattern '^version = "([^"]+)"' |
        Select-Object -First 1 |
        ForEach-Object { $_.Matches[0].Groups[1].Value }
    foreach ($Pair in @(
        @{ Name = "src-tauri/tauri.conf.json"; Value = $ConfigVersion },
        @{ Name = "frontend/package.json"; Value = $FrontendVersion },
        @{ Name = "src-tauri/Cargo.toml"; Value = $CargoVersion }
    )) {
        if ($Pair.Value -ne $Version) {
            Fail "$($Pair.Name) is $($Pair.Value), expected $Version"
        }
    }
}

function Assert-ReleaseConfiguration {
    if (-not (Test-Path -LiteralPath $ReleaseConfig -PathType Leaf)) {
        Fail "Windows release config is missing at $ReleaseConfig"
    }
    $Config = Get-Content -Raw $ReleaseConfig | ConvertFrom-Json
    if ($Config.bundle.createUpdaterArtifacts -ne $true) {
        Fail "Windows release config must create updater artifacts"
    }
    if ($Config.bundle.windows.digestAlgorithm -ne "sha256") {
        Fail "Windows release config must use SHA-256 Authenticode digests"
    }
    if ($Config.bundle.windows.timestampUrl -ne $TimestampUrl) {
        Fail "Windows release config must use the reviewed RFC 3161 timestamp endpoint"
    }
    if ($Config.plugins.updater.windows.installMode -ne "passive") {
        Fail "Windows updater install mode must remain passive and current-user"
    }
    if (-not (Test-Path -LiteralPath $ExpectedPublicKey -PathType Leaf)) {
        Fail "pinned updater public key is missing at $ExpectedPublicKey"
    }
    $PinnedKey = (Get-Content -Raw $ExpectedPublicKey).Trim()
    $AppKey = (Get-Content -Raw (Join-Path $RepoRoot "src-tauri\tauri.conf.json") | ConvertFrom-Json).plugins.updater.pubkey.Trim()
    if ($PinnedKey -ne $AppKey) {
        Fail "release/updater.pub does not match the public key pinned in Sessions"
    }
}

function Find-SignTool {
    $Command = Get-Command signtool.exe -ErrorAction SilentlyContinue
    if ($Command) {
        return $Command.Source
    }
    $WindowsKits = Join-Path ${env:ProgramFiles(x86)} "Windows Kits\10\bin"
    if (Test-Path $WindowsKits) {
        $Candidate = Get-ChildItem -Path $WindowsKits -Recurse -Filter signtool.exe -File |
            Where-Object { $_.FullName -match '\\x64\\signtool\.exe$' } |
            Sort-Object FullName -Descending |
            Select-Object -First 1
        if ($Candidate) {
            return $Candidate.FullName
        }
    }
    Fail "Windows SDK signtool.exe is required"
}

function Assert-AuthenticodeFile {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$Thumbprint,

        [Parameter(Mandatory = $true)]
        [string]$SignTool
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        Fail "signed artifact is missing at $Path"
    }
    & $SignTool verify /pa /all /v $Path
    if ($LASTEXITCODE -ne 0) {
        Fail "signtool rejected $Path"
    }
    $Signature = Get-AuthenticodeSignature -FilePath $Path
    if ($Signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
        Fail "Windows rejected $Path with Authenticode status $($Signature.Status)"
    }
    if ((Normalize-Thumbprint $Signature.SignerCertificate.Thumbprint) -ne $Thumbprint) {
        Fail "Authenticode signer mismatch on $Path"
    }
}

Assert-ReleaseVersion
Assert-ReleaseConfiguration

if (-not $OutputDirectory) {
    $OutputDirectory = Join-Path $RepoRoot "release\out\v$Version\windows"
}
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
$NotesFile = [IO.Path]::GetFullPath($NotesFile)
if ($BaseManifest) {
    $BaseManifest = [IO.Path]::GetFullPath($BaseManifest)
    if (-not (Test-Path -LiteralPath $BaseManifest -PathType Leaf)) {
        Fail "-BaseManifest must name an existing manifest"
    }
}

if ($DryRun) {
    Write-Host "release version: $Version"
    Write-Host "updater target: windows-x86_64"
    Write-Host "Authenticode: SHA-256 plus RFC 3161 timestamp"
    Write-Host "updater trust: release/updater.pub"
    Write-Host "output: $OutputDirectory"
    if ($BaseManifest) {
        Write-Host "manifest merge base: $BaseManifest"
    }
    Write-Host "dry run: no certificate read, build, signing, upload, publication, installation, or runtime action performed"
    exit 0
}

if (-not $IsWindows) {
    Fail "signed Windows releases must run on Windows"
}
foreach ($RequiredCommand in @("git", "go", "node", "npm")) {
    if (-not (Get-Command $RequiredCommand -ErrorAction SilentlyContinue)) {
        Fail "required command not found: $RequiredCommand"
    }
}
if ((& git -C $RepoRoot status --porcelain=v1)) {
    Fail "release builds require a clean reviewed worktree"
}

$CertificatePath = $env:SESSIONS_WINDOWS_CERTIFICATE_PATH
$CertificatePassword = $env:SESSIONS_WINDOWS_CERTIFICATE_PASSWORD
$UpdaterKeyPath = $env:SESSIONS_UPDATER_KEY_PATH
if (-not $CertificatePath -or -not (Test-Path -LiteralPath $CertificatePath -PathType Leaf)) {
    Fail "SESSIONS_WINDOWS_CERTIFICATE_PATH must name the user-authorized PFX"
}
if (-not $CertificatePassword) {
    Fail "SESSIONS_WINDOWS_CERTIFICATE_PASSWORD is required"
}
if (-not $UpdaterKeyPath -or -not (Test-Path -LiteralPath $UpdaterKeyPath -PathType Leaf) -or -not (Test-Path -LiteralPath "$UpdaterKeyPath.pub" -PathType Leaf)) {
    Fail "SESSIONS_UPDATER_KEY_PATH must name the updater keypair"
}
if ((Get-Content -Raw "$UpdaterKeyPath.pub").Trim() -ne (Get-Content -Raw $ExpectedPublicKey).Trim()) {
    Fail "updater public key does not match the key pinned in Sessions"
}

$ExistingThumbprints = @{}
Get-ChildItem Cert:\CurrentUser\My | ForEach-Object {
    $ExistingThumbprints[(Normalize-Thumbprint $_.Thumbprint)] = $true
}
$ImportedThumbprints = @()
$GeneratedConfig = Join-Path ([IO.Path]::GetTempPath()) "sessions-windows-release-$([Guid]::NewGuid().ToString('N')).json"
$PreviousEnvironment = @{
    SESSIONS_WINDOWS_SIGNING_MODE = $env:SESSIONS_WINDOWS_SIGNING_MODE
    SESSIONS_WINDOWS_CERTIFICATE_THUMBPRINT = $env:SESSIONS_WINDOWS_CERTIFICATE_THUMBPRINT
    SESSIONS_WINDOWS_TIMESTAMP_URL = $env:SESSIONS_WINDOWS_TIMESTAMP_URL
    TAURI_SIGNING_PRIVATE_KEY = $env:TAURI_SIGNING_PRIVATE_KEY
    TAURI_SIGNING_PRIVATE_KEY_PASSWORD = $env:TAURI_SIGNING_PRIVATE_KEY_PASSWORD
}

try {
    $SecurePassword = ConvertTo-SecureString -String $CertificatePassword -AsPlainText -Force
    $Imported = @(Import-PfxCertificate -FilePath $CertificatePath -CertStoreLocation Cert:\CurrentUser\My -Password $SecurePassword)
    if ($Imported.Count -eq 0) {
        Fail "the Windows certificate bundle did not import a certificate"
    }
    $Imported | ForEach-Object {
        $Normalized = Normalize-Thumbprint $_.Thumbprint
        if (-not $ExistingThumbprints.ContainsKey($Normalized)) {
            $ImportedThumbprints += $Normalized
        }
    }

    $CodeSigningOid = "1.3.6.1.5.5.7.3.3"
    $Signer = $Imported |
        Where-Object {
            $_.HasPrivateKey -and
            $_.NotBefore -le (Get-Date) -and
            $_.NotAfter -gt (Get-Date) -and
            ($_.EnhancedKeyUsageList.ObjectId.Value -contains $CodeSigningOid)
        } |
        Select-Object -First 1
    if (-not $Signer) {
        Fail "the PFX has no currently valid code-signing certificate with a private key"
    }
    $Thumbprint = Normalize-Thumbprint $Signer.Thumbprint
    if ($Thumbprint -notmatch '^[0-9A-F]{40}$') {
        Fail "the code-signing certificate has an invalid thumbprint"
    }

    $Config = Get-Content -Raw $ReleaseConfig | ConvertFrom-Json
    $Config.bundle.windows | Add-Member -NotePropertyName certificateThumbprint -NotePropertyValue $Thumbprint -Force
    $Config | ConvertTo-Json -Depth 10 | Set-Content -Encoding utf8 $GeneratedConfig

    $env:SESSIONS_WINDOWS_SIGNING_MODE = "authenticode"
    $env:SESSIONS_WINDOWS_CERTIFICATE_THUMBPRINT = $Thumbprint
    $env:SESSIONS_WINDOWS_TIMESTAMP_URL = $TimestampUrl
    $env:TAURI_SIGNING_PRIVATE_KEY = $UpdaterKeyPath
    if ($null -eq $env:TAURI_SIGNING_PRIVATE_KEY_PASSWORD) {
        $env:TAURI_SIGNING_PRIVATE_KEY_PASSWORD = ""
    }

    Push-Location $RepoRoot
    try {
        & npm.cmd exec tauri build -- --bundles nsis --config $GeneratedConfig
        if ($LASTEXITCODE -ne 0) {
            Fail "Tauri Windows release build failed"
        }
    }
    finally {
        Pop-Location
    }

    $SignTool = Find-SignTool
    $ReleaseExecutable = Join-Path $RepoRoot "src-tauri\target\release\sessions-app.exe"
    Assert-AuthenticodeFile -Path $ReleaseExecutable -Thumbprint $Thumbprint -SignTool $SignTool
    foreach ($RuntimeBinary in @("sessions.exe", "sessionsd.exe", "sessions-runner.exe")) {
        Assert-AuthenticodeFile -Path (Join-Path $RepoRoot "src-tauri\runtime\$RuntimeBinary") -Thumbprint $Thumbprint -SignTool $SignTool
    }

    $Installers = @(Get-ChildItem -Path (Join-Path $RepoRoot "src-tauri\target\release\bundle\nsis") -Filter "*-setup.exe" -File)
    if ($Installers.Count -ne 1) {
        Fail "expected exactly one NSIS installer, found $($Installers.Count)"
    }
    $Installer = $Installers[0]
    Assert-AuthenticodeFile -Path $Installer.FullName -Thumbprint $Thumbprint -SignTool $SignTool
    $UpdaterSignature = "$($Installer.FullName).sig"
    $UpdaterSignatureText = if (Test-Path -LiteralPath $UpdaterSignature -PathType Leaf) {
        (Get-Content -Raw $UpdaterSignature).Trim()
    }
    else {
        ""
    }
    if (-not $UpdaterSignatureText) {
        Fail "Tauri did not create the signed NSIS updater envelope at $UpdaterSignature"
    }
    & node (Join-Path $RepoRoot "scripts\verify-updater-signature.mjs") `
        --public-key $ExpectedPublicKey `
        --artifact $Installer.FullName `
        --signature $UpdaterSignature
    if ($LASTEXITCODE -ne 0) {
        Fail "the NSIS updater envelope did not verify with release/updater.pub"
    }

    New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
    $OutputInstaller = Join-Path $OutputDirectory $Installer.Name
    $OutputSignature = "$OutputInstaller.sig"
    Copy-Item -LiteralPath $Installer.FullName -Destination $OutputInstaller -Force
    Copy-Item -LiteralPath $UpdaterSignature -Destination $OutputSignature -Force
    $Digest = Get-Sha256Hex -Path $OutputInstaller
    "$Digest  $($Installer.Name)" | Set-Content -Encoding ascii "$OutputInstaller.sha256"

    $ArtifactUrl = "https://github.com/somewhere-tech/sessions/releases/download/v$Version/$($Installer.Name)"
    $Manifest = Join-Path $OutputDirectory $(if ($BaseManifest) { "latest.json" } else { "latest.windows.json" })
    $Arguments = @(
        (Join-Path $RepoRoot "scripts\render-updater-manifest.mjs"),
        "--version", $Version,
        "--artifact", $OutputInstaller,
        "--url", $ArtifactUrl,
        "--target", "windows-x86_64",
        "--notes-file", $NotesFile,
        "--output", $Manifest
    )
    if ($BaseManifest) {
        $Arguments += @("--base", $BaseManifest)
    }
    & node @Arguments
    if ($LASTEXITCODE -ne 0) {
        Fail "rendering the Windows updater manifest failed"
    }

    Write-Host "Windows release candidate verified for publisher $($Signer.Subject)"
    Write-Host "immutable installer: $OutputInstaller"
    Write-Host "Tauri updater signature: $OutputSignature"
    Write-Host "manifest: $Manifest"
    Write-Host "No artifact was uploaded, published, installed, or promoted."
}
finally {
    foreach ($Name in $PreviousEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($Name, $PreviousEnvironment[$Name], "Process")
    }
    if (Test-Path -LiteralPath $GeneratedConfig) {
        Remove-Item -LiteralPath $GeneratedConfig -Force
    }
    foreach ($Thumbprint in $ImportedThumbprints) {
        $Certificate = "Cert:\CurrentUser\My\$Thumbprint"
        if (Test-Path $Certificate) {
            Remove-Item $Certificate -Force
        }
    }
}
