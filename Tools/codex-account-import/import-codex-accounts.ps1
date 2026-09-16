param(
    [Parameter(Mandatory = $true)]
    [string]$ConfigPath,

    [string]$InputPath,

    [switch]$WhatIf
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Get-JsonProperty {
    param(
        [Parameter(Mandatory = $true)]$Object,
        [Parameter(Mandatory = $true)][string]$Name,
        $Default = $null
    )

    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $Default
    }
    return $property.Value
}

function Read-DotEnv {
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "找不到 Sub2API 环境文件: $Path"
    }

    $values = @{}
    foreach ($line in Get-Content -LiteralPath $Path -Encoding UTF8) {
        if ($line -notmatch '^([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
            continue
        }
        $value = $matches[2].Trim()
        if ($value.Length -ge 2 -and (($value[0] -eq '"' -and $value[-1] -eq '"') -or ($value[0] -eq "'" -and $value[-1] -eq "'"))) {
            $value = $value.Substring(1, $value.Length - 2)
        }
        $values[$matches[1]] = $value
    }
    return $values
}

function Get-ApiData {
    param([Parameter(Mandatory = $true)]$Response)

    $code = $Response.PSObject.Properties["code"]
    if ($null -ne $code -and [int]$code.Value -ne 0) {
        throw "Sub2API 返回错误，code=$($code.Value)；服务器响应未输出，以免泄露凭据"
    }
    $data = $Response.PSObject.Properties["data"]
    if ($null -ne $data) {
        return $data.Value
    }
    return $Response
}

function Invoke-Sub2Api {
    param(
        [Parameter(Mandatory = $true)][string]$Method,
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$BaseUrl,
        [string]$Token,
        $Body = $null
    )

    $request = @{
        Uri         = $BaseUrl.TrimEnd('/') + "/api/v1" + $Path
        Method      = $Method
        TimeoutSec  = 120
        ErrorAction = "Stop"
    }
    if ($Token) {
        $request.Headers = @{ Authorization = "Bearer $Token" }
    }
    if ($null -ne $Body) {
        $request.ContentType = "application/json"
        $request.Body = $Body | ConvertTo-Json -Depth 50 -Compress
    }

    try {
        $response = Invoke-RestMethod @request
    }
    catch {
        $statusCode = $null
        if ($null -ne $_.Exception.Response -and $null -ne $_.Exception.Response.StatusCode) {
            $statusCode = [int]$_.Exception.Response.StatusCode
        }
        $status = if ($null -ne $statusCode) { "，HTTP $statusCode" } else { "" }
        throw "请求 Sub2API $Method $Path 失败$status；服务器响应未输出，以免泄露凭据"
    }
    return Get-ApiData $response
}

function Resolve-UniqueByName {
    param(
        [Parameter(Mandatory = $true)][object[]]$Items,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Label
    )

    $matches = @($Items | Where-Object { [string]::Equals([string]$_.name, $Name, [StringComparison]::OrdinalIgnoreCase) })
    if ($matches.Count -ne 1) {
        throw "$Label '$Name' 匹配到 $($matches.Count) 条记录，请使用唯一名称"
    }
    return $matches[0]
}

$resolvedConfigPath = (Resolve-Path -LiteralPath $ConfigPath).Path
$configDirectory = Split-Path -Parent $resolvedConfigPath
$config = Get-Content -LiteralPath $resolvedConfigPath -Raw -Encoding UTF8 | ConvertFrom-Json

$runtimeEnvPath = [string](Get-JsonProperty $config "runtime_env_path" "C:\ProgramData\Sub2API\runtime\.env")
$runtime = Read-DotEnv $runtimeEnvPath
if (-not $runtime.ContainsKey("ADMIN_EMAIL") -or -not $runtime.ContainsKey("ADMIN_PASSWORD")) {
    throw "Sub2API 环境文件缺少 ADMIN_EMAIL 或 ADMIN_PASSWORD"
}

