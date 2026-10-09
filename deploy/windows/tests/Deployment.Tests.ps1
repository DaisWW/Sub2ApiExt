Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$moduleRoot = Join-Path (Split-Path -Parent $PSScriptRoot) 'modules'
. (Join-Path $moduleRoot 'Common.ps1')
. (Join-Path $moduleRoot 'Docker.ps1')
. (Join-Path $moduleRoot 'Deployment.ps1')

function Assert-DeploymentTest {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) {
        throw $Message
    }
}

$testRoot = Join-Path ([System.IO.Path]::GetTempPath()) "sub2api-deployment-test-$([guid]::NewGuid())"
$context = New-Sub2ApiContext
$context.AppRoot = $testRoot
$context.PolicyGatewayConfigFile = Join-Path $testRoot 'config.json'
$script:calls = New-Object 'System.Collections.Generic.List[string]'
$script:tasks = @()
$script:gatewayExists = $false
$script:disableFails = $false
$script:stopFails = $false
$script:startDuringDisable = $false
$script:taskRunning = $false

function Write-Sub2ApiMessage { param($Level, $Message) }
function Get-ScheduledTask {
    param($TaskPath, $ErrorAction)
    Assert-DeploymentTest ($TaskPath -eq '\') 'Task lookup must stay in the root task folder.'
    return $script:tasks
}
function Disable-ScheduledTask {
    param($TaskName, $TaskPath, $ErrorAction)
    Assert-DeploymentTest ($TaskName -eq 'Sub2API Auto Start' -and $TaskPath -eq '\') 'Unexpected task disable target.'
    if ($script:disableFails) { throw 'Task access denied.' }
    $script:calls.Add('disable-task')
    if ($script:startDuringDisable) { $script:taskRunning = $true }
}
function Stop-ScheduledTask {
    param($TaskName, $TaskPath, $ErrorAction)
    Assert-DeploymentTest ($TaskName -eq 'Sub2API Auto Start' -and $TaskPath -eq '\') 'Unexpected task stop target.'
    if ($script:stopFails) { throw 'Task stop failed.' }
    $script:calls.Add('stop-task')
    $script:taskRunning = $false
}
function Test-Sub2ApiNative {
    param($FilePath, $ArgumentList)
    Assert-DeploymentTest ($FilePath -eq 'docker' -and ($ArgumentList -join ' ') -eq 'container inspect sub2api-policy-gateway') 'Unexpected container inspect target.'
    return $script:gatewayExists
}
function Invoke-Sub2ApiNative {
    param($FilePath, $ArgumentList, [switch]$CaptureOutput, [switch]$Quiet)
    Assert-DeploymentTest ($FilePath -eq 'docker') 'Unexpected deployment command.'
    if ($ArgumentList[0] -eq 'rm') {
        Assert-DeploymentTest (($ArgumentList -join ' ') -eq 'rm --force sub2api-policy-gateway') 'Unexpected container removal target.'
        $script:calls.Add('remove-gateway')
        $script:gatewayExists = $false
        return
    }
    Assert-DeploymentTest ($ArgumentList[0] -eq 'compose' -and $ArgumentList -contains $context.ComposeFile -and $ArgumentList -contains $context.ComposeOverrideFile) 'Compose must retain both base files.'
    $withPolicy = $ArgumentList -contains $context.PolicyGatewayOverrideFile
    if (-not $withPolicy -and $script:gatewayExists) {
        throw 'Bind for 0.0.0.0:18080 failed: port is already allocated.'
    }
    $script:calls.Add("compose-policy=$withPolicy")
    $script:gatewayExists = $withPolicy
}
function Wait-Sub2ApiHealthy { $script:calls.Add('healthy-sub2api') }
function Wait-Sub2ApiPolicyGatewayHealthy {
    param($Context)
    if (Test-Sub2ApiPolicyGatewayEnabled -Context $Context) {
        $script:calls.Add('healthy-gateway')
    }
}

try {
    New-Item -ItemType Directory -Path $testRoot | Out-Null
    $legacyScript = Join-Path $testRoot 'manager\AutoStart.ps1'
    foreach ($case in @(
        @{ State = 'Ready'; Arguments = "-File $legacyScript"; Expected = 'disable-task,stop-task' },
        @{ State = 'Running'; Arguments = "-NoProfile -File `"$legacyScript`""; Expected = 'disable-task,stop-task' },
        @{ State = 'Disabled'; Arguments = "-File $legacyScript"; Expected = 'disable-task,stop-task' },
        @{ State = 'Ready'; Arguments = "-File $legacyScript.other.ps1"; Expected = '' },
        @{ State = 'Ready'; Arguments = '-File C:\other\AutoStart.ps1'; Expected = '' }
    )) {
        $script:calls.Clear()
        $script:taskRunning = $case.State -eq 'Running'
        $script:startDuringDisable = $case.State -eq 'Ready'
        $script:tasks = @([pscustomobject]@{
            TaskName = 'Sub2API Auto Start'
            State = $case.State
            Actions = @([pscustomobject]@{ Execute = 'powershell.exe'; Arguments = $case.Arguments })
        })
        Disable-Sub2ApiLegacyAutoStart -Context $context
        Assert-DeploymentTest (($script:calls -join ',') -eq $case.Expected) 'Legacy task ownership or running-state handling failed.'
        Assert-DeploymentTest (-not $script:taskRunning) 'An instance started during task disabling must be stopped.'
    }
    $script:startDuringDisable = $false
    $script:calls.Clear()
    $script:tasks = @()
    Disable-Sub2ApiLegacyAutoStart -Context $context
    Assert-DeploymentTest ($script:calls.Count -eq 0) 'A missing legacy task must be harmless.'

    $script:tasks = @([pscustomobject]@{
        TaskName = 'Sub2API Auto Start'
        State = 'Ready'
        Actions = @([pscustomobject]@{ Execute = 'powershell.exe'; Arguments = "-File $legacyScript" })
    })
    $script:disableFails = $true
    $failed = $false
    try { Disable-Sub2ApiLegacyAutoStart -Context $context } catch { $failed = $true }
    Assert-DeploymentTest $failed 'Task migration errors must prevent deployment from continuing.'
    $script:disableFails = $false
    $script:stopFails = $true
    $failed = $false
    try { Disable-Sub2ApiLegacyAutoStart -Context $context } catch { $failed = $true }
    Assert-DeploymentTest $failed 'Task stop errors must prevent deployment from continuing.'
    $script:stopFails = $false
    Write-Host 'PASS: legacy task ownership, idempotence, concurrent start and migration failures.'

    Write-Sub2ApiJsonFile -Path $context.PolicyGatewayConfigFile -Value @{ enabled = $true }
    $script:gatewayExists = $true
    $failed = $false
    try {
        Invoke-Sub2ApiNative -FilePath docker -ArgumentList @('compose', '-f', $context.ComposeFile, '-f', $context.ComposeOverrideFile, 'up', '-d')
    } catch { $failed = $true }
    Assert-DeploymentTest $failed 'The old two-file Compose command must fail in the port-conflict simulation.'

    foreach ($enabled in @($true, $false, $true, $false)) {
        Write-Sub2ApiJsonFile -Path $context.PolicyGatewayConfigFile -Value @{ enabled = $enabled }
        $script:calls.Clear()
        Start-Sub2ApiComposeServices -Context $context
        $expected = if ($enabled) {
            'compose-policy=True,healthy-sub2api,healthy-gateway'
        } else {
            'remove-gateway,compose-policy=False,healthy-sub2api'
        }
        Assert-DeploymentTest (($script:calls -join ',') -eq $expected) "Policy mode $enabled did not preserve port ownership or health checks."
    }
    $script:calls.Clear()
    Start-Sub2ApiComposeServices -Context $context -ApplicationOnly
    Assert-DeploymentTest (($script:calls -join ',') -eq 'compose-policy=False,healthy-sub2api') 'Repeated disabled deployment must be harmless.'

    Write-Sub2ApiJsonFile -Path $context.PolicyGatewayConfigFile -Value @{ enabled = 'false' }
    $script:calls.Clear()
    $failed = $false
    try { Start-Sub2ApiComposeServices -Context $context } catch { $failed = $true }
    Assert-DeploymentTest ($failed -and $script:calls.Count -eq 0) 'Invalid policy config must fail before changing containers.'
    Write-Host 'PASS: port conflict simulation, both policy transitions, repeat deployment and invalid config.'
} finally {
    Remove-Sub2ApiSafeItem -Path $testRoot -AllowedRoot ([System.IO.Path]::GetTempPath())
}
