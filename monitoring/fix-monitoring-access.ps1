[CmdletBinding()]
param(
    [string]$RuntimeRoot,
    [switch]$PrepareOnly,
    [switch]$SkipFirewall,
    [switch]$SkipElevation
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '..\scripts\deploy-common.ps1')
Add-ExtensionDockerPath

$script:Sub2ApiContext = $null

if ([string]::IsNullOrWhiteSpace($RuntimeRoot)) {
    $RuntimeRoot = Get-ExtensionRuntimeRoot -Service 'monitoring'
}

# The normal entry point is intentionally elevated because it may update a
# ProgramData file and the Windows Firewall.  Deployment calls this script
# with -SkipElevation after its own UAC preflight.
if (-not $SkipElevation -and -not (Test-ExtensionAdministrator)) {
    $defaultRoot = Get-ExtensionRuntimeRoot -Service 'monitoring'
    if ([IO.Path]::GetFullPath($RuntimeRoot) -ne [IO.Path]::GetFullPath($defaultRoot)) {
        throw "自定义 RuntimeRoot 请在管理员 PowerShell 中运行。"
    }
    $elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath -Force
    if ($null -ne $elevatedExit) {
        exit [int]$elevatedExit
    }
}

$settingsPath = Join-Path $RuntimeRoot 'settings.env'
$composeEnvPath = Join-Path $RuntimeRoot '.env'
$databaseEnvPath = Join-Path $RuntimeRoot 'database.runtime.env'
$composePath = Join-Path $RuntimeRoot 'docker-compose.yml'

function Read-EnvFile {
    param([Parameter(Mandatory = $true)][string]$Path)

    $values = [ordered]@{}
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $values
    }
    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        $trimmed = $line.Trim()
        if ($trimmed -eq '' -or $trimmed.StartsWith('#')) {
            continue
        }
        $pair = $line -split '=', 2
        if ($pair.Count -eq 2) {
            $name = $pair[0].Trim()
            $value = $pair[1].Trim()
            if ($value.Length -ge 2 -and
                    (($value.StartsWith('"') -and $value.EndsWith('"')) -or
                        ($value.StartsWith("'") -and $value.EndsWith("'") -and $value -ne "'self'"))) {
                $value = $value.Substring(1, $value.Length - 2)
            }
            $values[$name] = $value
        }
    }
    return $values
}

function Get-LocalSub2ApiContext {
    if ($null -eq $script:Sub2ApiContext) {
        try {
            $script:Sub2ApiContext = Get-Sub2ApiDockerContext
        }
        catch {
            $script:Sub2ApiContext = $false
        }
    }
    if ($script:Sub2ApiContext -eq $false) {
        return $null
    }
    return $script:Sub2ApiContext
}

function Invoke-LocalSql {
    param([Parameter(Mandatory = $true)][string]$Query)

    $context = Get-LocalSub2ApiContext
    if ($null -eq $context) {
        return ''
    }
    try {
        $output = Invoke-ExtensionDocker -Arguments @(
            'exec', 'sub2api-postgres', 'psql',
            '-U', $context.PostgresUser,
            '-d', $context.PostgresDatabase,
            '-Atq', '-v', 'ON_ERROR_STOP=1', '-c', $Query
        ) -Capture
        $outputLines = @($output | ForEach-Object { [string]$_ })
        return (($outputLines -join [Environment]::NewLine).Trim())
    }
    catch {
        return ''
    }
}