$baseUrl = [string](Get-JsonProperty $config "sub2api_url" "")
if (-not $baseUrl) {
    $port = if ($runtime.ContainsKey("SERVER_PORT")) { $runtime["SERVER_PORT"] } else { "18080" }
    $baseUrl = "http://127.0.0.1:$port"
}
$baseUri = $null
if (-not [Uri]::TryCreate($baseUrl, [UriKind]::Absolute, [ref]$baseUri) -or
    ($baseUri.Scheme -ne "http" -and $baseUri.Scheme -ne "https")) {
    throw "sub2api_url 必须是有效的 http/https 地址"
}
if (-not $baseUri.IsLoopback) {
    throw "sub2api_url 只允许本机回环地址，避免把管理员密码发送到其他主机"
}
if ($baseUri.UserInfo -or $baseUri.Query -or $baseUri.Fragment) {
    throw "sub2api_url 不能包含用户信息、查询参数或片段"
}
$baseUrl = $baseUrl.TrimEnd('/')

$configuredInputPath = [string](Get-JsonProperty $config "input_path" "")
$inputPathFromArgument = -not [string]::IsNullOrWhiteSpace($InputPath)
$inputPath = if ($inputPathFromArgument) { $InputPath } else { $configuredInputPath }
if ([string]::IsNullOrWhiteSpace($inputPath)) {
    throw "未提供账号 JSON；请拖入 BAT，或使用 -InputPath，或在配置中填写 input_path"
}
if (-not [IO.Path]::IsPathRooted($inputPath)) {
    $inputBasePath = if ($inputPathFromArgument) { (Get-Location).Path } else { $configDirectory }
    $inputPath = [IO.Path]::GetFullPath((Join-Path $inputBasePath $inputPath))
}
if (-not (Test-Path -LiteralPath $inputPath -PathType Leaf)) {
    throw "找不到账号 JSON: $inputPath"
}

$source = Get-Content -LiteralPath $inputPath -Raw -Encoding UTF8 | ConvertFrom-Json
$sourceAccounts = Get-JsonProperty $source "accounts" $null
if ($null -ne $sourceAccounts) {
    $records = @($sourceAccounts | ForEach-Object { Get-JsonProperty $_ "credentials" $null } | Where-Object { $null -ne $_ })
}
else {
    $records = @($source)
}
$sourceType = [string](Get-JsonProperty $config "source_type" "codex")
$records = @($records | Where-Object {
    $recordType = [string](Get-JsonProperty $_ "type" "")
    -not $sourceType -or [string]::Equals($recordType, $sourceType, [StringComparison]::OrdinalIgnoreCase)
})
if ($records.Count -eq 0) {
    throw "账号 JSON 中没有符合 source_type='$sourceType' 的记录"
}
foreach ($record in $records) {
    if (-not (Get-JsonProperty $record "access_token" "") -and -not (Get-JsonProperty $record "accessToken" "")) {
        throw "账号 JSON 中存在缺少 access_token/accessToken 的记录"
    }
}

$login = Invoke-Sub2Api -Method Post -Path "/auth/login" -BaseUrl $baseUrl -Body @{
    email    = $runtime["ADMIN_EMAIL"]
    password = $runtime["ADMIN_PASSWORD"]
}
$token = [string](Get-JsonProperty $login "access_token" "")
if (-not $token) {
    throw "管理员登录未返回 access_token；如已启用二次验证，请先在 Sub2API 中完成登录"
}

$groups = @(Invoke-Sub2Api -Method Get -Path "/admin/groups/all?include_inactive=true" -BaseUrl $baseUrl -Token $token)
$groupNames = @(Get-JsonProperty $config "group_names" @())
$groupIds = @()
foreach ($groupName in $groupNames) {
    $group = Resolve-UniqueByName -Items $groups -Name ([string]$groupName) -Label "分组"
    if ($group.platform -ne "openai" -or $group.status -ne "active") {
        throw "分组 '$groupName' 必须是启用状态的 OpenAI 分组"
    }
    $groupIds += [int64]$group.id
}

$proxyId = $null
$proxyName = [string](Get-JsonProperty $config "proxy_name" "")
if ($proxyName) {
    $proxies = @(Invoke-Sub2Api -Method Get -Path "/admin/proxies/all" -BaseUrl $baseUrl -Token $token)
    $proxy = Resolve-UniqueByName -Items $proxies -Name $proxyName -Label "代理"
    if ($proxy.status -ne "active") {
        throw "代理 '$proxyName' 当前未启用"
    }
    $proxyId = [int64]$proxy.id
}

