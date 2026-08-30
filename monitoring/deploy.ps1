[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '..\scripts\deploy-common.ps1')

$runtimeRoot = Get-ExtensionRuntimeRoot -Service 'monitoring'
$elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath -ProbePath $runtimeRoot
if ($null -ne $elevatedExit) {
    exit [int]$elevatedExit
}

$composeEnvPath = Join-Path $runtimeRoot '.env'
$existingComposeEnv = Read-ExtensionEnvFile -Path $composeEnvPath
$bindHost = if ($existingComposeEnv['MONITORING_BIND_HOST']) {
    $existingComposeEnv['MONITORING_BIND_HOST'].Trim()
} else {
    '0.0.0.0'
}
if ($bindHost -eq 'localhost') {
    $bindHost = '127.0.0.1'
}
[Net.IPAddress]$bindAddress = $null
if ($bindHost -notmatch '^\d{1,3}(\.\d{1,3}){3}$' -or
        -not [Net.IPAddress]::TryParse($bindHost, [ref]$bindAddress) -or
        $bindAddress.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) {
    throw "Invalid monitoring IPv4 bind address in ${composeEnvPath}: $bindHost"
}
$bindHost = $bindAddress.ToString()
$lanBind = -not [Net.IPAddress]::IsLoopback($bindAddress)
$port = if ($existingComposeEnv['MONITORING_PORT']) {
    $existingComposeEnv['MONITORING_PORT']
} else {
    '18090'
}
if ($port -notmatch '^\d+$' -or [int]$port -lt 1 -or [int]$port -gt 65535) {
    throw "Invalid monitoring port in ${composeEnvPath}: $port"
}
$needsFirewallChange = -not (Test-ExtensionLanFirewallRule -Port ([int]$port) -Enabled $lanBind)

if ($needsFirewallChange) {
    $elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath -Force
    if ($null -ne $elevatedExit) {
        exit [int]$elevatedExit
    }
}

Assert-ExtensionDocker
$sub2api = Get-Sub2ApiDockerContext
$image = 'sub2api-ext-monitoring:local'

Build-ExtensionImage `
    -Context $PSScriptRoot `
    -Dockerfile (Join-Path $PSScriptRoot 'Dockerfile') `
    -Image $image

New-Item -ItemType Directory -Path $runtimeRoot -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'compose.runtime.yml') `
    -Destination (Join-Path $runtimeRoot 'docker-compose.yml') -Force
Copy-Item -LiteralPath (Join-Path $PSScriptRoot '..\scripts\manage-runtime.ps1') `
    -Destination (Join-Path $runtimeRoot 'manage.ps1') -Force
Copy-Item -LiteralPath (Join-Path $PSScriptRoot '..\scripts\manage-runtime.bat') `
    -Destination (Join-Path $runtimeRoot 'manage.bat') -Force

$settingsPath = Join-Path $runtimeRoot 'settings.env'
if (-not (Test-Path -LiteralPath $settingsPath -PathType Leaf)) {
    $localSettings = Join-Path $PSScriptRoot 'settings.env'
    $settingsSource = if (Test-Path -LiteralPath $localSettings -PathType Leaf) {
        $localSettings
    } else {
        Join-Path $PSScriptRoot 'settings.env.example'
    }
    Copy-Item -LiteralPath $settingsSource -Destination $settingsPath
}