function Get-LocalIPv4Addresses {
    $addresses = [Collections.Generic.List[string]]::new()

    # Prefer active physical interfaces with a default gateway.  Docker,
    # WSL, Hyper-V and VPN adapters otherwise tend to appear first.
    try {
        $preferred = @(
            Get-NetIPConfiguration -ErrorAction Stop |
                Where-Object {
                    $description = [string]$_.InterfaceDescription
                    $null -ne $_.IPv4DefaultGateway -and $_.IPv4Address -and
                        $description -notmatch '(?i)(virtual|hyper-v|docker|wsl|vmware|virtualbox|zerotier|tailscale|vpn|tap)'
                } |
                ForEach-Object { @($_.IPv4Address | ForEach-Object { [string]$_.IPAddress }) }
        )
        foreach ($address in $preferred) {
            if (-not $addresses.Contains([string]$address)) {
                $addresses.Add([string]$address)
            }
        }
    }
    catch {
    }

    if ($addresses.Count -eq 0) {
        try {
            $all = @(Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop |
                Where-Object {
                    $_.IPAddress -and
                        $_.IPAddress -notlike '127.*' -and
                        $_.IPAddress -notlike '169.254.*'
                } |
                Select-Object -ExpandProperty IPAddress)
            foreach ($address in $all) {
                if (-not $addresses.Contains([string]$address)) {
                    $addresses.Add([string]$address)
                }
            }
        }
        catch {
        }
    }

    if ($addresses.Count -eq 0) {
        try {
            $dnsAddresses = @([Net.Dns]::GetHostEntry([Net.Dns]::GetHostName()).AddressList |
                Where-Object {
                    $_.AddressFamily -eq [Net.Sockets.AddressFamily]::InterNetwork -and
                        -not ([string]$_).StartsWith('127.') -and
                        -not ([string]$_).StartsWith('169.254.')
                } |
                ForEach-Object { $_.ToString() })
            foreach ($address in $dnsAddresses) {
                if (-not $addresses.Contains([string]$address)) {
                    $addresses.Add([string]$address)
                }
            }
        }
        catch {
        }
    }

    $private = @($addresses | Where-Object {
        $parts = ([string]$_).Split('.')
        if ($parts.Count -ne 4) {
            return $false
        }
        $first = 0; $second = 0
        if (-not [int]::TryParse($parts[0], [ref]$first) -or
                -not [int]::TryParse($parts[1], [ref]$second)) {
            return $false
        }
        return $first -eq 10 -or
            ($first -eq 172 -and $second -ge 16 -and $second -le 31) -or
            ($first -eq 192 -and $second -eq 168)
    })
    if ($private.Count -gt 0) {
        return @($private)
    }
    return @($addresses)
}

function Get-Sub2ApiHostPort {
    if ($null -eq (Get-Command docker.exe -ErrorAction SilentlyContinue)) {
        return $null
    }
    try {
        $lines = @(& docker port sub2api 2>$null)
        if ($LASTEXITCODE -eq 0) {
            foreach ($line in $lines) {
                if ([string]$line -match ':(\d+)\s*$') {
                    return [int]$matches[1]
                }
            }
        }
    }
    catch {
    }
    return $null
}

function Parse-HttpUrl {
    param(
        [Parameter(Mandatory = $true)][string]$Value,
        [Parameter(Mandatory = $true)][string]$Field,
        [switch]$AllowPath
    )

    $uri = $null
    $text = $Value.Trim()
    if (-not [Uri]::TryCreate($text, [UriKind]::Absolute, [ref]$uri) -or
            $uri.Scheme -notin @('http', 'https') -or
            [string]::IsNullOrWhiteSpace($uri.Host) -or
            -not [string]::IsNullOrEmpty($uri.UserInfo) -or
            -not [string]::IsNullOrEmpty($uri.Query) -or
            -not [string]::IsNullOrEmpty($uri.Fragment) -or
            (-not $AllowPath -and $uri.AbsolutePath -ne '/')) {
        throw "$Field 必须是合法的 http(s) 地址。"
    }
    return $uri
}

function Get-Origin {
    param(
        [Parameter(Mandatory = $true)][string]$Value,
        [Parameter(Mandatory = $true)][string]$Field
    )

    return (Parse-HttpUrl $Value $Field -AllowPath).GetLeftPart([UriPartial]::Authority).ToLowerInvariant()
}

function Get-OriginTokens {
    param(
        [AllowNull()][string]$Value,
        [Parameter(Mandatory = $true)][string]$Field
    )

    if ([string]::IsNullOrWhiteSpace($Value)) {
        return @()
    }
    $raw = $Value.Trim()
    if ($raw.Length -ge 2 -and $raw.StartsWith('"') -and $raw.EndsWith('"')) {
        $raw = $raw.Substring(1, $raw.Length - 2)
    }
    $result = [Collections.Generic.List[string]]::new()
    foreach ($token in [regex]::Split($raw, '[,\s]+')) {
        if ([string]::IsNullOrWhiteSpace($token)) {
            continue
        }
        if ($token -eq "'self'") {
            if (-not $result.Contains("'self'")) {
                $result.Add("'self'")
            }
            continue
        }
        if ($token.Contains('*')) {
            throw "$Field 不允许使用通配符 *。"
        }
        $origin = Get-Origin $token $Field
        if (-not $result.Contains($origin)) {
            $result.Add($origin)
        }
    }
    return @($result)
}

