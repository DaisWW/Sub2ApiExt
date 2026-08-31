function Get-Sub2ApiBackupRecords {
    param([Parameter(Mandatory = $true)]$Context)

    if (-not (Test-Path -LiteralPath $Context.BackupRoot -PathType Container)) {
        return @()
    }

    $records = @()
    foreach ($directory in Get-ChildItem -LiteralPath $Context.BackupRoot -Directory | Sort-Object Name -Descending) {
        $manifestPath = Join-Path $directory.FullName 'manifest.json'
        if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
            continue
        }
        try {
            $manifest = Read-Sub2ApiJsonFile -Path $manifestPath
            $records += [pscustomobject]@{
                Id = [string]$manifest.Id
                CreatedAt = [string]$manifest.CreatedAt
                Reason = [string]$manifest.Reason
                Version = [string]$manifest.Version
                BackupImageTag = [string]$manifest.BackupImageTag
                Path = $directory.FullName
            }
        } catch {
            Write-Sub2ApiMessage -Level Warning -Message "Ignoring invalid backup manifest: $manifestPath"
        }
    }
    return $records
}

function New-Sub2ApiDeploymentBackup {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)][string]$Reason,
        [Parameter(Mandatory = $true)][string]$Version
    )

    if (-not (Test-Sub2ApiDeploymentExists -Context $Context)) {
        throw 'Cannot back up a deployment that does not exist.'
    }
    if (-not (Test-Sub2ApiCommand 'tar.exe')) {
        throw 'Windows tar.exe is required for consistent deployment backups.'
    }

    $containerId = Invoke-Sub2ApiCompose -Context $Context -Arguments @('ps', '-a', '-q', 'sub2api') -CaptureOutput -Quiet
    if ([string]::IsNullOrWhiteSpace($containerId)) {
        throw 'The Sub2API container was not found; no image can be backed up.'
    }

    $imageId = Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('inspect', '--format', '{{.Image}}', $containerId.Trim()) -CaptureOutput -Quiet
    $id = "$(Get-Sub2ApiTimestamp)-$((Get-Sub2ApiRandomHex -ByteCount 3))"
    $safeVersion = ($Version -replace '[^0-9A-Za-z._-]', '_')
    $backupName = "$id--$safeVersion"
    $staging = Join-Path $Context.BackupRoot ".creating-$id"
    $finalPath = Join-Path $Context.BackupRoot $backupName
    $imageArchive = Join-Path $staging 'sub2api-image.tar'
    $stateArchive = Join-Path $staging 'state.tar'
    $backupImageTag = "sub2api/local-backup:$id"
    $containersStopped = $false

    New-Item -ItemType Directory -Force -Path $Context.BackupRoot, $staging | Out-Null
    try {
        Write-Sub2ApiMessage -Level Info -Message "Saving the current application image as $backupImageTag"
        Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'tag', $imageId.Trim(), $backupImageTag) -Quiet
        Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'save', '--output', $imageArchive, $backupImageTag) -Quiet

        Write-Sub2ApiMessage -Level Info -Message 'Stopping containers for a consistent data snapshot.'
        Invoke-Sub2ApiCompose -Context $Context -Arguments @('stop') -Quiet
        $containersStopped = $true

        $entries = @('.env', 'docker-compose.yml', 'docker-compose.windows.yml', 'data', 'postgres_data', 'redis_data')
        if (Test-Path -LiteralPath $Context.StateFile) {
            $entries += 'deployment.json'
        }
        $tarArguments = @('-cf', $stateArchive, '-C', $Context.RuntimeRoot) + $entries
        Invoke-Sub2ApiNative -FilePath 'tar.exe' -ArgumentList $tarArguments -Quiet

        $manifest = [ordered]@{
            FormatVersion = 1
            Id = $id
            CreatedAt = (Get-Date).ToString('o')
            Reason = $Reason
            Version = $Version
            BackupImageTag = $backupImageTag
        }
        Write-Sub2ApiJsonFile -Path (Join-Path $staging 'manifest.json') -Value $manifest
        Move-Item -LiteralPath $staging -Destination $finalPath
        Write-Sub2ApiMessage -Level Success -Message "Backup created: $finalPath"

        return [pscustomobject]@{
            Id = $id
            CreatedAt = [string]$manifest.CreatedAt
            Reason = $Reason
            Version = $Version
            BackupImageTag = $backupImageTag
            Path = $finalPath
        }
    } catch {
        if ($containersStopped) {
            try { Invoke-Sub2ApiCompose -Context $Context -Arguments @('up', '-d') -Quiet } catch { }
        }
        if (Test-Path -LiteralPath $staging) {
            Remove-Sub2ApiSafeItem -Path $staging -AllowedRoot $Context.BackupRoot
        }
        throw
    }
}

