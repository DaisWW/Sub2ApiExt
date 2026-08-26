Set-StrictMode -Version Latest

function Test-ExtensionAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Test-ExtensionRuntimeWritable {
    $runtimeBase = Join-Path $env:ProgramData 'Sub2API\extensions'
    $probePath = $null
    try {
        New-Item -ItemType Directory -Path $runtimeBase -Force | Out-Null
        $probePath = Join-Path $runtimeBase ('.write-probe-{0}.tmp' -f [Guid]::NewGuid().ToString('N'))
        $stream = [IO.File]::Open($probePath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
        $stream.Dispose()
        [IO.File]::Delete($probePath)
        return $true
    }
    catch {
        if ($null -ne $probePath -and [IO.File]::Exists($probePath)) {
            [IO.File]::Delete($probePath)
        }
        return $false
    }
}

function Invoke-ExtensionElevated {
    param([Parameter(Mandatory = $true)][string]$ScriptPath)

    if ((Test-ExtensionAdministrator) -or (Test-ExtensionRuntimeWritable)) {
        return $null
    }

    $powerShell = Get-Command pwsh.exe -ErrorAction SilentlyContinue
    if ($null -eq $powerShell) {
        $powerShell = Get-Command powershell.exe -ErrorAction Stop
    }
    $escapedPath = $ScriptPath.Replace('"', '""')
    $arguments = '-NoLogo -NoProfile -ExecutionPolicy Bypass -File "{0}"' -f $escapedPath
    $process = Start-Process -FilePath $powerShell.Source -ArgumentList $arguments -Verb RunAs -Wait -PassThru
    return $process.ExitCode
}

function Invoke-ExtensionDocker {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [switch]$Capture
    )

    if ($Capture) {
        $output = & docker @Arguments 2>&1
        if ($LASTEXITCODE -ne 0) {
            $details = ($output | Out-String).Trim()
            throw "Docker command failed: docker $($Arguments -join ' ')`n$details"
        }
        return $output
    }

    & docker @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Docker command failed: docker $($Arguments -join ' ')"
    }
}

function Assert-ExtensionDocker {
    if ($null -eq (Get-Command docker.exe -ErrorAction SilentlyContinue)) {
        throw 'Docker was not found. Install and start Docker Desktop first.'
    }
    Invoke-ExtensionDocker -Arguments @('info') -Capture | Out-Null
    Invoke-ExtensionDocker -Arguments @('compose', 'version') -Capture | Out-Null
}

function Get-ExtensionContainerInspect {
    param([Parameter(Mandatory = $true)][string]$Name)

    $json = (Invoke-ExtensionDocker -Arguments @('inspect', $Name) -Capture | Out-String)
    $items = @($json | ConvertFrom-Json)
    if ($items.Count -ne 1) {
        throw "Expected one Docker container named $Name."
    }
    return $items[0]
}

function Get-ExtensionContainerEnvironment {
    param([Parameter(Mandatory = $true)]$Inspect)

    $values = @{}
    foreach ($entry in $Inspect.Config.Env) {
        $pair = $entry -split '=', 2
        $values[$pair[0]] = if ($pair.Count -eq 2) { $pair[1] } else { '' }
    }
    return $values
}

function Get-ExtensionHostBindSource {
    param(
        [Parameter(Mandatory = $true)][string]$Container,
        [Parameter(Mandatory = $true)][string]$Destination,
        [string]$RelativePath = ''
    )

    if (-not (Test-ExtensionContainerExists -Name $Container)) {
        return $null
    }
    $inspect = Get-ExtensionContainerInspect -Name $Container
    $labels = $inspect.Config.Labels
    if ($null -eq $labels) {
        return $null
    }
    foreach ($property in $labels.PSObject.Properties) {
        if ($property.Name -notlike 'desktop.docker.io/binds/*/Target' -or $property.Value -ne $Destination) {
            continue
        }
        $prefix = $property.Name.Substring(0, $property.Name.LastIndexOf('/'))
        $sourceProperty = $labels.PSObject.Properties["$prefix/Source"]
        if ($null -ne $sourceProperty -and (Test-Path -LiteralPath $sourceProperty.Value -PathType Leaf)) {
            return [string]$sourceProperty.Value
        }
    }
    if (-not [string]::IsNullOrWhiteSpace($RelativePath)) {
        $workingDirectory = [string]$labels.'com.docker.compose.project.working_dir'
        if (-not [string]::IsNullOrWhiteSpace($workingDirectory)) {
            $candidate = Join-Path $workingDirectory $RelativePath
            if (Test-Path -LiteralPath $candidate -PathType Leaf) {
                return $candidate
            }
        }
    }
    return $null
}

