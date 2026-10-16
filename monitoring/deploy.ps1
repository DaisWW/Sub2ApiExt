[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '..\scripts\deploy-common.ps1')
. (Join-Path $PSScriptRoot '..\deploy\windows\modules\Common.ps1')

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
$mainContext = New-Sub2ApiContext
if (-not (Test-Sub2ApiDeploymentExists -Context $mainContext) -or
        -not (Test-Sub2ApiPolicyGatewayEnabled -Context $mainContext) -or
        -not (Test-ExtensionContainerExists -Name 'sub2api-policy-gateway')) {
    throw '请先运行根目录的一键部署，监控需要主站网关提供 /monitoring/ 同源登录入口。'
}
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
    -Image $image `
    -Containers @('sub2api-monitoring')

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

Grant-ExtensionRuntimeAccess -Path $runtimeRoot -SensitiveFiles @(
    (Join-Path $runtimeRoot 'database.runtime.env'),
    (Join-Path $runtimeRoot 'redis.runtime.env'),
    (Join-Path $runtimeRoot 'settings.env')
)

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

# Update gateway code while preserving its ProgramData rules and configuration.
$gatewaySource = Join-Path $PSScriptRoot '..\deploy\windows\policy-gateway'
foreach ($name in @('Dockerfile', 'go.mod', 'main.go')) {
    Copy-Item -LiteralPath (Join-Path $gatewaySource $name) -Destination (Join-Path $mainContext.PolicyGatewayRuntimeRoot $name) -Force
}
Invoke-Sub2ApiCompose -Context $mainContext -Arguments @('up', '-d', '--no-deps', '--build', 'policy-gateway')
Wait-ExtensionContainer -Name 'sub2api-policy-gateway'
$monitorHealth = (Invoke-ExtensionDocker -Arguments @('exec', 'sub2api-policy-gateway', 'wget', '-q', '-T', '5', '-O', '-', 'http://127.0.0.1:8080/monitoring/healthz') -Capture | Out-String) | ConvertFrom-Json
if ($monitorHealth.status -ne 'ok' -or $monitorHealth.service -ne 'sub2api-monitoring') {
    throw '主站网关的监控入口健康检查失败。'
}

# Retire the original-workspace deployment only after the independent
# ProgramData deployment is healthy, so two workers do not probe in parallel.
if (Test-ExtensionContainerExists -Name 'sub2api-monitoring-standalone') {
    Write-Host 'Removing legacy monitoring container...' -ForegroundColor Cyan
    Remove-ExtensionContainer -Name 'sub2api-monitoring-standalone'
}

Write-Host ''
Write-Host 'Monitoring deployment completed.' -ForegroundColor Green
Write-Host 'Authenticated dashboard: <main-site-origin>/monitoring/ (use this URL in the Sub2API menu)'
Write-Host "Dashboard (this PC): http://localhost:$port"
if ($lanBind) {
    $hostName = [Net.Dns]::GetHostName()
    Write-Host "Dashboard (LAN): http://${hostName}:$port or http://<this-PC-IP>:$port"
}
Write-Host "Runtime directory: $runtimeRoot"
