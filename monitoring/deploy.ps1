[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '..\scripts\deploy-common.ps1')

$elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath
if ($null -ne $elevatedExit) {
    exit [int]$elevatedExit
}

Assert-ExtensionDocker
$sub2api = Get-Sub2ApiDockerContext
$runtimeRoot = Get-ExtensionRuntimeRoot -Service 'monitoring'
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

$composeEnvPath = Join-Path $runtimeRoot '.env'
$existingComposeEnv = Read-ExtensionEnvFile -Path $composeEnvPath
$bindHost = if ($existingComposeEnv['MONITORING_BIND_HOST']) {
    $existingComposeEnv['MONITORING_BIND_HOST']
} else {
    '127.0.0.1'
}
$port = if ($existingComposeEnv['MONITORING_PORT']) {
    $existingComposeEnv['MONITORING_PORT']
} else {
    '18090'
}
if ($port -notmatch '^\d+$' -or [int]$port -lt 1 -or [int]$port -gt 65535) {
    throw "Invalid monitoring port in ${composeEnvPath}: $port"
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
Start-ExtensionCompose -RuntimeRoot $runtimeRoot
Wait-ExtensionContainer -Name 'sub2api-monitoring'

Write-Host ''
Write-Host 'Monitoring deployment completed.' -ForegroundColor Green
Write-Host "Dashboard: http://localhost:$port"
Write-Host "Runtime directory: $runtimeRoot"
