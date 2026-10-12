Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$cleanupScript = Join-Path (Split-Path -Parent $PSScriptRoot) 'cleanup-docker.ps1'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) "sub2api-cleanup-execution-$([guid]::NewGuid())"
$previousProgramData = $env:ProgramData
$previousPath = $env:Path
$configuredId = 'sha256:' + ('a' * 64)
$backupId = 'sha256:' + ('b' * 64)
$unusedId = 'sha256:' + ('c' * 64)
$testState = @{ Calls = [Collections.Generic.List[object]]::new(); MissingReference = ''; UntaggedConfigured = $false; DigestOnlyUnused = $false; References = @{
    $configuredId = $configuredId
    'ghcr.io/wei-shaw/sub2api@sha256:configured' = $configuredId
    'sub2api/local-backup:keep' = $backupId
    'ghcr.io/wei-shaw/sub2api:old-configured' = $configuredId
    'ghcr.io/wei-shaw/sub2api:old-backup' = $backupId
    'sub2api-ext-monitoring:test-old' = $unusedId
    $unusedId = $unusedId
} }
$dockerStub = {
    $arguments = @($args)
    $testState.Calls.Add($arguments)
    $global:LASTEXITCODE = 0
    if ($arguments[0] -eq 'info') { return 'test-server' }
    if ($arguments[0] -eq 'ps') { return }
    if ($arguments[0] -eq 'image' -and $arguments[1] -eq 'ls') {
        if ($arguments -contains 'dangling=true') {
            if ($testState.UntaggedConfigured) { return $configuredId }
            return
        }
        foreach ($image in @(
            @{ ID = $configuredId; Repository = 'ghcr.io/wei-shaw/sub2api'; Tag = 'old-configured' },
            @{ ID = $backupId; Repository = 'ghcr.io/wei-shaw/sub2api'; Tag = 'old-backup' },
            @{ ID = $unusedId; Repository = 'sub2api-ext-monitoring'; Tag = 'test-old' }
        )) {
            if (($image.ID -eq $configuredId -and $testState.UntaggedConfigured) -or
                ($image.ID -eq $unusedId -and $testState.DigestOnlyUnused)) {
                $image.Repository = '<none>'
                $image.Tag = '<none>'
            }
            $image.CreatedAt = '2026-08-01 00:00:00 +0000 UTC'
            $image | ConvertTo-Json -Compress
        }
        return
    }
    if ($arguments[0] -eq 'image' -and $arguments[1] -eq 'inspect') {
        foreach ($reference in $arguments[4..($arguments.Count - 1)]) {
            if ($reference -eq $testState.MissingReference) {
                $global:LASTEXITCODE = 1
                return 'Simulated missing image reference.'
            }
            $testState.References[$reference]
        }
        return
    }
    if ($arguments[0] -eq 'image' -and $arguments[1] -eq 'rm') { return }
    if ($arguments[0] -eq 'buildx' -and $arguments[1] -eq 'prune') { return }
    throw "Unexpected Docker command in cleanup execution test: $($arguments -join ' ')"
}.GetNewClosure()
Set-Item -Path Function:docker -Value $dockerStub
function Assert-ExecutionTest {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw $Message }
}
try {
    $env:ProgramData = $testRoot
    $runtimeRoot = Join-Path $testRoot 'Sub2API\runtime'
    $backupRoot = Join-Path $testRoot 'Sub2API\backups\keep'
    New-Item -ItemType Directory -Path $runtimeRoot,$backupRoot -Force | Out-Null
    '{"BackupImageTag":"sub2api/local-backup:keep"}' | Set-Content -LiteralPath (Join-Path $backupRoot 'manifest.json')
    foreach ($reference in @($configuredId, 'ghcr.io/wei-shaw/sub2api@sha256:configured')) {
        "SUB2API_IMAGE=$reference" | Set-Content -LiteralPath (Join-Path $runtimeRoot '.env')
        $testState.Calls.Clear()
        & $cleanupScript -Apply | Out-Null
        $deletions = @($testState.Calls | Where-Object { $_[0] -eq 'image' -and $_[1] -eq 'rm' })
        Assert-ExecutionTest ($deletions.Count -eq 1 -and $deletions[0][2] -eq 'sub2api-ext-monitoring:test-old') 'Configured image IDs/digests and backup aliases must be resolved to protected IDs before cleanup.'
    }
    $testState.UntaggedConfigured = $true
    $testState.Calls.Clear()
    & $cleanupScript -Apply -PruneBuildCache | Out-Null
    $deletions = @($testState.Calls | Where-Object { $_[0] -eq 'image' -and $_[1] -eq 'rm' })
    Assert-ExecutionTest ($deletions.Count -eq 1 -and $deletions[0][2] -eq 'sub2api-ext-monitoring:test-old') 'Cache cleanup must preserve an untagged configured image.'
    $testState.DigestOnlyUnused = $true
    $testState.Calls.Clear()
    & $cleanupScript -Apply -PruneBuildCache | Out-Null
    Assert-ExecutionTest (@($testState.Calls | Where-Object { $_[0] -eq 'image' -and $_[1] -eq 'rm' }).Count -eq 0) 'Cache cleanup must retain digest-only images that Docker does not classify as dangling.'
    $testState.DigestOnlyUnused = $false
    foreach ($missing in @($configuredId, 'sub2api/local-backup:keep')) {
        "SUB2API_IMAGE=$configuredId" | Set-Content -LiteralPath (Join-Path $runtimeRoot '.env')
        $testState.MissingReference = $missing
        $testState.Calls.Clear()
        $failed = $false
        try { & $cleanupScript -Apply | Out-Null } catch { $failed = $true }
        $deletions = @($testState.Calls | Where-Object { $_[0] -eq 'image' -and $_[1] -eq 'rm' })
        Assert-ExecutionTest ($failed -and $deletions.Count -eq 0) 'Missing configured or backup images must stop cleanup before any deletion.'
    }
    $testState.MissingReference = ''
    $testState.Calls.Clear()
    & $cleanupScript | Out-Null
    Assert-ExecutionTest (@($testState.Calls | Where-Object { $_[0] -eq 'image' -and $_[1] -eq 'rm' }).Count -eq 0) 'Preview must not delete images.'
    Write-Host 'PASS: ID/digest and backup alias protection, missing-reference failures and preview.'
} finally {
    $env:ProgramData = $previousProgramData
    $env:Path = $previousPath
    $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if (-not $resolvedRoot.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase)) { throw 'Test cleanup path escaped the temporary directory.' }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
