Set-StrictMode -Version Latest

function New-Sub2ApiContext {
    param([string]$ManagerRoot = (Join-Path $env:ProgramData 'Sub2API\manager'))

    $appRoot = Join-Path $env:ProgramData 'Sub2API'
    $runtimeRoot = Join-Path $appRoot 'runtime'

    return [pscustomobject]@{
        AppRoot = $appRoot
        BackupRoot = Join-Path $appRoot 'backups'
        LogRoot = Join-Path $appRoot 'logs'
        ManagerRoot = $ManagerRoot
        RuntimeRoot = $runtimeRoot
        ComposeFile = Join-Path $runtimeRoot 'docker-compose.yml'
        ComposeOverrideFile = Join-Path $runtimeRoot 'docker-compose.windows.yml'
        EnvFile = Join-Path $runtimeRoot '.env'
        StateFile = Join-Path $runtimeRoot 'deployment.json'
        ProjectName = 'sub2api'
        ImageRepository = 'ghcr.io/wei-shaw/sub2api'
        ImageMirrorRepository = 'ghcr.m.daocloud.io/wei-shaw/sub2api'
        ReleaseApiUrl = 'https://api.github.com/repos/Wei-Shaw/sub2api/releases/latest'
        ComposeTemplateUrl = 'https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/docker-compose.local.yml'
        EnvTemplateUrl = 'https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/.env.example'
    }
}

function Write-Sub2ApiMessage {
    param(
        [Parameter(Mandatory = $true)][ValidateSet('Info', 'Success', 'Warning', 'Error')][string]$Level,
        [Parameter(Mandatory = $true)][string]$Message
    )

    $color = switch ($Level) {
        'Success' { 'Green' }
        'Warning' { 'Yellow' }
        'Error' { 'Red' }
        default { 'Cyan' }
    }
    Write-Host "[$Level] $Message" -ForegroundColor $color
}

function Read-Sub2ApiConfirmation {
    param([Parameter(Mandatory = $true)][string]$Message)

    if ([Console]::IsInputRedirected) {
        $answer = [Console]::In.ReadLine()
        Write-Host "$Message [y/N]: $answer"
    } else {
        $answer = Read-Host "$Message [y/N]"
    }
    if ([string]::IsNullOrWhiteSpace($answer)) {
        return $false
    }
    return @('y', 'yes') -contains $answer.Trim().ToLowerInvariant()
}

function Invoke-Sub2ApiNative {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [string[]]$ArgumentList = @(),
        [int[]]$AllowedExitCodes = @(0),
        [switch]$CaptureOutput,
        [switch]$Quiet
    )

    $previousErrorAction = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        if (-not $CaptureOutput -and -not $Quiet) {
            & $FilePath @ArgumentList
            $exitCode = $LASTEXITCODE
            if ($AllowedExitCodes -notcontains $exitCode) {
                throw "Command failed ($exitCode): $FilePath $($ArgumentList -join ' ')"
            }
            return
        }

        $output = & $FilePath @ArgumentList 2>&1
        $exitCode = $LASTEXITCODE
        if ($AllowedExitCodes -notcontains $exitCode) {
            $details = ($output | Out-String).Trim()
            throw "Command failed ($exitCode): $FilePath $($ArgumentList -join ' ')`n$details"
        }
        if ($CaptureOutput) {
            return ($output | Out-String).Trim()
        }
    } finally {
        $ErrorActionPreference = $previousErrorAction
    }
}

function Test-Sub2ApiNative {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [string[]]$ArgumentList = @()
    )

    $previousErrorAction = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        & $FilePath @ArgumentList *> $null
        return $LASTEXITCODE -eq 0
    } finally {
        $ErrorActionPreference = $previousErrorAction
    }
}

function Test-Sub2ApiCommand {
    param([Parameter(Mandatory = $true)][string]$Name)
    return $null -ne (Get-Command $Name -ErrorAction SilentlyContinue)
}

function Get-Sub2ApiComposeArguments {
    param([Parameter(Mandatory = $true)]$Context)

    return @(
        'compose',
        '--project-name', $Context.ProjectName,
        '--project-directory', $Context.RuntimeRoot,
        '--env-file', $Context.EnvFile,
        '-f', $Context.ComposeFile,
        '-f', $Context.ComposeOverrideFile
    )
}

function Invoke-Sub2ApiCompose {
    param(
        [Parameter(Mandatory = $true)]$Context,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [switch]$CaptureOutput,
        [switch]$Quiet
    )

    $allArguments = @(Get-Sub2ApiComposeArguments -Context $Context) + $Arguments
    return Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList $allArguments -CaptureOutput:$CaptureOutput -Quiet:$Quiet
}

function Test-Sub2ApiDeploymentExists {
    param([Parameter(Mandatory = $true)]$Context)

    return (Test-Path -LiteralPath $Context.ComposeFile -PathType Leaf) -and
        (Test-Path -LiteralPath $Context.ComposeOverrideFile -PathType Leaf) -and
        (Test-Path -LiteralPath $Context.EnvFile -PathType Leaf)
}

function Get-Sub2ApiEnvValue {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Name
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $null
    }

    $match = Get-Content -LiteralPath $Path -Encoding UTF8 | Where-Object { $_ -match "^$([regex]::Escape($Name))=" } | Select-Object -First 1
    if ($null -eq $match) {
        return $null
    }
    return $match.Substring($Name.Length + 1)
}

