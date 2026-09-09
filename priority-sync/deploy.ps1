[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '..\scripts\deploy-common.ps1')

$runtimeRoot = Get-ExtensionRuntimeRoot -Service 'priority-sync'
$elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath -ProbePath $runtimeRoot
if ($null -ne $elevatedExit) {
    exit [int]$elevatedExit
}

Assert-ExtensionDocker
$sub2api = Get-Sub2ApiDockerContext
$image = 'sub2api-ext-priority-sync:local'

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
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'settings.env.example') -Destination $settingsPath
}

Write-ExtensionEnvFile -Path (Join-Path $runtimeRoot '.env') -Values ([ordered]@{
    SUB2API_NETWORK = $sub2api.Network
})
Write-ExtensionEnvFile -Path (Join-Path $runtimeRoot '.runtime.env') -Values ([ordered]@{
    PGHOST = $sub2api.PostgresHost
    PGPORT = $sub2api.PostgresPort
    PGUSER = $sub2api.PostgresUser
    PGPASSWORD = $sub2api.PostgresPassword
    PGDATABASE = $sub2api.PostgresDatabase
    PGSSLMODE = 'disable'
    PGCONNECT_TIMEOUT = '10'
    PGOPTIONS = '-c default_transaction_read_only=on'
})
Grant-ExtensionRuntimeAccess -Path $runtimeRoot -SensitiveFiles @(
    (Join-Path $runtimeRoot '.runtime.env'),
    (Join-Path $runtimeRoot 'settings.env')
)

Start-ExtensionCompose -RuntimeRoot $runtimeRoot
Wait-ExtensionContainer -Name 'sub2api-priority-sync'

Write-Host ''
Write-Host 'Priority sync deployment completed (dry-run by default).' -ForegroundColor Green
Write-Host "Runtime directory: $runtimeRoot"