function Get-ConfiguredSub2ApiOrigins {
    $origins = [Collections.Generic.List[string]]::new()
    $query = "SELECT value FROM settings WHERE key IN ('frontend_url', 'api_base_url') AND btrim(value) <> '' ORDER BY CASE key WHEN 'frontend_url' THEN 0 ELSE 1 END;"
    $raw = Invoke-LocalSql -Query $query
    foreach ($value in @($raw -split "`r?`n")) {
        if ([string]::IsNullOrWhiteSpace($value)) {
            continue
        }
        try {
            $origin = Get-Origin $value 'Sub2API settings'
            if (-not $origins.Contains($origin)) {
                $origins.Add($origin)
            }
        }
        catch {
            Write-Warning '忽略 Sub2API 中无效的 frontend_url/api_base_url。'
        }
    }
    return @($origins)
}

function Add-Origin {
    param(
        [Parameter(Mandatory = $true)][Collections.Generic.List[string]]$List,
        [Parameter(Mandatory = $true)][string]$Value,
        [Parameter(Mandatory = $true)][string]$Field
    )

    try {
        $origin = Get-Origin $Value $Field
        if (-not $List.Contains($origin)) {
            $List.Add($origin)
        }
    }
    catch {
        Write-Warning "忽略无效的 $Field。"
    }
}

function Merge-FrameAncestors {
    param(
        [AllowNull()][string]$Existing,
        [Parameter(Mandatory = $true)][string[]]$AutoOrigins
    )

    $result = [Collections.Generic.List[string]]::new()
    $result.Add("'self'")
    foreach ($token in @(Get-OriginTokens $Existing 'MONITORING_FRAME_ANCESTORS')) {
        if (-not $result.Contains($token)) {
            $result.Add($token)
        }
    }
    foreach ($origin in $AutoOrigins) {
        if (-not [string]::IsNullOrWhiteSpace($origin) -and -not $result.Contains($origin)) {
            $result.Add($origin)
        }
    }
    return ($result -join ' ')
}

function Set-FrameAncestorsFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Value
    )

    $lines = [Collections.Generic.List[string]]::new()
    $found = $false
    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        if ($line -match '^\s*MONITORING_FRAME_ANCESTORS\s*=') {
            if (-not $found) {
                $lines.Add("MONITORING_FRAME_ANCESTORS=$Value")
                $found = $true
            }
        }
        else {
            $lines.Add($line)
        }
    }
    if (-not $found) {
        $lines.Add("MONITORING_FRAME_ANCESTORS=$Value")
    }

    $temp = "$Path.$([Guid]::NewGuid().ToString('N')).tmp"
    try {
        [IO.File]::WriteAllLines($temp, $lines, [Text.UTF8Encoding]::new($false))
        Move-Item -LiteralPath $temp -Destination $Path -Force
    }
    finally {
        if (Test-Path -LiteralPath $temp -PathType Leaf) {
            Remove-Item -LiteralPath $temp -Force -ErrorAction SilentlyContinue
        }
    }
}

function Restart-Monitoring {
    param([Parameter(Mandatory = $true)][string]$Root)

    if (-not (Test-Path -LiteralPath (Join-Path $Root 'docker-compose.yml') -PathType Leaf)) {
        throw '未找到监控 Compose 文件，请先运行 monitoring\deploy.bat。'
    }
    Push-Location $Root
    try {
        Invoke-ExtensionDocker -Arguments @('compose', 'up', '-d', '--remove-orphans')
    }
    finally {
        Pop-Location
    }
    Wait-ExtensionContainer -Name 'sub2api-monitoring'
}

function Test-MonitoringPortBinding {
    param(
        [Parameter(Mandatory = $true)]$Inspect,
        [Parameter(Mandatory = $true)][string]$BindHost,
        [Parameter(Mandatory = $true)][int]$Port
    )

    try {
        $bindings = $Inspect.HostConfig.PortBindings
        $property = $bindings.PSObject.Properties['8090/tcp']
        if ($null -eq $property) {
            return $false
        }
        $entries = @($property.Value)
        if ($entries.Count -ne 1) {
            return $false
        }
        $entry = $entries[0]
        if ([string]$entry.HostPort -ne [string]$Port) {
            return $false
        }
        $hostIP = ([string]$entry.HostIp).Trim()
        if ($BindHost -eq '0.0.0.0') {
            return [string]::IsNullOrWhiteSpace($hostIP) -or $hostIP -eq '0.0.0.0'
        }
        return $hostIP -eq $BindHost
    }
    catch {
        return $false
    }
}

