function Assert-Sub2ApiDockerEnvironment {
    foreach ($path in @(
        (Join-Path $env:ProgramFiles 'Docker\Docker\resources\bin'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Docker\Docker\resources\bin')
    )) {
        if ((Test-Path -LiteralPath $path -PathType Container) -and -not (($env:Path -split ';') -contains $path)) {
            $env:Path = "$path;$env:Path"
        }
    }

    if (-not (Test-Sub2ApiCommand 'docker')) {
        throw 'Docker was not found. Install and start Docker Desktop manually, then run this deployment again.'
    }
    if (-not (Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('compose', 'version'))) {
        throw 'Docker Compose is unavailable. Install a current Docker Desktop release, then run this deployment again.'
    }
    if (-not (Test-Sub2ApiNative -FilePath 'docker' -ArgumentList @('info'))) {
        throw 'The Docker engine is not ready. Start Docker Desktop, wait until it is running, then try again.'
    }

    $engineVersion = Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('version', '--format', '{{.Server.Version}}') -CaptureOutput -Quiet
    $composeVersion = Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('compose', 'version', '--short') -CaptureOutput -Quiet
    Write-Sub2ApiMessage -Level Success -Message "Docker Engine $engineVersion; Docker Compose $composeVersion."
}

function Disable-Sub2ApiLegacyAutoStart {
    param([Parameter(Mandatory = $true)]$Context)

    $taskName = 'Sub2API Auto Start'
    $task = Get-ScheduledTask -TaskPath '\' -ErrorAction Stop |
        Where-Object { $_.TaskName -eq $taskName } | Select-Object -First 1
    if ($null -eq $task) {
        return
    }

    $legacyScript = Join-Path $Context.AppRoot 'manager\AutoStart.ps1'
    $fileArgument = '(?i)(?:^|\s)-File\s+(?:"{0}"|{0})(?=\s|$)' -f [regex]::Escape($legacyScript)
    $actions = @($task.Actions)
    if ($actions.Count -ne 1 -or
            @('powershell.exe', 'pwsh.exe') -notcontains (Split-Path -Leaf ([string]$actions[0].Execute)) -or
            [string]$actions[0].Arguments -notmatch $fileArgument) {
        Write-Sub2ApiMessage -Level Warning -Message 'Sub2API Auto Start does not belong to the legacy installer; the task was left unchanged.'
        return
    }

    Disable-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction Stop | Out-Null
    # Disabling a trigger does not stop an instance started after the task query.
    Stop-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction Stop
    Write-Sub2ApiMessage -Level Success -Message 'Disabled the legacy Sub2API Auto Start task. Enable Docker Desktop login startup to restore containers automatically.'
}