$concurrency = [int](Get-JsonProperty $config "concurrency" 3)
$priority = [int](Get-JsonProperty $config "priority" 50)
$rateMultiplier = [double](Get-JsonProperty $config "rate_multiplier" 1)
if ($concurrency -lt 1) { throw "concurrency 必须大于 0" }
if ($priority -lt 1) { throw "priority 必须大于 0" }
if ($rateMultiplier -lt 0) { throw "rate_multiplier 不能小于 0" }

$loadFactor = Get-JsonProperty $config "load_factor" $null
if ($null -ne $loadFactor) {
    $loadFactor = [int]$loadFactor
    if ($loadFactor -lt 1) { throw "load_factor 必须大于 0，或设为 null 使用默认值" }
}

$namePrefix = [string](Get-JsonProperty $config "name_prefix" "")
$nameStart = [int](Get-JsonProperty $config "name_start" 1)
$nameWidth = [int](Get-JsonProperty $config "name_width" 3)
if ($nameStart -lt 0) { throw "name_start 不能小于 0" }
if ($nameWidth -lt 1 -or $nameWidth -gt 99) { throw "name_width 必须在 1 到 99 之间" }
if ($nameStart -gt [int]::MaxValue - ($records.Count - 1)) { throw "name_start 加账号数量超出整数范围" }
$extra = Get-JsonProperty $config "extra" ([pscustomobject]@{})
$created = 0
$updated = 0
$skipped = 0

for ($index = 0; $index -lt $records.Count; $index++) {
    $record = $records[$index]
    $name = if ($namePrefix) {
        $namePrefix + ($nameStart + $index).ToString("D$nameWidth")
    }
    else {
        $candidate = ([string](Get-JsonProperty $record "email" "")).Trim()
        if (-not $candidate) {
            throw "第 $($index + 1) 个账号缺少 email，无法按邮箱命名"
        }
        $candidate
    }

    $payload = @{
        content               = $record | ConvertTo-Json -Depth 50 -Compress
        name                  = $name
        notes                 = [string](Get-JsonProperty $config "notes" "")
        proxy_id              = $proxyId
        concurrency           = $concurrency
        priority              = $priority
        rate_multiplier       = $rateMultiplier
        group_ids             = $groupIds
        auto_pause_on_expired = [bool](Get-JsonProperty $config "auto_pause_on_expired" $false)
        extra                 = $extra
        update_existing       = $true
    }
    if ($null -ne $loadFactor) { $payload.load_factor = [int]$loadFactor }
    $expiresAt = [string](Get-JsonProperty $config "expires_at" "")
    if ($expiresAt) {
        [void][DateTimeOffset]::Parse($expiresAt)
        $payload.expires_at = $expiresAt
    }

    if ($WhatIf) {
        continue
    }

    $result = Invoke-Sub2Api -Method Post -Path "/admin/accounts/import/codex-session" -BaseUrl $baseUrl -Token $token -Body $payload
    $created += [int](Get-JsonProperty $result "created" 0)
    $updated += [int](Get-JsonProperty $result "updated" 0)
    $skipped += [int](Get-JsonProperty $result "skipped" 0)
    if ([int](Get-JsonProperty $result "failed" 0) -gt 0) {
        throw "第 $($index + 1) 个账号导入失败；凭据内容未输出，请在 Sub2API 操作日志中查看请求结果"
    }
}

if ($WhatIf) {
    $groupSummary = if ($groupNames.Count) { $groupNames -join ", " } else { "无" }
    $proxySummary = if ($proxyName) { $proxyName } else { "直连" }
    $nameSummary = if ($namePrefix) { "前缀 $namePrefix" } else { "邮箱" }
    Write-Host "校验通过：$($records.Count) 个账号；命名=$nameSummary；分组=$groupSummary；代理=$proxySummary；并发=$concurrency；优先级=$priority；倍率=$rateMultiplier"
}
else {
    Write-Host "导入完成：创建 $created，更新 $updated，跳过 $skipped"
}
