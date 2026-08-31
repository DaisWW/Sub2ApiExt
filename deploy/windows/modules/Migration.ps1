function Get-Sub2ApiContainerInspect {
    param([Parameter(Mandatory = $true)][string]$Name)

    if (-not (Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('container', 'inspect', $Name))) {
        return $null
    }

    $json = Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('container', 'inspect', $Name) -CaptureOutput -Quiet
    return @($json | ConvertFrom-Json)[0]
}

function Get-Sub2ApiContainerLabel {
    param(
        [Parameter(Mandatory = $true)]$Inspect,
        [Parameter(Mandatory = $true)][string]$Name
    )

    $property = $Inspect.Config.Labels.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }
    return [string]$property.Value
}

function Assert-Sub2ApiLegacyMount {
    param(
        [Parameter(Mandatory = $true)]$Inspect,
        [Parameter(Mandatory = $true)][string]$Destination,
        [Parameter(Mandatory = $true)][string]$ExpectedSource
    )

    $mount = @($Inspect.Mounts | Where-Object { $_.Destination -eq $Destination }) | Select-Object -First 1
    if ($null -eq $mount -or $mount.Type -ne 'bind') {
        throw "Existing container $($Inspect.Name) does not use the supported bind mount at $Destination."
    }

    $actual = [System.IO.Path]::GetFullPath([string]$mount.Source).TrimEnd('\')
    $expected = [System.IO.Path]::GetFullPath($ExpectedSource).TrimEnd('\')
    if (-not $actual.Equals($expected, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Existing container $($Inspect.Name) stores data outside the detected deployment directory."
    }
}

function Test-Sub2ApiRuntimeDataEmpty {
    param([Parameter(Mandatory = $true)]$Context)

    if (Test-Path -LiteralPath $Context.StateFile -PathType Leaf) {
        return $false
    }
    foreach ($name in @('data', 'postgres_data', 'redis_data')) {
        $path = Join-Path $Context.RuntimeRoot $name
        if ((Test-Path -LiteralPath $path) -and $null -ne (Get-ChildItem -LiteralPath $path -Force | Select-Object -First 1)) {
            return $false
        }
    }
    return $true
}

function Import-Sub2ApiLegacyDeployment {
    param([Parameter(Mandatory = $true)]$Context)

    $application = Get-Sub2ApiContainerInspect -Name 'sub2api'
    if ($null -eq $application) {
        return
    }

    $projectName = Get-Sub2ApiContainerLabel -Inspect $application -Name 'com.docker.compose.project'
    $sourceRoot = Get-Sub2ApiContainerLabel -Inspect $application -Name 'com.docker.compose.project.working_dir'
    $sourceCompose = Get-Sub2ApiContainerLabel -Inspect $application -Name 'com.docker.compose.project.config_files'

    if ($projectName -eq $Context.ProjectName -and -not [string]::IsNullOrWhiteSpace($sourceRoot)) {
        try {
            $managedRoot = [System.IO.Path]::GetFullPath($Context.RuntimeRoot).TrimEnd('\')
            $detectedRoot = [System.IO.Path]::GetFullPath($sourceRoot).TrimEnd('\')
            if ($managedRoot.Equals($detectedRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
                return
            }
        }
        catch {
            # Let the normal validation below report an unsupported deployment.
        }
    }

    if ([string]::IsNullOrWhiteSpace($projectName) -or [string]::IsNullOrWhiteSpace($sourceRoot) -or
        [string]::IsNullOrWhiteSpace($sourceCompose) -or $sourceCompose.Contains(',')) {
        throw 'An unmanaged Sub2API container exists, but its Docker Compose deployment cannot be imported automatically.'
    }

    $sourceRoot = [System.IO.Path]::GetFullPath($sourceRoot)
    $sourceCompose = [System.IO.Path]::GetFullPath($sourceCompose)
    $sourceEnv = Join-Path $sourceRoot '.env'
    foreach ($path in @($sourceCompose, $sourceEnv)) {
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            throw "Existing Sub2API deployment file is missing: $path"
        }
    }
    foreach ($name in @('data', 'postgres_data', 'redis_data')) {
        if (-not (Test-Path -LiteralPath (Join-Path $sourceRoot $name) -PathType Container)) {
            throw "Existing Sub2API data directory is missing: $name"
        }
    }
    if (-not (Test-Sub2ApiRuntimeDataEmpty -Context $Context)) {
        throw 'Both managed and unmanaged Sub2API data were found. Automatic import stopped without changing either deployment.'
    }

    $postgres = Get-Sub2ApiContainerInspect -Name 'sub2api-postgres'
    $redis = Get-Sub2ApiContainerInspect -Name 'sub2api-redis'
    if ($null -eq $postgres -or $null -eq $redis) {
        throw 'The existing Sub2API PostgreSQL or Redis container was not found.'
    }
    Assert-Sub2ApiLegacyMount -Inspect $application -Destination '/app/data' -ExpectedSource (Join-Path $sourceRoot 'data')
    Assert-Sub2ApiLegacyMount -Inspect $postgres -Destination '/var/lib/postgresql/data' -ExpectedSource (Join-Path $sourceRoot 'postgres_data')
    Assert-Sub2ApiLegacyMount -Inspect $redis -Destination '/data' -ExpectedSource (Join-Path $sourceRoot 'redis_data')

    $versionOutput = Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('exec', 'sub2api', '/app/sub2api', '--version') -CaptureOutput -Quiet
    $version = Get-Sub2ApiVersionFromText -Text $versionOutput
    if ([string]::IsNullOrWhiteSpace($version)) {
        throw 'The existing Sub2API version could not be detected.'
    }

    $operationId = "$(Get-Sub2ApiTimestamp)-$((Get-Sub2ApiRandomHex -ByteCount 3))"
    $staging = Join-Path $Context.AppRoot "runtime.import-$operationId"
    $recovery = Join-Path $Context.AppRoot "runtime.incomplete-$operationId"
    $failed = Join-Path $Context.AppRoot "runtime.failed-$operationId"
    $backupImageTag = "sub2api/local-backup:import-$operationId"
    $legacyArguments = @(
        'compose', '--project-name', $projectName,
        '--project-directory', $sourceRoot,
        '--env-file', $sourceEnv,
        '-f', $sourceCompose
    )
    $legacyStopped = $false
    $runtimeSwapped = $false

    Write-Sub2ApiMessage -Level Warning -Message "Existing Sub2API $version detected at $sourceRoot"
    Write-Sub2ApiMessage -Level Info -Message 'Importing it into the deployment manager. The original data directory will be kept.'

    try {
        New-Item -ItemType Directory -Force -Path $staging | Out-Null
        Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('image', 'tag', [string]$application.Image, $backupImageTag) -Quiet
        Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList ($legacyArguments + @('stop')) -Quiet
        $legacyStopped = $true

        Copy-Item -LiteralPath $sourceEnv -Destination (Join-Path $staging '.env') -Force
        foreach ($name in @('data', 'postgres_data', 'redis_data')) {
            Copy-Item -LiteralPath (Join-Path $sourceRoot $name) -Destination $staging -Recurse -Force
        }
        Copy-Item -LiteralPath $sourceCompose -Destination (Join-Path $staging 'docker-compose.yml') -Force
        Copy-Item -LiteralPath (Join-Path $Context.ManagerRoot 'docker-compose.windows.yml') -Destination (Join-Path $staging 'docker-compose.windows.yml') -Force
        Set-Sub2ApiEnvValue -Path (Join-Path $staging '.env') -Name 'SUB2API_IMAGE' -Value $backupImageTag
        Grant-Sub2ApiRuntimeAccess -Path $staging

        Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList ($legacyArguments + @('down')) -Quiet
        if (Test-Path -LiteralPath $Context.RuntimeRoot) {
            Move-Item -LiteralPath $Context.RuntimeRoot -Destination $recovery
        }
        Move-Item -LiteralPath $staging -Destination $Context.RuntimeRoot
        $runtimeSwapped = $true

        Invoke-Sub2ApiCompose -Context $Context -Arguments @('up', '-d') -Quiet
        Wait-Sub2ApiHealthy
        Write-Sub2ApiDeploymentState -Context $Context -Version $version -Image $backupImageTag
        if (Test-Path -LiteralPath $recovery) {
            Remove-Sub2ApiSafeItem -Path $recovery -AllowedRoot $Context.AppRoot
        }
        Write-Sub2ApiMessage -Level Success -Message "Existing Sub2API $version was imported successfully."
    } catch {
        if ($runtimeSwapped) {
            try { Invoke-Sub2ApiCompose -Context $Context -Arguments @('down') -Quiet } catch { }
            if (Test-Path -LiteralPath $Context.RuntimeRoot) {
                Move-Item -LiteralPath $Context.RuntimeRoot -Destination $failed
            }
            if (Test-Path -LiteralPath $recovery) {
                Move-Item -LiteralPath $recovery -Destination $Context.RuntimeRoot
            }
        }
        if ($legacyStopped) {
            try {
                Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList ($legacyArguments + @('up', '-d')) -Quiet
                Wait-Sub2ApiHealthy
            } catch { }
        }
        throw
    } finally {
        if (Test-Path -LiteralPath $staging) {
            Remove-Sub2ApiSafeItem -Path $staging -AllowedRoot $Context.AppRoot
        }
    }
}
