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
