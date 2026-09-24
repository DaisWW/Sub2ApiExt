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
        Write-DeploymentTrace "Deployment script is missing: $ScriptPath"
        throw "Deployment script is missing: $ScriptPath"
    }

    Write-DeploymentTrace "Starting child script: $ScriptPath"
    $argumentValues = @('-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $ScriptPath) + $Arguments
    $argumentList = @($argumentValues | ForEach-Object { Format-DeploymentArgument -Value ([string]$_) })
    $process = Start-Process `
        -FilePath (Get-DeploymentPowerShell) `
        -ArgumentList $argumentList `
        -WorkingDirectory $PSScriptRoot `
        -NoNewWindow `
        -Wait `
        -PassThru
    Write-DeploymentTrace "Child script exited with code $($process.ExitCode): $ScriptPath"
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

$script:DeploymentLogPath = $null
try {
    $deploymentLogRoot = Join-Path $env:ProgramData 'Sub2API\logs'
    New-Item -ItemType Directory -Path $deploymentLogRoot -Force | Out-Null
    $script:DeploymentLogPath = Join-Path $deploymentLogRoot (
        'extensions-{0}.log' -f (Get-Date -Format 'yyyyMMdd-HHmmss-fff'))
    "[$(Get-Date -Format o)] Integrated extension deployment started." |
        Set-Content -LiteralPath $script:DeploymentLogPath -Encoding UTF8
}
catch {
    # Logging must not hide the original deployment error.
}

function Write-DeploymentTrace {
    param([Parameter(Mandatory = $true)][string]$Message)

    if ([string]::IsNullOrWhiteSpace($script:DeploymentLogPath)) {
        return
    }
    try {
        "[$(Get-Date -Format o)] $Message" |
            Add-Content -LiteralPath $script:DeploymentLogPath -Encoding UTF8
    }
    catch {
        # Logging must not hide the original deployment error.
    }
}

if (-not [string]::IsNullOrWhiteSpace($script:DeploymentLogPath)) {
    Write-Host "Deployment log: $script:DeploymentLogPath" -ForegroundColor DarkGray
    Write-DeploymentTrace "PowerShell: edition=$($PSVersionTable.PSEdition) version=$($PSVersionTable.PSVersion)"
    try {
        $dockerCommand = Get-Command docker.exe -ErrorAction Stop
        $composeVersion = (& $dockerCommand.Source compose version 2>&1 | Out-String).Trim()
        Write-DeploymentTrace "Docker Compose: $composeVersion"
    }
    catch {
        Write-DeploymentTrace "Docker Compose version check failed: $($_.Exception.Message)"
    }
}

$bootstrapPath = Join-Path $PSScriptRoot 'deploy\windows\Bootstrap.ps1'
$rateSyncPath = Join-Path $PSScriptRoot 'rate-sync\deploy.ps1'
$prioritySyncPath = Join-Path $PSScriptRoot 'priority-sync\deploy.ps1'
$monitoringPath = Join-Path $PSScriptRoot 'monitoring\deploy.ps1'

try {
    Write-Host 'Deploying Sub2API, PostgreSQL, Redis, and extensions...' -ForegroundColor Cyan
    Write-DeploymentTrace 'Starting main Sub2API deployment.'
    Invoke-DeploymentChild -ScriptPath $bootstrapPath -Arguments @('-NoPause', '-AutoStart')

    Write-Host 'Deploying rate-sync extensions...' -ForegroundColor Cyan
    Write-DeploymentTrace 'Starting rate-sync deployment.'
    Invoke-DeploymentChild -ScriptPath $rateSyncPath

    Write-Host 'Deploying priority-sync extension...' -ForegroundColor Cyan
    Write-DeploymentTrace 'Starting priority-sync deployment.'
    Invoke-DeploymentChild -ScriptPath $prioritySyncPath

    Write-Host 'Deploying monitoring extension...' -ForegroundColor Cyan
    Write-DeploymentTrace 'Starting monitoring deployment.'
    Invoke-DeploymentChild -ScriptPath $monitoringPath

    Write-Host ''
    Write-Host 'All Sub2API services and extensions were deployed.' -ForegroundColor Green
    Write-DeploymentTrace 'Integrated extension deployment completed successfully.'
}
catch {
    Write-DeploymentTrace "Integrated extension deployment failed: $($_.Exception.Message)"
    Write-Error $_.Exception.Message
    if (-not [string]::IsNullOrWhiteSpace($script:DeploymentLogPath)) {
        Write-Error "Deployment log: $script:DeploymentLogPath"
    }
    exit 1
}