function Get-Sub2ApiAccessUrl {
    param([Parameter(Mandatory = $true)]$Context)

    $port = Get-Sub2ApiEnvValue -Path $Context.EnvFile -Name 'SERVER_PORT'
    if ([string]::IsNullOrWhiteSpace($port)) {
        $port = '18080'
    }
    return "http://localhost:$port"
}

function Set-Sub2ApiEnvValue {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Value
    )

    $lines = @()
    if (Test-Path -LiteralPath $Path) {
        $lines = @(Get-Content -LiteralPath $Path -Encoding UTF8)
    }
    $expression = "^$([regex]::Escape($Name))="
    $updated = $false
    for ($index = 0; $index -lt $lines.Count; $index++) {
        if ($lines[$index] -match $expression) {
            $lines[$index] = "$Name=$Value"
            $updated = $true
            break
        }
    }
    if (-not $updated) {
        $lines += "$Name=$Value"
    }

    $encoding = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllLines($Path, [string[]]$lines, $encoding)
}

function Get-Sub2ApiRandomHex {
    param([int]$ByteCount = 32)

    $bytes = New-Object byte[] $ByteCount
    $generator = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $generator.GetBytes($bytes)
    } finally {
        $generator.Dispose()
    }
    return ([BitConverter]::ToString($bytes)).Replace('-', '').ToLowerInvariant()
}

function Get-Sub2ApiTimestamp {
    return Get-Date -Format 'yyyyMMdd-HHmmss'
}

function Test-Sub2ApiPathWithin {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Root
    )

    $fullPath = [System.IO.Path]::GetFullPath($Path)
    $fullRoot = [System.IO.Path]::GetFullPath($Root).TrimEnd('\') + '\'
    return $fullPath.StartsWith($fullRoot, [System.StringComparison]::OrdinalIgnoreCase)
}

function Remove-Sub2ApiSafeItem {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$AllowedRoot
    )

    if (-not (Test-Sub2ApiPathWithin -Path $Path -Root $AllowedRoot)) {
        throw "Refusing to remove a path outside ${AllowedRoot}: $Path"
    }
    if (Test-Path -LiteralPath $Path) {
        Remove-Item -LiteralPath $Path -Recurse -Force
    }
}

function Grant-Sub2ApiRuntimeAccess {
    param([Parameter(Mandatory = $true)][string]$Path)

    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $trustee = "*$($identity.User.Value)"
    Invoke-Sub2ApiNative -FilePath 'icacls.exe' -ArgumentList @(
        $Path, '/grant', "${trustee}:(OI)(CI)M", '/T', '/Q'
    ) -Quiet
}

function Invoke-Sub2ApiDownload {
    param(
        [Parameter(Mandatory = $true)][string]$Url,
        [Parameter(Mandatory = $true)][string]$Destination
    )

    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $directory = Split-Path -Parent $Destination
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
    $temporary = "$Destination.download"
    try {
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $temporary
        Move-Item -LiteralPath $temporary -Destination $Destination -Force
    } finally {
        if (Test-Path -LiteralPath $temporary) {
            Remove-Item -LiteralPath $temporary -Force
        }
    }
}

function Read-Sub2ApiJsonFile {
    param([Parameter(Mandatory = $true)][string]$Path)
    return Get-Content -LiteralPath $Path -Raw -Encoding UTF8 | ConvertFrom-Json
}

function Write-Sub2ApiJsonFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)]$Value
    )

    $directory = Split-Path -Parent $Path
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
    $encoding = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($Path, ($Value | ConvertTo-Json -Depth 8), $encoding)
}

function Get-Sub2ApiVersionFromText {
    param([Parameter(Mandatory = $true)][string]$Text)

    $match = [regex]::Match($Text, 'v?\d+\.\d+\.\d+(?:[.+-][0-9A-Za-z.-]+)?')
    if ($match.Success) {
        return $match.Value
    }
    return $null
}

function Test-Sub2ApiVersionEqual {
    param([string]$Left, [string]$Right)
    if ([string]::IsNullOrWhiteSpace($Left) -or [string]::IsNullOrWhiteSpace($Right)) {
        return $false
    }
    return $Left.TrimStart('v') -eq $Right.TrimStart('v')
}

function Test-Sub2ApiUpgradeAvailable {
    param([string]$Current, [string]$Target)

    $targetMatch = [regex]::Match([string]$Target, '\d+\.\d+\.\d+')
    if (-not $targetMatch.Success) {
        return $false
    }

    $currentMatch = [regex]::Match([string]$Current, '\d+\.\d+\.\d+')
    if (-not $currentMatch.Success) {
        return $true
    }

    return [version]$targetMatch.Value -gt [version]$currentMatch.Value
}

function Wait-Sub2ApiHealthy {
    param([int]$TimeoutSeconds = 180)

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        $status = Invoke-Sub2ApiNative -FilePath 'docker' -ArgumentList @('inspect', '--format', '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}', 'sub2api') -CaptureOutput -Quiet -AllowedExitCodes @(0, 1)
        if ($status -eq 'healthy') {
            return
        }
        if ($status -eq 'running') {
            Start-Sleep -Seconds 2
            return
        }
        Start-Sleep -Seconds 3
    } while ((Get-Date) -lt $deadline)

    throw "Sub2API did not become healthy within $TimeoutSeconds seconds."
}