function Get-Sub2ApiDockerContext {
    $application = Get-ExtensionContainerInspect -Name 'sub2api'
    $postgres = Get-ExtensionContainerInspect -Name 'sub2api-postgres'
    if (-not $application.State.Running -or -not $postgres.State.Running) {
        throw 'The sub2api and sub2api-postgres containers must be running.'
    }

    $postgresEnvironment = Get-ExtensionContainerEnvironment -Inspect $postgres
    $postgresUser = if ($postgresEnvironment['POSTGRES_USER']) { $postgresEnvironment['POSTGRES_USER'] } else { 'sub2api' }
    $postgresDatabase = if ($postgresEnvironment['POSTGRES_DB']) { $postgresEnvironment['POSTGRES_DB'] } else { 'sub2api' }
    $postgresPassword = [string]$postgresEnvironment['POSTGRES_PASSWORD']
    if ([string]::IsNullOrWhiteSpace($postgresPassword)) {
        throw 'POSTGRES_PASSWORD is missing from sub2api-postgres.'
    }

    $portOutput = Invoke-ExtensionDocker -Arguments @(
        'exec', 'sub2api-postgres', 'psql',
        '-U', $postgresUser,
        '-d', $postgresDatabase,
        '-At', '-c', 'SHOW port;'
    ) -Capture
    $postgresPort = ($portOutput -join '').Trim()
    if ($postgresPort -notmatch '^\d+$') {
        throw 'Could not determine the PostgreSQL port.'
    }

    $networkNames = @($application.NetworkSettings.Networks.PSObject.Properties.Name)
    if ($networkNames.Count -eq 0) {
        throw 'The sub2api container is not attached to a Docker network.'
    }
    $network = [string]($networkNames | Where-Object { $_ -like '*sub2api-network*' } | Select-Object -First 1)
    if ([string]::IsNullOrWhiteSpace($network)) {
        $network = [string]$networkNames[0]
    }

    return [pscustomobject]@{
        Network = $network
        PostgresHost = ([string]$postgres.Name).TrimStart('/')
        PostgresPort = $postgresPort
        PostgresUser = $postgresUser
        PostgresPassword = $postgresPassword
        PostgresDatabase = $postgresDatabase
    }
}

function Write-ExtensionEnvFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][System.Collections.IDictionary]$Values
    )

    $lines = [System.Collections.Generic.List[string]]::new()
    foreach ($entry in $Values.GetEnumerator()) {
        $name = [string]$entry.Key
        $value = [string]$entry.Value
        if ($name -notmatch '^[A-Z][A-Z0-9_]*$') {
            throw "Invalid environment variable name: $name"
        }
        if ($value.Contains("`r") -or $value.Contains("`n")) {
            throw "Environment variable $name contains a newline."
        }
        $lines.Add("$name=$value")
    }
    [IO.File]::WriteAllLines($Path, $lines, [Text.UTF8Encoding]::new($false))
}

function Read-ExtensionEnvFile {
    param([Parameter(Mandatory = $true)][string]$Path)

    $values = @{}
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $values
    }
    foreach ($line in Get-Content -LiteralPath $Path) {
        $trimmed = $line.Trim()
        if ($trimmed -eq '' -or $trimmed.StartsWith('#')) {
            continue
        }
        $pair = $line -split '=', 2
        if ($pair.Count -eq 2) {
            $values[$pair[0].Trim()] = $pair[1]
        }
    }
    return $values
}

function Grant-ExtensionRuntimeAccess {
    param([Parameter(Mandatory = $true)][string]$Path)

    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $currentUser = "*$($identity.User.Value)"
    & icacls.exe $Path '/inheritance:r' '/grant:r' `
        '*S-1-5-18:(OI)(CI)F' `
        '*S-1-5-32-544:(OI)(CI)F' `
        "${currentUser}:(OI)(CI)M" '/Q' | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "Could not restrict access to $Path."
    }

    $children = @(Get-ChildItem -LiteralPath $Path -Force)
    if ($children.Count -gt 0) {
        & icacls.exe (Join-Path $Path '*') '/inheritance:e' '/T' '/C' '/Q' | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "Could not restore inherited access below $Path."
        }
    }
}

function Get-ExtensionRuntimeRoot {
    param([Parameter(Mandatory = $true)][string]$Service)

    return Join-Path $env:ProgramData "Sub2API\extensions\$Service"
}

function Build-ExtensionImage {
    param(
        [Parameter(Mandatory = $true)][string]$Context,
        [Parameter(Mandatory = $true)][string]$Dockerfile,
        [Parameter(Mandatory = $true)][string]$Image
    )

    Write-Host "Building $Image..." -ForegroundColor Cyan
    Invoke-ExtensionDocker -Arguments @('build', '--file', $Dockerfile, '--tag', $Image, $Context)
}

function Start-ExtensionCompose {
    param([Parameter(Mandatory = $true)][string]$RuntimeRoot)

    Push-Location $RuntimeRoot
    try {
        Invoke-ExtensionDocker -Arguments @('compose', 'up', '-d', '--remove-orphans')
    }
    finally {
        Pop-Location
    }
}

function Wait-ExtensionContainer {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [int]$TimeoutSeconds = 90
    )

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        try {
            $inspect = Get-ExtensionContainerInspect -Name $Name
            if ($inspect.State.Running) {
                if ($null -eq $inspect.State.Health -or $inspect.State.Health.Status -eq 'healthy') {
                    return
                }
                if ($inspect.State.Health.Status -eq 'unhealthy') {
                    throw "$Name is unhealthy."
                }
            } elseif ($inspect.State.Status -eq 'exited') {
                throw "$Name exited with code $($inspect.State.ExitCode)."
            }
        } catch {
            if ((Get-Date) -ge $deadline) {
                throw
            }
        }
        Start-Sleep -Seconds 2
    } while ((Get-Date) -lt $deadline)

    throw "$Name did not become ready within $TimeoutSeconds seconds."
}

function Test-ExtensionContainerExists {
    param([Parameter(Mandatory = $true)][string]$Name)

    & docker container inspect $Name *> $null
    return $LASTEXITCODE -eq 0
}

function Remove-ExtensionContainer {
    param([Parameter(Mandatory = $true)][string]$Name)

    if (Test-ExtensionContainerExists -Name $Name) {
        Invoke-ExtensionDocker -Arguments @('container', 'rm', '--force', $Name)
    }
}
