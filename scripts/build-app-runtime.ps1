$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Split-Path -Parent $ScriptDir
$GoRoot = Join-Path $RepoRoot "runtime"
$FrontendDist = Join-Path $RepoRoot "frontend\dist"
$EmbeddedAssets = Join-Path $GoRoot "internal\webassets\dist"
$RuntimeDir = Join-Path $RepoRoot "src-tauri\runtime"

if (-not (Test-Path (Join-Path $FrontendDist "index.html"))) {
    throw "frontend build missing at $FrontendDist"
}
foreach ($RequiredCommand in @("go", "git")) {
    if (-not (Get-Command $RequiredCommand -ErrorAction SilentlyContinue)) {
        throw "required command not found: $RequiredCommand"
    }
}

$SigningMode = if ($env:SESSIONS_WINDOWS_SIGNING_MODE) {
    $env:SESSIONS_WINDOWS_SIGNING_MODE.Trim().ToLowerInvariant()
}
else {
    "unsigned"
}
if ($SigningMode -notin @("unsigned", "authenticode")) {
    throw "SESSIONS_WINDOWS_SIGNING_MODE must be unsigned or authenticode"
}

$SigningThumbprint = ""
$TimestampUrl = ""
$SignToolPath = ""
if ($SigningMode -eq "authenticode") {
    $SigningThumbprint = ($env:SESSIONS_WINDOWS_CERTIFICATE_THUMBPRINT -replace '[^0-9A-Fa-f]', '').ToUpperInvariant()
    if ($SigningThumbprint -notmatch '^[0-9A-F]{40}$') {
        throw "SESSIONS_WINDOWS_CERTIFICATE_THUMBPRINT must be a 40-character SHA-1 certificate thumbprint"
    }
    $TimestampUrl = $env:SESSIONS_WINDOWS_TIMESTAMP_URL
    if (-not $TimestampUrl) {
        throw "SESSIONS_WINDOWS_TIMESTAMP_URL is required for Authenticode builds"
    }
    $TimestampUri = $null
    if (-not [Uri]::TryCreate($TimestampUrl, [UriKind]::Absolute, [ref]$TimestampUri) -or $TimestampUri.Scheme -notin @("http", "https")) {
        throw "SESSIONS_WINDOWS_TIMESTAMP_URL must be an absolute HTTP(S) RFC 3161 endpoint"
    }

    $SignTool = Get-Command signtool.exe -ErrorAction SilentlyContinue
    if ($SignTool) {
        $SignToolPath = $SignTool.Source
    }
    else {
        $WindowsKits = Join-Path ${env:ProgramFiles(x86)} "Windows Kits\10\bin"
        if (Test-Path $WindowsKits) {
            $SignTool = Get-ChildItem -Path $WindowsKits -Recurse -Filter signtool.exe -File |
                Where-Object { $_.FullName -match '\\x64\\signtool\.exe$' } |
                Sort-Object FullName -Descending |
                Select-Object -First 1
            if ($SignTool) {
                $SignToolPath = $SignTool.FullName
            }
        }
    }
    if (-not $SignToolPath) {
        throw "Windows SDK signtool.exe is required for Authenticode runtime signing"
    }
}

