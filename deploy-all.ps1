[CmdletBinding()]
param(
    [switch]$Elevated,
    [switch]$NoPause
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'scripts\deploy-common.ps1')

function Get-DeploymentPowerShell {
    $command = Get-Command powershell.exe -ErrorAction SilentlyContinue
    if ($null -eq $command) {
        $command = Get-Command pwsh.exe -ErrorAction SilentlyContinue
    }
    if ($null -eq $command) {
        throw 'PowerShell was not found.'
    }
    return $command.Source
}

function Format-DeploymentArgument {
    param([Parameter(Mandatory = $true)][string]$Value)

    if ($Value -notmatch '[\s"]') {
        return $Value
    }
    return '"{0}"' -f $Value.Replace('"', '\"')
}

function Invoke-DeploymentChild {
    param(
        [Parameter(Mandatory = $true)][string]$ScriptPath,
        [string[]]$Arguments = @()
    )

    if (-not (Test-Path -LiteralPath $ScriptPath -PathType Leaf)) {
        throw "Deployment script is missing: $ScriptPath"
    }

    $argumentValues = @('-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $ScriptPath) + $Arguments
    $argumentList = @($argumentValues | ForEach-Object { Format-DeploymentArgument -Value ([string]$_) })
    $process = Start-Process `
        -FilePath (Get-DeploymentPowerShell) `
        -ArgumentList $argumentList `
        -WorkingDirectory $PSScriptRoot `
        -NoNewWindow `
        -Wait `
        -PassThru
    if ($process.ExitCode -ne 0) {
        throw "Deployment script failed with exit code $($process.ExitCode): $ScriptPath"
    }
}

if (-not (Test-ExtensionAdministrator)) {
    $arguments = @('-Elevated')
    if ($NoPause) {
        $arguments += '-NoPause'
    }
    $argumentValues = @('-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $PSCommandPath) + $arguments
    $argumentList = @($argumentValues | ForEach-Object { Format-DeploymentArgument -Value ([string]$_) })
    try {
        $process = Start-Process `
            -FilePath (Get-DeploymentPowerShell) `
            -ArgumentList $argumentList `
            -WorkingDirectory $PSScriptRoot `
            -Verb RunAs `
            -Wait `
            -PassThru
        exit $process.ExitCode
    }
    catch {
        Write-Error "Could not start the elevated deployment: $($_.Exception.Message)"
        exit 1
    }
}

$bootstrapPath = Join-Path $PSScriptRoot 'deploy\windows\Bootstrap.ps1'
$rateSyncPath = Join-Path $PSScriptRoot 'rate-sync\deploy.ps1'
$monitoringPath = Join-Path $PSScriptRoot 'monitoring\deploy.ps1'

try {
    Write-Host 'Deploying Sub2API, PostgreSQL, Redis, and extensions...' -ForegroundColor Cyan
    Invoke-DeploymentChild -ScriptPath $bootstrapPath -Arguments @('-NoPause')

    Write-Host 'Deploying rate-sync extensions...' -ForegroundColor Cyan
    Invoke-DeploymentChild -ScriptPath $rateSyncPath

    Write-Host 'Deploying monitoring extension...' -ForegroundColor Cyan
    Invoke-DeploymentChild -ScriptPath $monitoringPath

    Write-Host ''
    Write-Host 'All Sub2API services and extensions were deployed.' -ForegroundColor Green
}
catch {
    Write-Error $_.Exception.Message
    exit 1
}