$settingsValues = Read-ExtensionEnvFile -Path $settingsPath
$frameAncestors = [string]$settingsValues['MONITORING_FRAME_ANCESTORS']
if ([string]::IsNullOrWhiteSpace($frameAncestors) -or $frameAncestors.Trim() -eq "'self'") {
    $siteOrigins = @()
    try {
        $siteURLQuery = @'
SELECT value
FROM settings
WHERE key IN ('frontend_url', 'api_base_url')
  AND btrim(value) <> ''
ORDER BY CASE key WHEN 'frontend_url' THEN 0 ELSE 1 END;
'@
        $siteURLs = @(Invoke-ExtensionDocker -Arguments @(
            'exec', 'sub2api-postgres', 'psql',
            '-U', $sub2api.PostgresUser,
            '-d', $sub2api.PostgresDatabase,
            '-At', '-c', $siteURLQuery
        ) -Capture | ForEach-Object { ([string]$_).Trim() } | Where-Object { $_ -ne '' })

        foreach ($siteURL in $siteURLs) {
            [Uri]$siteUri = $null
            if ([Uri]::TryCreate($siteURL, [UriKind]::Absolute, [ref]$siteUri) -and
                    ($siteUri.Scheme -eq 'http' -or $siteUri.Scheme -eq 'https') -and
                    -not [string]::IsNullOrWhiteSpace($siteUri.Host) -and
                    [string]::IsNullOrEmpty($siteUri.UserInfo)) {
                $origin = $siteUri.GetLeftPart([UriPartial]::Authority).ToLowerInvariant()
                if ($siteOrigins -notcontains $origin) {
                    $siteOrigins += $origin
                }
                break
            }
        }
    }
    catch {
        Write-Warning "Could not discover the Sub2API site origin. $($_.Exception.Message)"
    }

    if ($siteOrigins.Count -gt 0) {
        $frameAncestors = (@("'self'") + $siteOrigins) -join ' '
        $settingsLines = [Collections.Generic.List[string]]::new()
        $updatedFrameAncestors = $false
        foreach ($line in [IO.File]::ReadAllLines($settingsPath)) {
            if ($line -match '^\s*MONITORING_FRAME_ANCESTORS=') {
                $settingsLines.Add("MONITORING_FRAME_ANCESTORS=$frameAncestors")
                $updatedFrameAncestors = $true
            }
            else {
                $settingsLines.Add($line)
            }
        }
        if (-not $updatedFrameAncestors) {
            $settingsLines.Add("MONITORING_FRAME_ANCESTORS=$frameAncestors")
        }
        [IO.File]::WriteAllLines($settingsPath, $settingsLines, [Text.UTF8Encoding]::new($false))
        Write-Host "Monitoring iframe: allowing $($siteOrigins -join ', ')." -ForegroundColor DarkGray
    }
    else {
        Write-Warning 'Sub2API frontend_url/api_base_url is empty or invalid. Configure MONITORING_FRAME_ANCESTORS before embedding the monitoring page.'
    }
}

Write-ExtensionEnvFile -Path $composeEnvPath -Values ([ordered]@{
    SUB2API_NETWORK = $sub2api.Network
    MONITORING_BIND_HOST = $bindHost
    MONITORING_PORT = $port
})
Write-ExtensionEnvFile -Path (Join-Path $runtimeRoot 'database.runtime.env') -Values ([ordered]@{
    DATABASE_HOST = $sub2api.PostgresHost
    DATABASE_PORT = $sub2api.PostgresPort
    DATABASE_USER = $sub2api.PostgresUser
    DATABASE_PASSWORD = $sub2api.PostgresPassword
    DATABASE_DBNAME = $sub2api.PostgresDatabase
    DATABASE_SSLMODE = 'disable'
})
Grant-ExtensionRuntimeAccess -Path $runtimeRoot

# Replace the pre-Compose verification container from the original workspace.
Remove-ExtensionContainer -Name 'sub2api-monitor-check'
if ($needsFirewallChange) {
    if (-not (Set-ExtensionLanFirewallRule -Port ([int]$port) -Enabled $lanBind) -or
            -not (Test-ExtensionLanFirewallRule -Port ([int]$port) -Enabled $lanBind)) {
        throw "Could not configure the Windows Firewall rule for monitoring TCP $port."
    }
}
Start-ExtensionCompose -RuntimeRoot $runtimeRoot
Wait-ExtensionContainer -Name 'sub2api-monitoring'

# Retire the original-workspace deployment only after the independent
# ProgramData deployment is healthy, so two workers do not probe in parallel.
if (Test-ExtensionContainerExists -Name 'sub2api-monitoring-standalone') {
    Write-Host 'Removing legacy monitoring container...' -ForegroundColor Cyan
    Remove-ExtensionContainer -Name 'sub2api-monitoring-standalone'
}

Write-Host ''
Write-Host 'Monitoring deployment completed.' -ForegroundColor Green
Write-Host "Dashboard (this PC): http://localhost:$port"
if ($lanBind) {
    $hostName = [Net.Dns]::GetHostName()
    Write-Host "Dashboard (LAN): http://${hostName}:$port or http://<this-PC-IP>:$port"
}
Write-Host "Runtime directory: $runtimeRoot"
