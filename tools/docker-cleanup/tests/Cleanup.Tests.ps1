Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path (Split-Path -Parent $PSScriptRoot) 'cleanup-docker.ps1')

function Assert-CleanupTest {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw $Message }
}

$old = '2026-08-01T00:00:00Z'
$images = @(
    @{ Id = 'used'; Created = $old; RepoTags = @('sub2api-ext-monitoring:test') },
    @{ Id = 'local'; Created = $old; RepoTags = @('sub2api-ext-monitoring:local', 'sub2api-ext-monitoring:old-test', 'sub2api-ext-monitoring:rollback-20261011-120000-000-aaaaaaaaaaaa') },
    @{ Id = 'backup'; Created = $old; RepoTags = @('sub2api/local-backup:keep', 'ghcr.io/wei-shaw/sub2api:0.2.5') },
    @{ Id = 'configured'; Created = $old; RepoTags = @('ghcr.io/wei-shaw/sub2api:0.2.15') },
    @{ Id = 'rollback-1'; Created = '2026-09-08T00:00:00Z'; RepoTags = @('sub2api-ext-monitoring:rollback-new') },
    @{ Id = 'rollback-2'; Created = '2026-09-07T00:00:00Z'; RepoTags = @('sub2api-ext-monitoring:before-change') },
    @{ Id = 'rollback-3'; Created = $old; RepoTags = @('sub2api-ext-monitoring:rollback-old') },
    @{ Id = 'test'; Created = $old; RepoTags = @('sub2api-ext-priority-sync:test', 'other-project:shared') },
    @{ Id = 'other'; Created = $old; RepoTags = @('other-project:test') },
    @{ Id = 'fresh'; Created = '2026-10-10T00:00:00Z'; RepoTags = @('sub2api-ext-monitoring:test-new') },
    @{ Id = 'dangling'; Created = $old; RepoTags = $null }
)
$plan = @(Get-Sub2ApiCleanupCandidates -Images $images -ProtectedImageIds @('used', 'backup', 'configured') `
    -Cutoff ([DateTimeOffset]'2026-09-28T00:00:00Z'))
Assert-CleanupTest (($plan.Id | Sort-Object) -join ',' -eq 'rollback-3,test') 'Cleanup must protect containers, configured image, local tags, backups and two rollback images.'
Assert-CleanupTest (($plan | Where-Object { $_.Id -eq 'test' }).References -join ',' -eq 'sub2api-ext-priority-sync:test') 'Shared images must only lose Sub2API references.'
$allAges = @(Get-Sub2ApiCleanupCandidates -Images $images -ProtectedImageIds @('used', 'backup', 'configured') `
    -Cutoff ([DateTimeOffset]'2026-10-12T00:00:00Z'))
Assert-CleanupTest ($allAges.Id -contains 'fresh') 'The initial cleanup must include unused recent test images.'
Write-Host 'PASS: cleanup scope, shared image references, retention and protected image selection.'

$savedImages = @(
    @{ Id = 'old-build'; Created = $old; RepoTags = @('sub2api-ext-rate-sync:rollback-20260927-120000-000-aaaaaaaaaaaa') },
    @{ Id = 'new-build'; Created = '2026-09-20T00:00:00Z'; RepoTags = @('sub2api-ext-rate-sync:rollback-20260926-120000-000-bbbbbbbbbbbb') },
    @{ Id = 'older-save'; Created = '2026-09-21T00:00:00Z'; RepoTags = @('sub2api-ext-rate-sync:rollback-20260925-120000-000-cccccccccccc') },
    @{ Id = 'recent-save'; Created = $old; RepoTags = @('sub2api-ext-priority-sync:rollback-20261001-120000-000-dddddddddddd') },
    @{ Id = 'recent-1'; Created = '2026-10-05T00:00:00Z'; RepoTags = @('sub2api-ext-priority-sync:rollback-new') },
    @{ Id = 'recent-2'; Created = '2026-10-04T00:00:00Z'; RepoTags = @('sub2api-ext-priority-sync:before-new') }
)
$plan = @(Get-Sub2ApiCleanupCandidates -Images $savedImages -Cutoff ([DateTimeOffset]'2026-09-28T00:00:00Z'))
Assert-CleanupTest ($plan.Count -eq 1 -and $plan[0].Id -eq 'older-save') 'Rollback retention must use save time, preserve two distinct previous versions and retain recent saves of old builds.'
Write-Host 'PASS: rollback save time and separate retention for current and previous versions.'

$danglingImages = @(
    @{ Id = 'configured-id'; Created = $old; RepoTags = @() },
    @{ Id = 'unused-dangling'; Created = $old; RepoTags = @() },
    @{ Id = 'fresh-dangling'; Created = '2026-10-10T00:00:00Z'; RepoTags = @() },
    @{ Id = 'digest-only'; Created = $old; RepoTags = @() }
)
$plan = @(Get-Sub2ApiCleanupCandidates -Images $danglingImages -ProtectedImageIds @('configured-id') `
    -Cutoff ([DateTimeOffset]'2026-10-12T00:00:00Z') -DanglingImageIds @('configured-id', 'unused-dangling', 'fresh-dangling') `
    -DanglingCutoff ([DateTimeOffset]'2026-09-28T00:00:00Z'))
Assert-CleanupTest ($plan.Count -eq 1 -and $plan[0].References[0] -eq 'unused-dangling') 'Dangling cleanup must protect configured IDs and keep its own 14-day cutoff during initial cleanup.'
Write-Host 'PASS: protected dangling images, digest-only images and independent cache retention.'
