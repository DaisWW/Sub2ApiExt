[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'scripts\deploy-common.ps1')

$elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath
if ($null -ne $elevatedExit) {
    exit [int]$elevatedExit
}

Write-Host 'Deploying Sub2ApiExt services...' -ForegroundColor Cyan
& (Join-Path $PSScriptRoot 'rate-sync\deploy.ps1')
& (Join-Path $PSScriptRoot 'monitoring\deploy.ps1')
Write-Host ''
Write-Host 'All Sub2ApiExt services were deployed.' -ForegroundColor Green