function Get-ContainerEnvironmentValue {
    param(
        [Parameter(Mandatory = $true)]$Inspect,
        [Parameter(Mandatory = $true)][string]$Name
    )

    foreach ($entry in @($Inspect.Config.Env)) {
        $pair = ([string]$entry) -split '=', 2
        if ($pair.Count -eq 2 -and $pair[0] -eq $Name) {
            return [string]$pair[1]
        }
    }
    return $null
}

function Test-MonitoringHttp {
    param(
        [Parameter(Mandatory = $true)][string]$BindHost,
        [Parameter(Mandatory = $true)][int]$Port
    )

    $healthHost = if ([Net.IPAddress]::IsLoopback([Net.IPAddress]$BindHost)) {
        '127.0.0.1'
    }
    elseif ($BindHost -eq '0.0.0.0') {
        '127.0.0.1'
    }
    else {
        $BindHost
    }
    $uri = 'http://{0}:{1}/healthz' -f $healthHost, $Port
    try {
        $response = Invoke-WebRequest -UseBasicParsing -Uri $uri -TimeoutSec 10
        if ([int]$response.StatusCode -ne 200) {
            throw "HTTP $($response.StatusCode)"
        }
    }
    catch {
        throw "监控健康检查失败：$uri。请检查容器日志、端口和防火墙。"
    }
}

if (-not (Test-Path -LiteralPath $settingsPath -PathType Leaf)) {
    throw "未找到 $settingsPath，请先运行 monitoring\deploy.bat。"
}
if (-not (Test-Path -LiteralPath $composeEnvPath -PathType Leaf)) {
    throw "未找到 $composeEnvPath，请先运行 monitoring\deploy.bat。"
}
if (-not (Test-Path -LiteralPath $databaseEnvPath -PathType Leaf)) {
    throw "未找到 $databaseEnvPath，请先运行 monitoring\deploy.bat。"
}
if (-not (Test-Path -LiteralPath $composePath -PathType Leaf)) {
    throw "未找到 $composePath，请先运行 monitoring\deploy.bat。"
}

Assert-ExtensionDocker

$runtimeValues = Read-EnvFile -Path $settingsPath
$composeValues = Read-EnvFile -Path $composeEnvPath

$bindHost = if ($composeValues.Contains('MONITORING_BIND_HOST')) {
    [string]$composeValues['MONITORING_BIND_HOST']
}
elseif ($runtimeValues.Contains('MONITORING_BIND_HOST')) {
    [string]$runtimeValues['MONITORING_BIND_HOST']
}
else {
    '0.0.0.0'
}
$bindHost = $bindHost.Trim()
if ($bindHost -eq 'localhost') {
    $bindHost = '127.0.0.1'
}
[Net.IPAddress]$bindAddress = $null
if ($bindHost -notmatch '^\d{1,3}(\.\d{1,3}){3}$' -or
        -not [Net.IPAddress]::TryParse($bindHost, [ref]$bindAddress) -or
        $bindAddress.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) {
    throw "监控绑定地址无效：$bindHost。请检查 $composeEnvPath。"
}
$bindHost = $bindAddress.ToString()
$lanBind = -not [Net.IPAddress]::IsLoopback($bindAddress)

$portValue = if ($composeValues.Contains('MONITORING_PORT')) {
    [string]$composeValues['MONITORING_PORT']
}
else {
    '18090'
}
$port = 0
if (-not [int]::TryParse($portValue.Trim(), [ref]$port) -or $port -lt 1 -or $port -gt 65535) {
    throw "监控端口无效：$portValue。请检查 $composeEnvPath。"
}

$localIPs = @(Get-LocalIPv4Addresses)
$autoOrigins = [Collections.Generic.List[string]]::new()
foreach ($origin in @(Get-ConfiguredSub2ApiOrigins)) {
    if (-not $autoOrigins.Contains($origin)) {
        $autoOrigins.Add($origin)
    }
}

