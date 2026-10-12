Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

function Assert-DeploymentCleanupTest {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw $Message }
}

$testRoot = Join-Path ([IO.Path]::GetTempPath()) "sub2api-cleanup-deployment-$([guid]::NewGuid())"
$previousProgramData = $env:ProgramData
$previousTestRoot = $env:SUB2API_CLEANUP_TEST_ROOT
$previousTestFailure = $env:SUB2API_CLEANUP_TEST_FAIL
$powerShell = (Get-Process -Id $PID).Path
try {
    $env:ProgramData = Join-Path $testRoot 'ProgramData'
    $env:SUB2API_CLEANUP_TEST_ROOT = $testRoot
    foreach ($directory in @('scripts', 'deploy\windows', 'rate-sync', 'priority-sync', 'monitoring', 'tools\docker-cleanup')) {
        New-Item -ItemType Directory -Path (Join-Path $testRoot $directory) -Force | Out-Null
    }
    Copy-Item -LiteralPath (Join-Path $repoRoot 'deploy-all.ps1') -Destination (Join-Path $testRoot 'deploy-all.ps1')
    @'
function Test-ExtensionAdministrator { return $true }
function Get-Command {
    param($Name, $ErrorAction)
    if ($Name -eq 'docker.exe') { throw 'Docker is isolated in this test.' }
    Microsoft.PowerShell.Core\Get-Command @PSBoundParameters
}
'@ | Set-Content -LiteralPath (Join-Path $testRoot 'scripts\deploy-common.ps1') -Encoding UTF8
    $fakeScripts = [ordered]@{
        'deploy\windows\Bootstrap.ps1' = 'bootstrap'
        'rate-sync\deploy.ps1' = 'rate'
        'priority-sync\deploy.ps1' = 'priority'
        'monitoring\deploy.ps1' = 'monitoring'
        'tools\docker-cleanup\cleanup-docker.ps1' = 'cleanup'
    }
    foreach ($entry in $fakeScripts.GetEnumerator()) {
        @'
param([switch]$NoPause, [switch]$AutoStart, [switch]$Apply, [switch]$PruneBuildCache)
$stage = '__STAGE__'
if ($stage -eq 'cleanup' -and (-not $Apply -or -not $PruneBuildCache)) { exit 8 }
Add-Content -LiteralPath (Join-Path $env:SUB2API_CLEANUP_TEST_ROOT 'calls.log') -Value $stage
if ($env:SUB2API_CLEANUP_TEST_FAIL -eq $stage) { exit 7 }
'@.Replace('__STAGE__', $entry.Value) | Set-Content -LiteralPath (Join-Path $testRoot $entry.Key) -Encoding UTF8
    }
    foreach ($case in @(
        @{ Failure = ''; Expected = 'bootstrap,rate,priority,monitoring,cleanup'; ExitCode = 0 },
        @{ Failure = 'cleanup'; Expected = 'bootstrap,rate,priority,monitoring,cleanup'; ExitCode = 0 },
        @{ Failure = 'priority'; Expected = 'bootstrap,rate,priority'; ExitCode = 1 }
    )) {
        $env:SUB2API_CLEANUP_TEST_FAIL = $case.Failure
        [IO.File]::WriteAllText((Join-Path $testRoot 'calls.log'), '')
        $previousErrorAction = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        $output = & $powerShell -NoLogo -NoProfile -ExecutionPolicy Bypass -File (Join-Path $testRoot 'deploy-all.ps1') -NoPause 2>&1
        $exitCode = $LASTEXITCODE
        $ErrorActionPreference = $previousErrorAction
        Assert-DeploymentCleanupTest ($exitCode -eq $case.ExitCode) "Unexpected deployment exit code for failure=$($case.Failure): $exitCode`n$($output | Out-String)"
        $calls = @(Get-Content -LiteralPath (Join-Path $testRoot 'calls.log'))
        Assert-DeploymentCleanupTest (($calls -join ',') -eq $case.Expected) 'Cleanup must run only after all deployments succeed.'
        if ($case.Failure -eq 'cleanup') {
            Assert-DeploymentCleanupTest (($output | Out-String) -match 'Deployment succeeded, but Docker cleanup failed') 'Cleanup failure must produce a warning.'
            $logs = (Get-ChildItem -LiteralPath (Join-Path $env:ProgramData 'Sub2API\logs') -File | Get-Content) -join "`n"
            Assert-DeploymentCleanupTest ($logs -match 'Docker cleanup warning') 'Cleanup failure must be recorded in the deployment log.'
        }
    }
    $installedScript = Join-Path $env:ProgramData 'Sub2API\maintenance\cleanup-docker.ps1'
    Assert-DeploymentCleanupTest ((Get-FileHash -LiteralPath $installedScript).Hash -eq (Get-FileHash -LiteralPath (Join-Path $testRoot 'tools\docker-cleanup\cleanup-docker.ps1')).Hash) 'The installed cleanup script must match the repository source.'
    Write-Host 'PASS: integrated deployment ordering, skipped cleanup on failure and nonfatal cleanup warnings.'
} finally {
    $env:ProgramData = $previousProgramData
    $env:SUB2API_CLEANUP_TEST_ROOT = $previousTestRoot
    $env:SUB2API_CLEANUP_TEST_FAIL = $previousTestFailure
    $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if (-not $resolvedRoot.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase)) { throw 'Test cleanup path escaped the temporary directory.' }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
