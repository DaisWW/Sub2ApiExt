Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path (Split-Path -Parent $PSScriptRoot) 'deploy-common.ps1')

function Assert-ExtensionImageTest {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw $Message }
}

$script:containerImages = @{}
$script:imageTags = @{}
$script:dockerCalls = [Collections.Generic.List[object]]::new()
$script:failTag = $false
$script:failBuild = $false
function Test-ExtensionContainerExists { param($Name); return $script:containerImages.ContainsKey($Name) }
function Get-ExtensionContainerInspect { param($Name); return @{ Image = $script:containerImages[$Name] } }
function Invoke-ExtensionDocker {
    param([string[]]$Arguments, [switch]$Capture)
    $script:dockerCalls.Add($Arguments)
    if ($Arguments[0] -eq 'image' -and $Arguments[1] -eq 'inspect') {
        return ConvertTo-Json -InputObject @($script:imageTags[$Arguments[-1]]) -Compress
    }
    if ($Arguments[0] -eq 'image' -and $Arguments[1] -eq 'tag') {
        if ($script:failTag) { throw 'Simulated tag failure.' }
        if ($script:imageTags[$Arguments[2]] -notcontains $Arguments[3]) {
            $script:imageTags[$Arguments[2]] += @($Arguments[3])
        }
        return
    }
    if ($Arguments[0] -eq 'image' -and $Arguments[1] -eq 'rm') {
        foreach ($id in @($script:imageTags.Keys)) {
            $script:imageTags[$id] = @($script:imageTags[$id] | Where-Object { $_ -ne $Arguments[2] })
        }
        return
    }
    Assert-ExtensionImageTest ($Arguments[0] -eq 'build') 'Unexpected Docker command.'
    if ($script:failBuild) { throw 'Simulated build failure.' }
}

$oldId = 'sha256:' + ('a' * 64)
$script:imageTags[$oldId] = @('sub2api-ext-rate-sync:manual-rollback')
$buildParameters = @{ Context = '.'; Dockerfile = 'Dockerfile'; Image = 'sub2api-ext-rate-sync:local'; Containers = @('sub2api-rate-sync', 'sub2api-rate-sync-account') }
Build-ExtensionImage @buildParameters
Assert-ExtensionImageTest ($script:dockerCalls.Count -eq 1) 'First installation must build without a rollback tag.'

$script:containerImages = @{ 'sub2api-rate-sync' = $oldId; 'sub2api-rate-sync-account' = $oldId }
$script:dockerCalls.Clear()
Build-ExtensionImage @buildParameters
Assert-ExtensionImageTest ($script:dockerCalls.Count -eq 3 -and $script:dockerCalls[1][1] -eq 'tag' -and $script:dockerCalls[2][0] -eq 'build') 'Two containers sharing an image must save one rollback before building.'
Assert-ExtensionImageTest ($script:imageTags[$oldId][-1] -match '^sub2api-ext-rate-sync:rollback-\d{8}-\d{6}-\d{3}-aaaaaaaaaaaa$') 'Automatic rollback tags must include UTC save time and the previous image identity.'

$expiredTag = 'sub2api-ext-rate-sync:rollback-20260801-000000-000-aaaaaaaaaaaa'
$script:imageTags[$oldId] = @('sub2api-ext-rate-sync:manual-rollback', $expiredTag)
$script:dockerCalls.Clear()
Build-ExtensionImage @buildParameters
Assert-ExtensionImageTest ($script:imageTags[$oldId] -notcontains $expiredTag -and $script:imageTags[$oldId].Count -eq 2) 'Saving a reused version must refresh its retention time without accumulating automatic tags or removing manual tags.'
Assert-ExtensionImageTest ($script:dockerCalls[1][1] -eq 'tag' -and $script:dockerCalls[2][1] -eq 'rm' -and $script:dockerCalls[-1][0] -eq 'build') 'The refreshed rollback tag must exist before removing its old tag or building.'

$script:imageTags[$oldId] = @($expiredTag)
$script:failTag = $true
$script:dockerCalls.Clear()
$failed = $false
try { Build-ExtensionImage @buildParameters } catch { $failed = $true }
Assert-ExtensionImageTest ($failed -and @($script:dockerCalls | Where-Object { $_[0] -eq 'build' -or $_[1] -eq 'rm' }).Count -eq 0 -and $script:imageTags[$oldId] -contains $expiredTag) 'A rollback tag failure must preserve the previous tag and prevent overwriting the local image.'
$script:failTag = $false
$script:failBuild = $true
$script:imageTags[$oldId] = @()
$failed = $false
try { Build-ExtensionImage @buildParameters } catch { $failed = $true }
Assert-ExtensionImageTest ($failed -and $script:imageTags[$oldId].Count -eq 1) 'A failed build must retain the saved rollback image.'
Write-Host 'PASS: deployed image preservation, retention refresh, tag deduplication and tag/build failures.'