# Add the addresses that can be used on this host as well.  This covers an
# empty or stale Sub2API URL setting on machines without a public domain.
$sub2apiPort = Get-Sub2ApiHostPort
if ($null -ne $sub2apiPort) {
    foreach ($ip in $localIPs) {
        Add-Origin -List $autoOrigins -Value ('http://{0}:{1}' -f $ip, $sub2apiPort) -Field '本机 Sub2API 地址'
    }
    Add-Origin -List $autoOrigins -Value ('http://127.0.0.1:{0}' -f $sub2apiPort) -Field '本机 Sub2API 地址'
    Add-Origin -List $autoOrigins -Value ('http://localhost:{0}' -f $sub2apiPort) -Field '本机 Sub2API 地址'
}

$oldFrame = [string]$runtimeValues['MONITORING_FRAME_ANCESTORS']
$newFrame = Merge-FrameAncestors -Existing $oldFrame -AutoOrigins @($autoOrigins)
$frameChanged = $oldFrame.Trim() -ne $newFrame.Trim()

Write-Host ''
Write-Host '监控访问修复计划：' -ForegroundColor Cyan
Write-Host "  绑定地址：$bindHost`:$port"
if ($autoOrigins.Count -gt 0) {
    Write-Host "  自动识别的 Sub2API 来源：$($autoOrigins -join ', ')"
}
else {
    Write-Warning '没有识别到 Sub2API 的访问来源；请把实际打开面板的来源手动写入 MONITORING_FRAME_ANCESTORS。'
}
Write-Host "  iframe 来源：$newFrame"

if ($frameChanged) {
    Set-FrameAncestorsFile -Path $settingsPath -Value $newFrame
    if ($PrepareOnly) {
        Write-Host '准备模式：已更新 MONITORING_FRAME_ANCESTORS（部署流程随后启动容器）。' -ForegroundColor Yellow
    }
    else {
        Write-Host '已更新监控 iframe 白名单。' -ForegroundColor Green
    }
}
else {
    Write-Host 'iframe 白名单已包含当前来源。' -ForegroundColor DarkGray
}

if ($lanBind) {
    if ($localIPs.Count -gt 0) {
        Write-Host "  局域网地址：$((@($localIPs | ForEach-Object { 'http://{0}:{1}' -f $_, $port }) -join ', '))"
    }
    else {
        Write-Warning '未找到可用的局域网 IPv4 地址。'
    }
}
else {
    Write-Warning '监控当前只绑定 127.0.0.1，其他电脑无法访问；如需局域网访问，请在 .env 设置 MONITORING_BIND_HOST=0.0.0.0 后重新部署。'
}

if ($PrepareOnly) {
    Write-Host '监控访问修复准备完成。' -ForegroundColor Green
    return
}

if (-not $SkipFirewall -and $lanBind) {
    if (-not (Test-ExtensionLanFirewallRule -Port $port -Enabled $true)) {
        if (-not (Set-ExtensionLanFirewallRule -Port $port -Enabled $true) -or
                -not (Test-ExtensionLanFirewallRule -Port $port -Enabled $true)) {
            throw "无法配置监控 TCP $port 的 Windows 防火墙规则。"
        }
    }
}

$needsRestart = $frameChanged
if (-not (Test-ExtensionContainerExists -Name 'sub2api-monitoring')) {
    $needsRestart = $true
}
else {
    try {
        $inspect = Get-ExtensionContainerInspect -Name 'sub2api-monitoring'
        if (-not $inspect.State.Running) {
            $needsRestart = $true
        }
        elseif (-not (Test-MonitoringPortBinding -Inspect $inspect -BindHost $bindHost -Port $port)) {
            $needsRestart = $true
        }
        elseif ((Get-ContainerEnvironmentValue -Inspect $inspect -Name 'MONITORING_FRAME_ANCESTORS') -ne $newFrame) {
            $needsRestart = $true
        }
        else {
            $health = $inspect.State.PSObject.Properties['Health']
            if ($null -ne $health -and $health.Value.Status -eq 'unhealthy') {
                $needsRestart = $true
            }
        }
    }
    catch {
        $needsRestart = $true
    }
}

if ($needsRestart) {
    Restart-Monitoring -Root $RuntimeRoot
}
else {
    Wait-ExtensionContainer -Name 'sub2api-monitoring'
}
Test-MonitoringHttp -BindHost $bindHost -Port $port
Write-Host '监控访问修复完成，健康检查通过。' -ForegroundColor Green
