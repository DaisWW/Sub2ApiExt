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
$application = Get-ExtensionContainerInspect -Name 'sub2api'
$redis = Get-ExtensionContainerInspect -Name 'sub2api-redis'
if (-not $redis.State.Running) {
    throw 'The sub2api-redis container must be running.'
}
$applicationEnvironment = Get-ExtensionContainerEnvironment -Inspect $application
$redisEnvironment = Get-ExtensionContainerEnvironment -Inspect $redis
$redisHost = if ($applicationEnvironment['REDIS_HOST']) {
    [string]$applicationEnvironment['REDIS_HOST']
} else {
    ([string]$redis.Name).TrimStart('/')
}
$redisPort = if ($applicationEnvironment['REDIS_PORT']) { [string]$applicationEnvironment['REDIS_PORT'] } else { '6379' }
if ($redisPort -notmatch '^\d+$' -or [int]$redisPort -lt 1 -or [int]$redisPort -gt 65535) {
    throw 'Could not determine the Redis port.'
}
$redisDB = if ($applicationEnvironment['REDIS_DB']) { [string]$applicationEnvironment['REDIS_DB'] } else { '0' }
if ($redisDB -notmatch '^\d+$') {
    throw 'Could not determine the Redis database.'
}
$redisPassword = if ($applicationEnvironment.ContainsKey('REDIS_PASSWORD')) {
    [string]$applicationEnvironment['REDIS_PASSWORD']
} elseif ($redisEnvironment.ContainsKey('REDIS_PASSWORD')) {
    [string]$redisEnvironment['REDIS_PASSWORD']
} else {
    ''
}
$redisTLS = if ($applicationEnvironment['REDIS_ENABLE_TLS']) {
    [string]$applicationEnvironment['REDIS_ENABLE_TLS']
} else {
    'false'
}
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
Write-ExtensionEnvFile -Path (Join-Path $runtimeRoot 'redis.runtime.env') -Values ([ordered]@{
    REDIS_HOST = $redisHost
    REDIS_PORT = $redisPort
    REDIS_PASSWORD = $redisPassword
    REDIS_DB = $redisDB
    REDIS_ENABLE_TLS = $redisTLS
})

# Keep all iframe/access repair in one local-only script.  It reads the
# current Sub2API settings and monitoring runtime files; it never writes a
# Sub2API menu or calls the Sub2API Admin API.
$fixScript = Join-Path $PSScriptRoot 'fix-monitoring-access.ps1'
try {
    & $fixScript -RuntimeRoot $runtimeRoot -PrepareOnly -SkipFirewall -SkipElevation
}
catch {
    throw "监控访问配置修复失败：$($_.Exception.Message)"
}

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