function Invoke-AuthenticodeSignAndVerify {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    & $SignToolPath sign /sha1 $SigningThumbprint /s My /fd SHA256 /tr $TimestampUrl /td SHA256 /v $Path
    if ($LASTEXITCODE -ne 0) {
        throw "signtool failed to sign $Path"
    }
    & $SignToolPath verify /pa /all /v $Path
    if ($LASTEXITCODE -ne 0) {
        throw "signtool rejected the Authenticode signature on $Path"
    }
    $Signature = Get-AuthenticodeSignature -FilePath $Path
    if ($Signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
        throw "Windows rejected the Authenticode signature on $Path with status $($Signature.Status)"
    }
    $ActualThumbprint = ($Signature.SignerCertificate.Thumbprint -replace '[^0-9A-Fa-f]', '').ToUpperInvariant()
    if ($ActualThumbprint -ne $SigningThumbprint) {
        throw "Authenticode signer mismatch on $Path"
    }
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

$AppVersion = (Get-Content -Raw (Join-Path $RepoRoot "package.json") | ConvertFrom-Json).version
$ExactTag = ((& git -C $RepoRoot tag --points-at HEAD --list "v$AppVersion") | Out-String).Trim()
$SourceCommit = ((& git -C $RepoRoot rev-parse --short=12 HEAD 2>$null) | Out-String).Trim()
if ($ExactTag -eq "v$AppVersion") {
    $BuildVersion = $ExactTag
}
else {
    if (-not $SourceCommit) {
        $SourceCommit = "unknown"
    }
    $BuildVersion = "v$AppVersion-dev.g$SourceCommit"
}
$SourceState = (& git -C $RepoRoot status --porcelain=v1 --untracked-files=all -- runtime frontend src-tauri scripts).Trim()
if ($SourceState) {
    $BuildVersion = "$BuildVersion-dirty-$(Get-Date -Format yyyyMMddHHmmss)"
}
if ($BuildVersion -notmatch '^[A-Za-z0-9._-]+$') {
    throw "unsafe runtime build version: $BuildVersion"
}

New-Item -ItemType Directory -Force -Path $EmbeddedAssets | Out-Null
Get-ChildItem -Force $EmbeddedAssets | Remove-Item -Recurse -Force
Copy-Item -Recurse -Force (Join-Path $FrontendDist "*") $EmbeddedAssets

New-Item -ItemType Directory -Force -Path $RuntimeDir | Out-Null
Get-ChildItem -Force $RuntimeDir | Where-Object { $_.Name -ne ".gitkeep" } | Remove-Item -Recurse -Force

$LdFlagsBase = "-s -w -X main.version=$BuildVersion -buildid=sessions/$BuildVersion"
$Binaries = @("sessions", "sessionsd", "sessions-runner")
foreach ($Binary in $Binaries) {
    $Output = Join-Path $RuntimeDir "$Binary.exe"
    $Tags = @()
    if ($Binary -eq "sessionsd") {
        $Tags = @("-tags", "embedui")
    }
    Write-Host "> Sessions runtime: building $Binary.exe ($BuildVersion)"
    Push-Location $GoRoot
    try {
        $env:CGO_ENABLED = "0"
        $env:GOOS = "windows"
        $env:GOARCH = "amd64"
        $env:GOFLAGS = "-buildvcs=false"
        & go build -trimpath @Tags -ldflags "$LdFlagsBase/$Binary" -o $Output "./cmd/$Binary"
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed for $Binary"
        }
        if ($SigningMode -eq "authenticode") {
            Write-Host "> Sessions runtime: Authenticode signing $Binary.exe"
            Invoke-AuthenticodeSignAndVerify -Path $Output
        }
    }
    finally {
        Pop-Location
    }
}

$Hashes = [ordered]@{}
foreach ($Binary in $Binaries) {
    $Name = "$Binary.exe"
    $Hashes[$Name] = Get-Sha256Hex -Path (Join-Path $RuntimeDir $Name)
}
$FingerprintText = ($Hashes.Values -join "`n")
$Sha = [System.Security.Cryptography.SHA256]::Create()
try {
    $FingerprintBytes = $Sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($FingerprintText))
}
finally {
    $Sha.Dispose()
}
$Fingerprint = ([System.BitConverter]::ToString($FingerprintBytes)).Replace("-", "").ToLowerInvariant().Substring(0, 12)
$RuntimeVersion = "$BuildVersion-bin.$Fingerprint"

$Manifest = [ordered]@{
    schemaVersion = 1
    runtimeVersion = $RuntimeVersion
    target = "windows-amd64"
    binaries = $Hashes
}
$Manifest | ConvertTo-Json -Depth 4 | Set-Content -Encoding utf8 (Join-Path $RuntimeDir "runtime-manifest.json")
if ($SigningMode -eq "authenticode") {
    Write-Host "> Sessions runtime: Authenticode-signed Windows binaries ready in $RuntimeDir"
}
else {
    Write-Host "> Sessions runtime: unsigned Windows binaries ready in $RuntimeDir"
}
