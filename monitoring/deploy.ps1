[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '..\scripts\deploy-common.ps1')

$runtimeRoot = Get-ExtensionRuntimeRoot -Service 'monitoring'
$composeEnvPath = Join-Path $runtimeRoot '.env'
$existingComposeEnv = Read-ExtensionEnvFile -Path $composeEnvPath
$bindHost = if ($existingComposeEnv['MONITORING_BIND_HOST']) {
    $existingComposeEnv['MONITORING_BIND_HOST']
} else {
    '0.0.0.0'
}
# Migrate the old installer-generated loopback default once. After this marker
# exists, an operator's explicit bind-host override is preserved on redeploy.
if (-not $existingComposeEnv.ContainsKey('MONITORING_DEPLOY_CONFIG_VERSION') -and $bindHost -eq '127.0.0.1') {
    $bindHost = '0.0.0.0'
}
$lanBind = $bindHost -notin @('127.0.0.1', '::1', 'localhost')
$port = if ($existingComposeEnv['MONITORING_PORT']) {
    $existingComposeEnv['MONITORING_PORT']
} else {
    '18090'
}
if ($port -notmatch '^\d+$' -or [int]$port -lt 1 -or [int]$port -gt 65535) {
    throw "Invalid monitoring port in ${composeEnvPath}: $port"
}
$needsLanFirewall = $lanBind -and -not (Test-ExtensionLanFirewallRule -Port ([int]$port))

$elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath -Force:$needsLanFirewall
if ($null -ne $elevatedExit) {
    exit [int]$elevatedExit
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

Write-ExtensionEnvFile -Path $composeEnvPath -Values ([ordered]@{
    SUB2API_NETWORK = $sub2api.Network
    MONITORING_BIND_HOST = $bindHost
    MONITORING_PORT = $port
    MONITORING_DEPLOY_CONFIG_VERSION = '2'
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
Start-ExtensionCompose -RuntimeRoot $runtimeRoot
Wait-ExtensionContainer -Name 'sub2api-monitoring'

if ($lanBind) {
    [void](Set-ExtensionLanFirewallRule -Port ([int]$port) -Enabled $true)
}

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
