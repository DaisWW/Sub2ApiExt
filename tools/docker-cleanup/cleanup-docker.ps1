[CmdletBinding()]
param(
    [switch]$Apply,
    [switch]$IncludeRecent,
    [switch]$PruneBuildCache
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Invoke-CleanupDocker {
    param([string[]]$Arguments)

    $previousErrorAction = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $output = & docker @Arguments 2>&1
        if ($LASTEXITCODE -ne 0) {
            throw "Docker cleanup command failed: docker $($Arguments -join ' ')`n$($output | Out-String)"
        }
        return $output
    } finally {
        $ErrorActionPreference = $previousErrorAction
    }
}

function Get-Sub2ApiCleanupCandidates {
    param(
        [object[]]$Images,
        [string[]]$ProtectedImageIds,
        [DateTimeOffset]$Cutoff,
        [string[]]$DanglingImageIds,
        [DateTimeOffset]$DanglingCutoff = [DateTimeOffset]::MinValue
    )

    $repositories = @(
        'sub2api-ext-rate-sync', 'sub2api-ext-priority-sync', 'sub2api-ext-monitoring',
        'sub2api-ext-monitoring-build', 'sub2api-ext-monitoring-builder',
        'sub2api-monitor', 'sub2api-monitoring', 'sub2api-monitoring-preview-builder',
        'sub2api-rate-sync', 'sub2api-rate-sync-test', 'sub2api-rate-sync-test-nocache',
        'sub2apiext-monitoring-review', 'sub2apiext-rate-sync-review',
        'ghcr.io/wei-shaw/sub2api', 'ghcr.m.daocloud.io/wei-shaw/sub2api',
        'weishaw/sub2api', 'sub2api/local-backup', 'sub2api/policy-gateway'
    )
    $protected = @{}
    $retainedSince = @{}
    foreach ($id in $ProtectedImageIds) { $protected[$id] = $true }
    foreach ($image in $Images) {
        $retainedSince[$image.Id] = [DateTimeOffset]$image.Created
        foreach ($reference in @($image.RepoTags)) {
            if ($reference -match '^sub2api-ext-(rate-sync|priority-sync|monitoring):rollback-(\d{8}-\d{6}-\d{3})-[0-9a-f]{12}$') {
                $savedAt = [DateTimeOffset]::ParseExact($Matches[2], 'yyyyMMdd-HHmmss-fff',
                    [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AssumeUniversal)
                if ($savedAt -gt $retainedSince[$image.Id]) { $retainedSince[$image.Id] = $savedAt }
            }
            if ($reference -match '^sub2api-(ext-(rate-sync|priority-sync|monitoring)|rate-sync|monitoring):local$' -or
                $reference -eq 'sub2api/policy-gateway:local') {
                $protected[$image.Id] = $true
            }
        }
    }
    foreach ($repository in @('sub2api-ext-rate-sync', 'sub2api-ext-priority-sync', 'sub2api-ext-monitoring')) {
        $rollbackImages = @($Images | Where-Object {
            -not $protected.ContainsKey($_.Id) -and @($_.RepoTags | Where-Object {
                $null -ne $_ -and $_.StartsWith("${repository}:") -and ($_ -match '(?i)rollback|:before-')
            }).Count -gt 0
        } | Sort-Object { $retainedSince[$_.Id] } -Descending | Select-Object -First 2)
        foreach ($image in $rollbackImages) { $protected[$image.Id] = $true }
    }
    foreach ($image in $Images) {
        if ($protected.ContainsKey($image.Id)) { continue }
        $references = @($image.RepoTags | Where-Object { $repositories -contains ($_ -replace ':[^:]+$', '') })
        if ($DanglingImageIds -contains $image.Id -and [DateTimeOffset]$image.Created -lt $DanglingCutoff) {
            $references = @($image.Id)
        } elseif ($retainedSince[$image.Id] -ge $Cutoff) {
            continue
        }
        if ($references.Count -gt 0) {
            [pscustomobject]@{ Id = $image.Id; References = $references }
        }
    }
}

if ($MyInvocation.InvocationName -eq '.') { return }

foreach ($path in @(
    (Join-Path $env:ProgramFiles 'Docker\Docker\resources\bin'),
    (Join-Path $env:LOCALAPPDATA 'Programs\Docker\Docker\resources\bin')
)) {
    if (Test-Path -LiteralPath $path -PathType Container) { $env:Path = "$path;$env:Path" }
}
Invoke-CleanupDocker -Arguments @('info', '--format', '{{.ServerVersion}}') | Out-Null
$appRoot = Join-Path $env:ProgramData 'Sub2API'
$backupRoot = Join-Path $appRoot 'backups'
$envFile = Join-Path $appRoot 'runtime\.env'
if (-not (Test-Path -LiteralPath $backupRoot -PathType Container) -or
    -not (Test-Path -LiteralPath $envFile -PathType Leaf)) {
    throw 'The deployed Sub2API configuration or backup directory is missing; cleanup was skipped.'
}
$backupReferences = @(foreach ($directory in Get-ChildItem -LiteralPath $backupRoot -Directory) {
    $manifestPath = Join-Path $directory.FullName 'manifest.json'
    $manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ([string]::IsNullOrWhiteSpace($manifest.BackupImageTag)) {
        throw "Backup image reference is missing: $manifestPath"
    }
    [string]$manifest.BackupImageTag
})
$configuredImage = @(Get-Content -LiteralPath $envFile -Encoding UTF8 |
    Where-Object { $_ -match '^SUB2API_IMAGE=' } | Select-Object -First 1)
if ($configuredImage.Count -ne 1) { throw 'SUB2API_IMAGE is missing; cleanup was skipped.' }
$configuredImage = $configuredImage[0].Substring('SUB2API_IMAGE='.Length).Trim().Trim('"').Trim("'")
if ([string]::IsNullOrWhiteSpace($configuredImage)) { throw 'SUB2API_IMAGE is empty; cleanup was skipped.' }
# Resolve references before planning: IDs/digests can refer to an untagged image.
$protectedImageIds = @(Invoke-CleanupDocker -Arguments (@('image', 'inspect', '--format', '{{.Id}}', $configuredImage) + $backupReferences))
$containers = @(Invoke-CleanupDocker -Arguments @('ps', '-aq'))
if ($containers.Count -gt 0) {
    $protectedImageIds += @(Invoke-CleanupDocker -Arguments (@('inspect', '--format', '{{.Image}}') + $containers))
}
$imageRows = @(Invoke-CleanupDocker -Arguments @('image', 'ls', '--no-trunc', '--format', '{{json .}}') |
    ForEach-Object { $_ | ConvertFrom-Json })
$images = @(foreach ($group in $imageRows | Group-Object ID) {
    $created = [DateTimeOffset]::Parse(($group.Group[0].CreatedAt -replace ' [A-Za-z]+$', ''),
        [Globalization.CultureInfo]::InvariantCulture)
    $references = @($group.Group | Where-Object { $_.Repository -ne '<none>' -and $_.Tag -ne '<none>' } |
        ForEach-Object { "$($_.Repository):$($_.Tag)" })
    [pscustomobject]@{ Id = $group.Name; Created = $created; RepoTags = $references }
})
$cutoff = [DateTimeOffset]::UtcNow.AddDays(-14)
$danglingImageIds = @()
$danglingCutoff = [DateTimeOffset]::MinValue
if ($PruneBuildCache) {
    $danglingCutoff = $cutoff
    $danglingImageIds = @(Invoke-CleanupDocker -Arguments @('image', 'ls', '--filter', 'dangling=true', '--no-trunc', '-q'))
}
if ($IncludeRecent) { $cutoff = [DateTimeOffset]::UtcNow }
$candidates = @(Get-Sub2ApiCleanupCandidates -Images $images -ProtectedImageIds $protectedImageIds `
    -Cutoff $cutoff -DanglingImageIds $danglingImageIds -DanglingCutoff $danglingCutoff)
Write-Output "Unused image candidates: $($candidates.Count)"
foreach ($candidate in $candidates) {
    Write-Output ($candidate.References -join ', ')
    if (-not $Apply) { continue }
    $currentContainers = @(Invoke-CleanupDocker -Arguments @('ps', '-aq'))
    if ($currentContainers.Count -gt 0) {
        $currentIds = @(Invoke-CleanupDocker -Arguments (@('inspect', '--format', '{{.Image}}') + $currentContainers))
        if ($currentIds -contains $candidate.Id) { continue }
    }
    foreach ($reference in $candidate.References) {
        if ($reference -eq $candidate.Id) {
            $currentDanglingIds = @(Invoke-CleanupDocker -Arguments @('image', 'ls', '--filter', 'dangling=true', '--no-trunc', '-q'))
            if ($currentDanglingIds -notcontains $candidate.Id) { continue }
        }
        $currentId = (Invoke-CleanupDocker -Arguments @('image', 'inspect', '--format', '{{.Id}}', $reference) | Out-String).Trim()
        if ($currentId -eq $candidate.Id) {
            Invoke-CleanupDocker -Arguments @('image', 'rm', $reference) | Out-Null
        }
    }
}
if ($PruneBuildCache) {
    if ($Apply) {
        Invoke-CleanupDocker -Arguments @('buildx', 'prune', '--force', '--filter', 'until=336h')
    } else {
        Write-Output 'Preview: prune dangling images and unused build cache older than 14 days.'
    }
}
