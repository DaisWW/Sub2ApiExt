param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)

$moduleRoot = Join-Path $PSScriptRoot 'modules'
. (Join-Path $moduleRoot 'Common.ps1')
. (Join-Path $moduleRoot 'Docker.ps1')
. (Join-Path $moduleRoot 'Migration.ps1')
. (Join-Path $moduleRoot 'Backup.ps1')
. (Join-Path $moduleRoot 'Deployment.ps1')

$context = New-Sub2ApiContext -ManagerRoot $PSScriptRoot
New-Item -ItemType Directory -Force -Path $context.AppRoot, $context.BackupRoot, $context.LogRoot | Out-Null
$logFile = Join-Path $context.LogRoot 'manager.log'
$transcriptFile = Join-Path $context.LogRoot ("manager-{0}.log" -f (Get-Date -Format 'yyyyMMdd-HHmmss-fff'))
$transcriptStarted = $false

try {
    Start-Transcript -LiteralPath $transcriptFile -Force | Out-Null
    $transcriptStarted = $true
    Write-Host ''
    Write-Host '==========================================' -ForegroundColor DarkCyan
    Write-Host '  Sub2API Windows Deployment Manager' -ForegroundColor Cyan
    Write-Host '==========================================' -ForegroundColor DarkCyan
    Write-Host "Runtime: $($context.RuntimeRoot)"
    Write-Host "Backups: $($context.BackupRoot)"
    Write-Host "Log: $transcriptFile"
    Write-Host ''

    Assert-Sub2ApiDockerEnvironment
    Import-Sub2ApiLegacyDeployment -Context $context

    $deploymentExists = Test-Sub2ApiDeploymentExists -Context $context
    if (-not $deploymentExists) {
        $latestVersion = Get-Sub2ApiLatestVersion -Context $context
        if ((Test-Path -LiteralPath $context.RuntimeRoot -PathType Container) -and $null -ne (Get-ChildItem -LiteralPath $context.RuntimeRoot -Force | Select-Object -First 1)) {
            Write-Sub2ApiMessage -Level Warning -Message 'An incomplete deployment was found. Missing files will be repaired without deleting runtime data.'
        } else {
            Write-Sub2ApiMessage -Level Info -Message "First deployment. Installing Sub2API $latestVersion without an upgrade prompt."
        }
        Initialize-Sub2ApiDeployment -Context $context -Version $latestVersion
        Write-Sub2ApiMessage -Level Success -Message "Sub2API is running at $(Get-Sub2ApiAccessUrl -Context $context)"
        exit 0
    }

    if (-not (Test-Path -LiteralPath $context.StateFile -PathType Leaf)) {
        Write-Sub2ApiMessage -Level Warning -Message 'An incomplete deployment was found. Starting it before version management.'
        Start-Sub2ApiDeployment -Context $context
        $recoveredVersion = Get-Sub2ApiCurrentVersion -Context $context
        $recoveredImage = Get-Sub2ApiEnvValue -Path $context.EnvFile -Name 'SUB2API_IMAGE'
        Write-Sub2ApiDeploymentState -Context $context -Version $recoveredVersion -Image $recoveredImage
    }

    $currentVersion = Get-Sub2ApiCurrentVersion -Context $context
    $targetVersion = $null
    try {
        $targetVersion = Get-Sub2ApiLatestVersion -Context $context
    } catch {
        Write-Sub2ApiMessage -Level Warning -Message 'The latest release could not be queried. Start and rollback operations remain available.'
    }

    Write-Host ''
    Write-Host "Current version: $currentVersion"
    Write-Host "Target version:  $(if ($null -eq $targetVersion) { 'unavailable' } else { $targetVersion })"

    if ($null -ne $targetVersion -and (Test-Sub2ApiUpgradeAvailable -Current $currentVersion -Target $targetVersion)) {
        if (Read-Sub2ApiConfirmation -Message "Upgrade Sub2API from $currentVersion to ${targetVersion}? A full backup will be created first.") {
            Update-Sub2ApiDeployment -Context $context -CurrentVersion $currentVersion -TargetVersion $targetVersion
            exit 0
        }
    } elseif ($null -ne $targetVersion -and (Test-Sub2ApiVersionEqual -Left $currentVersion -Right $targetVersion)) {
        Write-Sub2ApiMessage -Level Success -Message 'Sub2API is already at the latest release.'
    } elseif ($null -ne $targetVersion) {
        Write-Sub2ApiMessage -Level Warning -Message 'The installed Sub2API version is newer than the latest published release; no downgrade will be offered.'
    }

    Write-Host ''
    Write-Host '[1] Start or repair the current deployment'
    Write-Host '[2] Roll back to a backup'
    Write-Host '[0] Exit'
    $choice = Read-Host 'Select an operation'

    switch ($choice) {
        '1' {
            Start-Sub2ApiDeployment -Context $context
            Write-Sub2ApiMessage -Level Success -Message "Sub2API is running at $(Get-Sub2ApiAccessUrl -Context $context)"
        }
        '2' {
            $selectedBackup = Select-Sub2ApiBackup -Context $context
            if ($null -eq $selectedBackup) {
                exit 0
            }
            Write-Sub2ApiMessage -Level Warning -Message 'Rollback restores application, PostgreSQL, Redis, configuration, and data from the selected backup.'
            if (-not (Read-Sub2ApiConfirmation -Message "Restore backup $($selectedBackup.Id) ($($selectedBackup.Version))?")) {
                exit 0
            }

            $currentVersion = Get-Sub2ApiCurrentVersion -Context $context
            New-Sub2ApiDeploymentBackup -Context $context -Reason 'pre-rollback' -Version $currentVersion | Out-Null
            Restore-Sub2ApiDeploymentBackup -Context $context -Backup $selectedBackup
        }
        default { exit 0 }
    }
} catch {
    $message = $_ | Out-String
    Write-Sub2ApiMessage -Level Error -Message $_.Exception.Message
    try {
        "[$(Get-Date -Format o)] $message" | Out-File -LiteralPath $logFile -Append -Encoding utf8
    } catch {
        # The transcript already contains the failure when it is available.
    }
    Write-Sub2ApiMessage -Level Info -Message "Details were written to $transcriptFile (fallback: $logFile)"
    exit 1
} finally {
    if ($transcriptStarted) {
        Stop-Transcript | Out-Null
    }
}

exit 0
