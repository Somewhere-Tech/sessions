[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$WorkingDirectory,

    [Parameter(Mandatory = $true)]
    [string]$Package,

    [Parameter(Mandatory = $true)]
    [string]$Run,

    [ValidateRange(30, 900)]
    [int]$TimeoutSeconds = 300
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function ConvertTo-PowerShellLiteral {
    param([AllowEmptyString()][string]$Value)
    return "'" + $Value.Replace("'", "''") + "'"
}

$resolvedWorkingDirectory = (Resolve-Path -LiteralPath $WorkingDirectory).Path
$scratchRoot = Join-Path ([System.IO.Path]::GetTempPath()) (
    "sessions-windows-job-test-" + [guid]::NewGuid().ToString("N")
)
$null = New-Item -ItemType Directory -Path $scratchRoot
$childScript = Join-Path $scratchRoot "run-test.ps1"
$outputPath = Join-Path $scratchRoot "output.txt"
$exitPath = Join-Path $scratchRoot "exit-code.txt"
$expectedSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value

$forwardedEnvironment = [ordered]@{
    "Path" = $env:Path
    "CI" = $env:CI
    "GOTOOLCHAIN" = $env:GOTOOLCHAIN
    "GOFLAGS" = $env:GOFLAGS
    "GOROOT" = $env:GOROOT
    "GOPATH" = $env:GOPATH
}

$childLines = @(
    '$ErrorActionPreference = "Stop"'
    'Set-StrictMode -Version Latest'
    ('$outputPath = ' + (ConvertTo-PowerShellLiteral $outputPath))
    ('$exitPath = ' + (ConvertTo-PowerShellLiteral $exitPath))
    ('$expectedSid = ' + (ConvertTo-PowerShellLiteral $expectedSid))
    '$actualSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value'
    'if ($actualSid -ne $expectedSid) {'
    '    "refusing to run Windows lifetime tests as SID $actualSid; expected signed-in test owner $expectedSid" | Set-Content -LiteralPath $outputPath -Encoding UTF8'
    '    "125" | Set-Content -LiteralPath $exitPath -Encoding ASCII'
    '    exit 125'
    '}'
)
foreach ($entry in $forwardedEnvironment.GetEnumerator()) {
    if ([string]::IsNullOrEmpty($entry.Value)) {
        continue
    }
    $childLines += (
        '$env:' + $entry.Key + ' = ' + (ConvertTo-PowerShellLiteral ([string]$entry.Value))
    )
}
$childLines += @(
    ('Set-Location -LiteralPath ' + (ConvertTo-PowerShellLiteral $resolvedWorkingDirectory))
    'try {'
    (
        '    $testOutput = & go test ' +
        (ConvertTo-PowerShellLiteral $Package) +
        ' -run ' +
        (ConvertTo-PowerShellLiteral $Run) +
        ' -count=1 2>&1'
    )
    '    $testExitCode = $LASTEXITCODE'
    '    $testOutput | Out-File -LiteralPath $outputPath -Encoding UTF8'
    '} catch {'
    '    $_ | Out-String | Set-Content -LiteralPath $outputPath -Encoding UTF8'
    '    $testExitCode = 126'
    '}'
    '$testExitCode | Set-Content -LiteralPath $exitPath -Encoding ASCII'
    'exit $testExitCode'
)
Set-Content -LiteralPath $childScript -Value $childLines -Encoding UTF8

$windowsPowerShell = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe"
$commandLine = (
    '"' + $windowsPowerShell + '"' +
    ' -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' +
    $childScript +
    '"'
)
$createdProcessId = 0

try {
    # GitHub's Windows runner intentionally contains action steps in a Job
    # Object that does not allow process breakaway. Win32_Process.Create is a
    # system service boundary, so this disposable test process is not placed in
    # the action Job. The child script verifies that WMI retained the current
    # signed-in user SID before exercising Sessions' production breakaway path.
    $created = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
        CommandLine = $commandLine
        CurrentDirectory = $resolvedWorkingDirectory
    }
    if ($created.ReturnValue -ne 0 -or $created.ProcessId -le 0) {
        throw "Win32_Process.Create failed: return=$($created.ReturnValue) pid=$($created.ProcessId)"
    }
    $createdProcessId = [int]$created.ProcessId

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        Start-Sleep -Milliseconds 250
        $running = Get-CimInstance -ClassName Win32_Process -Filter (
            "ProcessId = " + $createdProcessId
        ) -ErrorAction SilentlyContinue
    } while ($null -ne $running -and [DateTime]::UtcNow -lt $deadline)

    if ($null -ne $running) {
        $null = Invoke-CimMethod -InputObject $running -MethodName Terminate -Arguments @{
            Reason = 124
        }
        throw "Windows lifetime test exceeded ${TimeoutSeconds}s (PID $createdProcessId)"
    }

    $exitDeadline = [DateTime]::UtcNow.AddSeconds(10)
    while (-not (Test-Path -LiteralPath $exitPath) -and [DateTime]::UtcNow -lt $exitDeadline) {
        Start-Sleep -Milliseconds 100
    }
    if (Test-Path -LiteralPath $outputPath) {
        Get-Content -LiteralPath $outputPath | Write-Host
    }
    if (-not (Test-Path -LiteralPath $exitPath)) {
        throw "Windows lifetime test PID $createdProcessId exited without an exit-code record"
    }
    $exitCode = [int](Get-Content -LiteralPath $exitPath -Raw).Trim()
    if ($exitCode -ne 0) {
        throw "Windows lifetime test failed with exit code $exitCode"
    }
} finally {
    if (Test-Path -LiteralPath $scratchRoot) {
        Remove-Item -LiteralPath $scratchRoot -Recurse -Force
    }
}
