function Get-Sub2ApiLatestVersion {
    param([Parameter(Mandatory = $true)]$Context)

    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $release = Invoke-RestMethod -Uri $Context.ReleaseApiUrl -Headers @{ 'User-Agent' = 'Sub2API-Windows-Deploy' }
    $version = Get-Sub2ApiVersionFromText -Text ([string]$release.tag_name)
    if ([string]::IsNullOrWhiteSpace($version)) {
        throw 'GitHub Releases did not return a valid Sub2API version.'
    }
    return $version
}

function Get-Sub2ApiImageForVersion {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)][string]$Version
    )

    return "$($Context.ImageRepository):$($Version.TrimStart('v'))"
}

function Test-Sub2ApiVersionedImage {
    param(
        [Parameter(Mandatory = $true)][string]$Image,
        [Parameter(Mandatory = $true)][string]$Version
    )

    if (-not (Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'inspect', $Image))) {
        return $false
    }
    $json = Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'inspect', $Image) -CaptureOutput -Quiet
    $inspect = @($json | ConvertFrom-Json)[0]
    $imageVersion = [string]$inspect.Config.Labels.'org.opencontainers.image.version'
    return Test-Sub2ApiVersionEqual -Left $imageVersion -Right $Version
}

function Invoke-Sub2ApiTimedImagePull {
    param(
        [Parameter(Mandatory = $true)][string]$Image,
        [int]$TimeoutSeconds = 180
    )

    $docker = (Get-Command 'docker' -ErrorAction Stop).Source
    $stdoutFile = Join-Path ([System.IO.Path]::GetTempPath()) "sub2api-pull-$([guid]::NewGuid()).out"
    $stderrFile = Join-Path ([System.IO.Path]::GetTempPath()) "sub2api-pull-$([guid]::NewGuid()).err"
    $process = $null

    try {
        Write-Sub2ApiMessage -Level Info -Message "Downloading $Image (timeout: ${TimeoutSeconds}s)."
        $process = Start-Process -FilePath $docker -ArgumentList @('pull', $Image) -WindowStyle Hidden -PassThru `
            -RedirectStandardOutput $stdoutFile -RedirectStandardError $stderrFile
        $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
        $nextNotice = (Get-Date).AddSeconds(15)
        $timedOut = $false

        while (-not $process.HasExited) {
            if ((Get-Date) -ge $deadline) {
                Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
                $process.WaitForExit()
                $timedOut = $true
                break
            }

            if ((Get-Date) -ge $nextNotice) {
                Write-Sub2ApiMessage -Level Info -Message "Still waiting for Docker to download $Image..."
                $nextNotice = (Get-Date).AddSeconds(15)
            }

            Start-Sleep -Seconds 1
            $process.Refresh()
        }

        $output = @()
        if (Test-Path -LiteralPath $stdoutFile) {
            $output += Get-Content -LiteralPath $stdoutFile -ErrorAction SilentlyContinue
        }
        if (Test-Path -LiteralPath $stderrFile) {
            $output += Get-Content -LiteralPath $stderrFile -ErrorAction SilentlyContinue
        }
        $allDetails = ($output -join [Environment]::NewLine).Trim()
        $details = (($output | Select-Object -Last 20) -join [Environment]::NewLine).Trim()
        $imageReady = Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'inspect', $Image)
        $pullReportedSuccess = $allDetails -match '(?im)(?:^|\r?\n)\s*Status:\s+(Downloaded newer image|Image is up to date)'

        if ($imageReady -and $pullReportedSuccess) {
            if ($timedOut) {
                Write-Sub2ApiMessage -Level Warning -Message "Docker pull exceeded $TimeoutSeconds seconds, but $Image is present locally with a successful pull status; continuing."
            } elseif ($process.ExitCode -ne 0) {
                Write-Sub2ApiMessage -Level Warning -Message "Docker pull returned code $($process.ExitCode), but $Image is present locally with a successful pull status; continuing."
            } else {
                Write-Sub2ApiMessage -Level Success -Message "Downloaded $Image."
            }
            return [pscustomobject]@{ Succeeded = $true; Details = $details }
        }

        if ($timedOut) {
            $timeoutDetails = "Docker pull timed out after $TimeoutSeconds seconds."
            if (-not [string]::IsNullOrWhiteSpace($details)) {
                $timeoutDetails = "$timeoutDetails $details"
            }
            Write-Sub2ApiMessage -Level Warning -Message $timeoutDetails
            return [pscustomobject]@{ Succeeded = $false; Details = $timeoutDetails }
        }

        if ($process.ExitCode -ne 0) {
            if ([string]::IsNullOrWhiteSpace($details)) {
                $details = "Docker pull exited with code $($process.ExitCode)."
            }
            $message = "Docker pull failed for ${Image}: $details"
            Write-Sub2ApiMessage -Level Warning -Message $message
            return [pscustomobject]@{ Succeeded = $false; Details = $details }
        }

        Write-Sub2ApiMessage -Level Success -Message "Downloaded $Image."
        return [pscustomobject]@{ Succeeded = $true; Details = $details }
    } catch {
        $details = $_.Exception.Message
        Write-Sub2ApiMessage -Level Warning -Message "Docker pull could not be started for ${Image}: $details"
        return [pscustomobject]@{ Succeeded = $false; Details = $details }
    } finally {
        if ($null -ne $process -and -not $process.HasExited) {
            Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        }
        foreach ($path in @($stdoutFile, $stderrFile)) {
            if (Test-Path -LiteralPath $path) {
                Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
            }
        }
    }
}

function Ensure-Sub2ApiDependencyImages {
    $dependencies = @(
        [pscustomobject]@{
            Name = 'PostgreSQL'
            Official = 'postgres:18-alpine'
            Mirror = 'docker.m.daocloud.io/library/postgres:18-alpine'
        },
        [pscustomobject]@{
            Name = 'Redis'
            Official = 'redis:8-alpine'
            Mirror = 'docker.m.daocloud.io/library/redis:8-alpine'
        }
    )

    foreach ($dependency in $dependencies) {
        if (Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'inspect', $dependency.Official)) {
            Write-Sub2ApiMessage -Level Success -Message "Using local $($dependency.Name) image $($dependency.Official)"
            continue
        }

        $officialPull = Invoke-Sub2ApiTimedImagePull -Image $dependency.Official
        if ($officialPull.Succeeded) {
            continue
        }

        Write-Sub2ApiMessage -Level Warning -Message "Docker Hub was unavailable. Trying the DaoCloud accelerator: $($dependency.Mirror)"
        if (Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'inspect', $dependency.Mirror)) {
            $mirrorPull = [pscustomobject]@{ Succeeded = $true; Details = 'Using the local accelerator image.' }
        } else {
            $mirrorPull = Invoke-Sub2ApiTimedImagePull -Image $dependency.Mirror
        }
        if (-not $mirrorPull.Succeeded) {
            throw "$($dependency.Name) could not be downloaded. Docker Hub: $($officialPull.Details) DaoCloud: $($mirrorPull.Details) The deployment was not changed."
        }

        Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'tag', $dependency.Mirror, $dependency.Official) -Quiet
        Write-Sub2ApiMessage -Level Success -Message "Prepared $($dependency.Official) from the DaoCloud accelerator."
    }
}

function Ensure-Sub2ApiImage {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)][string]$Version
    )

    $image = Get-Sub2ApiImageForVersion -Context $Context -Version $Version
    if (Test-Sub2ApiVersionedImage -Image $image -Version $Version) {
        Write-Sub2ApiMessage -Level Success -Message "Using verified local image $image"
        return $image
    }

    $mirrorImage = "$($Context.ImageMirrorRepository):$($Version.TrimStart('v'))"
    if (Test-Sub2ApiVersionedImage -Image $mirrorImage -Version $Version) {
        Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'tag', $mirrorImage, $image) -Quiet
        Write-Sub2ApiMessage -Level Success -Message "Using verified local accelerator image $mirrorImage"
        return $image
    }

    $officialPull = Invoke-Sub2ApiTimedImagePull -Image $image
    if ($officialPull.Succeeded) {
        return $image
    }

    Write-Sub2ApiMessage -Level Warning -Message "Official registry was unavailable. Trying the GHCR accelerator: $mirrorImage"
    if (Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'inspect', $mirrorImage)) {
        $mirrorPull = [pscustomobject]@{ Succeeded = $true; Details = 'Using the local accelerator image.' }
    } else {
        $mirrorPull = Invoke-Sub2ApiTimedImagePull -Image $mirrorImage
    }
    if (-not $mirrorPull.Succeeded) {
        throw "The Sub2API image could not be downloaded. Official registry: $($officialPull.Details) Accelerator: $($mirrorPull.Details) The current deployment was not changed."
    }
    Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'tag', $mirrorImage, $image) -Quiet
    return $image
}

function Get-Sub2ApiCurrentVersion {
    param([Parameter(Mandatory = $true)]$Context)

    try {
        $output = Invoke-Sub2ApiCompose -Context $Context -Arguments @('exec', '-T', 'sub2api', '/app/sub2api', '--version') -CaptureOutput -Quiet
        $version = Get-Sub2ApiVersionFromText -Text $output
        if (-not [string]::IsNullOrWhiteSpace($version)) {
            return $version
        }
    } catch {
        # Fall back to deployment state when the container is stopped.
    }

    if (Test-Path -LiteralPath $Context.StateFile) {
        $state = Read-Sub2ApiJsonFile -Path $Context.StateFile
        if ($null -ne $state.Version) {
            return [string]$state.Version
        }
    }

    return 'unknown'
}

function Write-Sub2ApiDeploymentState {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)][string]$Version,
        [Parameter(Mandatory = $true)][string]$Image
    )

    Write-Sub2ApiJsonFile -Path $Context.StateFile -Value ([ordered]@{
        FormatVersion = 1
        Version = $Version
        Image = $Image
        UpdatedAt = (Get-Date).ToString('o')
    })
}

function Initialize-Sub2ApiEnvironmentFile {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)][string]$Image
    )

    $temporary = "$($Context.EnvFile).new"
    try {
        Copy-Item -LiteralPath (Join-Path $Context.RuntimeRoot '.env.example') -Destination $temporary -Force
        Set-Sub2ApiEnvValue -Path $temporary -Name 'SERVER_PORT' -Value '18080'
        Set-Sub2ApiEnvValue -Path $temporary -Name 'JWT_SECRET' -Value (Get-Sub2ApiRandomHex)
        Set-Sub2ApiEnvValue -Path $temporary -Name 'TOTP_ENCRYPTION_KEY' -Value (Get-Sub2ApiRandomHex)
        Set-Sub2ApiEnvValue -Path $temporary -Name 'POSTGRES_PASSWORD' -Value (Get-Sub2ApiRandomHex)
        Set-Sub2ApiEnvValue -Path $temporary -Name 'ADMIN_EMAIL' -Value 'admin@sub2api.local'
        Set-Sub2ApiEnvValue -Path $temporary -Name 'ADMIN_PASSWORD' -Value 'admin123456'
        Set-Sub2ApiEnvValue -Path $temporary -Name 'SUB2API_IMAGE' -Value $Image
        Move-Item -LiteralPath $temporary -Destination $Context.EnvFile -Force
    } finally {
        if (Test-Path -LiteralPath $temporary) {
            Remove-Item -LiteralPath $temporary -Force
        }
    }
}

function Initialize-Sub2ApiDeployment {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)][string]$Version
    )

    if (Test-Sub2ApiDeploymentExists -Context $Context) {
        throw 'A Sub2API deployment already exists. Refusing to overwrite it.'
    }

    $image = Get-Sub2ApiImageForVersion -Context $Context -Version $Version
    $deploymentVersion = $Version
    if (Test-Path -LiteralPath $Context.EnvFile -PathType Leaf) {
        $existingImage = Get-Sub2ApiEnvValue -Path $Context.EnvFile -Name 'SUB2API_IMAGE'
        if (-not [string]::IsNullOrWhiteSpace($existingImage)) {
            $image = $existingImage
            $existingVersion = Get-Sub2ApiVersionFromText -Text $existingImage
            if (-not [string]::IsNullOrWhiteSpace($existingVersion)) {
                $deploymentVersion = $existingVersion
                if ($existingImage -match '^weishaw/sub2api:' -and (Test-Sub2ApiVersionEqual -Left $existingVersion -Right $Version)) {
                    $image = Get-Sub2ApiImageForVersion -Context $Context -Version $existingVersion
                }
            }
        }
    }

    New-Item -ItemType Directory -Force -Path $Context.RuntimeRoot, $Context.BackupRoot, $Context.LogRoot | Out-Null
    $persistentDataExists = $false
    foreach ($directory in @('data', 'postgres_data', 'redis_data')) {
        $path = Join-Path $Context.RuntimeRoot $directory
        New-Item -ItemType Directory -Force -Path $path | Out-Null
        if ($null -ne (Get-ChildItem -LiteralPath $path -Force | Select-Object -First 1)) {
            $persistentDataExists = $true
        }
    }

    Write-Sub2ApiMessage -Level Info -Message 'Preparing the Docker Compose configuration.'
    if (-not (Test-Path -LiteralPath $Context.ComposeFile -PathType Leaf)) {
        Invoke-Sub2ApiDownload -Url $Context.ComposeTemplateUrl -Destination $Context.ComposeFile
    }
    if (-not (Test-Path -LiteralPath $Context.ComposeOverrideFile -PathType Leaf)) {
        Copy-Item -LiteralPath (Join-Path $Context.ManagerRoot 'docker-compose.windows.yml') -Destination $Context.ComposeOverrideFile -Force
    }
    if (-not (Test-Path -LiteralPath $Context.EnvFile -PathType Leaf)) {
        if ($persistentDataExists) {
            throw 'Persistent data exists but .env is missing. Restore the original .env or a deployment backup; data was not overwritten.'
        }
        Invoke-Sub2ApiDownload -Url $Context.EnvTemplateUrl -Destination (Join-Path $Context.RuntimeRoot '.env.example')
        Initialize-Sub2ApiEnvironmentFile -Context $Context -Image $image
    } else {
        Set-Sub2ApiEnvValue -Path $Context.EnvFile -Name 'SUB2API_IMAGE' -Value $image
    }
    Grant-Sub2ApiRuntimeAccess -Path $Context.RuntimeRoot

    try {
        $image = Ensure-Sub2ApiImage -Context $Context -Version $deploymentVersion
        Ensure-Sub2ApiDependencyImages
        Set-Sub2ApiEnvValue -Path $Context.EnvFile -Name 'SUB2API_IMAGE' -Value $image
        Invoke-Sub2ApiCompose -Context $Context -Arguments @('up', '-d')
        Wait-Sub2ApiHealthy
        Write-Sub2ApiDeploymentState -Context $Context -Version $deploymentVersion -Image $image
    } catch {
        Write-Sub2ApiMessage -Level Error -Message 'Initial deployment failed. Runtime files were kept for diagnosis.'
        throw
    }
}

function Start-Sub2ApiDeployment {
    param([Parameter(Mandatory = $true)]$Context)

    $image = Get-Sub2ApiEnvValue -Path $Context.EnvFile -Name 'SUB2API_IMAGE'
    if ([string]::IsNullOrWhiteSpace($image)) {
        throw 'SUB2API_IMAGE is missing from the deployment .env file.'
    }

    $version = Get-Sub2ApiVersionFromText -Text $image
    if (-not [string]::IsNullOrWhiteSpace($version)) {
        $image = Ensure-Sub2ApiImage -Context $Context -Version $version
        Set-Sub2ApiEnvValue -Path $Context.EnvFile -Name 'SUB2API_IMAGE' -Value $image
    } elseif (-not (Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'inspect', $image))) {
        throw "The configured Sub2API image is not available locally and its version cannot be determined: $image"
    }

    Ensure-Sub2ApiDependencyImages
    Invoke-Sub2ApiCompose -Context $Context -Arguments @('up', '-d')
    Wait-Sub2ApiHealthy
}

function Update-Sub2ApiDeployment {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)][string]$CurrentVersion,
        [Parameter(Mandatory = $true)][string]$TargetVersion
    )

    if (-not (Test-Sub2ApiUpgradeAvailable -Current $CurrentVersion -Target $TargetVersion)) {
        Write-Sub2ApiMessage -Level Success -Message 'Sub2API is already up to date.'
        return
    }

    $targetImage = Get-Sub2ApiImageForVersion -Context $Context -Version $TargetVersion
    Write-Sub2ApiMessage -Level Info -Message 'Preparing the target image before the backup to minimize downtime.'
    $targetImage = Ensure-Sub2ApiImage -Context $Context -Version $TargetVersion
    $backup = New-Sub2ApiDeploymentBackup -Context $Context -Reason 'upgrade' -Version $CurrentVersion

    try {
        Set-Sub2ApiEnvValue -Path $Context.EnvFile -Name 'SUB2API_IMAGE' -Value $targetImage
        Invoke-Sub2ApiCompose -Context $Context -Arguments @('up', '-d', 'sub2api')
        Wait-Sub2ApiHealthy
        Write-Sub2ApiDeploymentState -Context $Context -Version $TargetVersion -Image $targetImage
        Write-Sub2ApiMessage -Level Success -Message "Sub2API upgraded from $CurrentVersion to $TargetVersion."
    } catch {
        Write-Sub2ApiMessage -Level Error -Message 'Upgrade failed. Restoring the backup created before the upgrade.'
        Restore-Sub2ApiDeploymentBackup -Context $Context -Backup $backup
        throw
    }
}