function Test-Sub2ApiArchiveEntries {
    param([Parameter(Mandatory = $true)][string]$ArchivePath)

    $entries = Invoke-Sub2ApiNative -FilePath 'tar.exe' -ArgumentList @('-tf', $ArchivePath) -CaptureOutput -Quiet
    foreach ($entry in ($entries -split "`r?`n")) {
        if ([string]::IsNullOrWhiteSpace($entry)) {
            continue
        }
        $normalized = $entry.Replace('/', '\')
        if ([System.IO.Path]::IsPathRooted($normalized) -or (($normalized -split '\\') -contains '..')) {
            throw "Unsafe path found in backup archive: $entry"
        }
    }
}

function Restore-Sub2ApiDeploymentBackup {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)]$Backup
    )

    if (-not (Test-Sub2ApiPathWithin -Path $Backup.Path -Root $Context.BackupRoot)) {
        throw "Backup is outside the configured backup directory: $($Backup.Path)"
    }

    $stateArchive = Join-Path $Backup.Path 'state.tar'
    $imageArchive = Join-Path $Backup.Path 'sub2api-image.tar'
    if (-not (Test-Path -LiteralPath $stateArchive -PathType Leaf) -or -not (Test-Path -LiteralPath $imageArchive -PathType Leaf)) {
        throw 'The selected backup is incomplete.'
    }

    Test-Sub2ApiArchiveEntries -ArchivePath $stateArchive
    $operationId = "$(Get-Sub2ApiTimestamp)-$((Get-Sub2ApiRandomHex -ByteCount 3))"
    $staging = Join-Path $Context.AppRoot "restore-staging-$operationId"
    $recovery = Join-Path $Context.AppRoot "restore-recovery-$operationId"
    $failedRestore = Join-Path $Context.AppRoot "failed-restore-$operationId"
    $runtimeMoved = $false

    New-Item -ItemType Directory -Force -Path $staging | Out-Null
    try {
        Invoke-Sub2ApiNative -FilePath 'tar.exe' -ArgumentList @('-xf', $stateArchive, '-C', $staging) -Quiet
        foreach ($required in @('.env', 'docker-compose.yml', 'docker-compose.windows.yml', 'data', 'postgres_data', 'redis_data')) {
            if (-not (Test-Path -LiteralPath (Join-Path $staging $required))) {
                throw "Backup is missing required state: $required"
            }
        }

        Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'load', '--input', $imageArchive) -Quiet
        Set-Sub2ApiEnvValue -Path (Join-Path $staging '.env') -Name 'SUB2API_IMAGE' -Value $Backup.BackupImageTag
        Write-Sub2ApiJsonFile -Path (Join-Path $staging 'deployment.json') -Value ([ordered]@{
            FormatVersion = 1
            Version = $Backup.Version
            Image = $Backup.BackupImageTag
            UpdatedAt = (Get-Date).ToString('o')
        })
        Grant-Sub2ApiRuntimeAccess -Path $staging

        if (Test-Sub2ApiDeploymentExists -Context $Context) {
            Invoke-Sub2ApiCompose -Context $Context -Arguments @('stop') -Quiet
        }

        Move-Item -LiteralPath $Context.RuntimeRoot -Destination $recovery
        $runtimeMoved = $true
        Move-Item -LiteralPath $staging -Destination $Context.RuntimeRoot

        Invoke-Sub2ApiCompose -Context $Context -Arguments @('up', '-d') -Quiet
        Wait-Sub2ApiHealthy
        Remove-Sub2ApiSafeItem -Path $recovery -AllowedRoot $Context.AppRoot
        Write-Sub2ApiMessage -Level Success -Message "Restored Sub2API backup $($Backup.Id) ($($Backup.Version))."
    } catch {
        if ($runtimeMoved) {
            try { Invoke-Sub2ApiCompose -Context $Context -Arguments @('stop') -Quiet } catch { }
            if (Test-Path -LiteralPath $Context.RuntimeRoot) {
                Move-Item -LiteralPath $Context.RuntimeRoot -Destination $failedRestore
            }
            if (Test-Path -LiteralPath $recovery) {
                Move-Item -LiteralPath $recovery -Destination $Context.RuntimeRoot
                try { Invoke-Sub2ApiCompose -Context $Context -Arguments @('up', '-d') -Quiet } catch { }
            }
        } elseif (Test-Path -LiteralPath $Context.RuntimeRoot) {
            try { Invoke-Sub2ApiCompose -Context $Context -Arguments @('up', '-d') -Quiet } catch { }
        }
        throw
    } finally {
        if (Test-Path -LiteralPath $staging) {
            Remove-Sub2ApiSafeItem -Path $staging -AllowedRoot $Context.AppRoot
        }
    }
}

function Select-Sub2ApiBackup {
    param([Parameter(Mandatory = $true)]$Context)

    $backups = @(Get-Sub2ApiBackupRecords -Context $Context)
    if ($backups.Count -eq 0) {
        Write-Sub2ApiMessage -Level Warning -Message 'No deployment backups are available.'
        return $null
    }

    Write-Host ''
    for ($index = 0; $index -lt $backups.Count; $index++) {
        $backup = $backups[$index]
        Write-Host ("[{0}] {1}  version={2}  reason={3}" -f ($index + 1), $backup.CreatedAt, $backup.Version, $backup.Reason)
    }
    Write-Host '[0] Cancel'

    $selection = 0
    if (-not [int]::TryParse((Read-Host 'Select a backup'), [ref]$selection)) {
        return $null
    }
    if ($selection -lt 1 -or $selection -gt $backups.Count) {
        return $null
    }
    return $backups[$selection - 1]
}
