[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '..\scripts\deploy-common.ps1')

$runtimeRoot = Get-ExtensionRuntimeRoot -Service 'rate-sync'
$elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath -ProbePath $runtimeRoot
if ($null -ne $elevatedExit) {
    exit [int]$elevatedExit
}

Assert-ExtensionDocker
$sub2api = Get-Sub2ApiDockerContext
$image = 'sub2api-ext-rate-sync:local'

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

$configs = @(
    @{ Name = 'config.json'; Example = 'config.example.json'; LegacyContainer = 'sub2api-rate-sync' },
    @{ Name = 'account-config.json'; Example = 'account-config.example.json'; LegacyContainer = 'sub2api-rate-sync-account' }
)
foreach ($config in $configs) {
    $destination = Join-Path $runtimeRoot $config.Name
    if (-not (Test-Path -LiteralPath $destination -PathType Leaf)) {
        $localConfig = Join-Path $PSScriptRoot $config.Name
        $source = Get-ExtensionHostBindSource `
            -Container $config.LegacyContainer `
            -Destination '/app/config.json' `
            -RelativePath $config.Name
        if ([string]::IsNullOrWhiteSpace($source)) {
            $source = if (Test-Path -LiteralPath $localConfig -PathType Leaf) {
                $localConfig
            } else {
                Join-Path $PSScriptRoot $config.Example
            }
        }
        Copy-Item -LiteralPath $source -Destination $destination
    }
    Get-Content -LiteralPath $destination -Raw | ConvertFrom-Json | Out-Null
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
Grant-ExtensionRuntimeAccess -Path $runtimeRoot

Start-ExtensionCompose -RuntimeRoot $runtimeRoot
Wait-ExtensionContainer -Name 'sub2api-rate-sync'
Wait-ExtensionContainer -Name 'sub2api-rate-sync-account'

Write-Host ''
Write-Host 'Rate sync deployment completed.' -ForegroundColor Green
Write-Host "Runtime directory: $runtimeRoot"
