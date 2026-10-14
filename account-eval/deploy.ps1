[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '..\scripts\deploy-common.ps1')
. (Join-Path $PSScriptRoot '..\deploy\windows\modules\Common.ps1')

$runtimeRoot = Get-ExtensionRuntimeRoot -Service 'account-eval'
$elevatedExit = Invoke-ExtensionElevated -ScriptPath $PSCommandPath -ProbePath $runtimeRoot
if ($null -ne $elevatedExit) { exit [int]$elevatedExit }

Assert-ExtensionDocker
$sub2api = Get-Sub2ApiDockerContext
$mainContext = New-Sub2ApiContext
if (-not (Test-Sub2ApiDeploymentExists -Context $mainContext) -or
        -not (Test-Sub2ApiPolicyGatewayEnabled -Context $mainContext) -or
        -not (Test-ExtensionContainerExists -Name 'sub2api-policy-gateway')) {
    throw '请先运行根目录的一键部署，测评需要主站网关提供 /account-eval/ 同源入口。'
}

function Get-EvalPublicUrl {
    param([System.Collections.IDictionary]$Existing)

    if ($Existing['EVAL_PUBLIC_URL']) {
        $value = [string]$Existing['EVAL_PUBLIC_URL']
    } else {
        $rows = Invoke-ExtensionDocker -Arguments @(
            'exec', 'sub2api-postgres', 'psql', '-U', $sub2api.PostgresUser,
            '-d', $sub2api.PostgresDatabase, '-Atq', '-v', 'ON_ERROR_STOP=1', '-c',
            "SELECT value FROM settings WHERE key IN ('frontend_url','api_base_url') AND btrim(value) <> '' ORDER BY CASE key WHEN 'frontend_url' THEN 0 ELSE 1 END;"
        ) -Capture
        foreach ($row in $rows) {
            $candidate = $null
            if ([Uri]::TryCreate(([string]$row).Trim(), [UriKind]::Absolute, [ref]$candidate) -and
                    $candidate.Scheme -in @('http', 'https') -and -not $candidate.IsLoopback -and
                    [string]::IsNullOrEmpty($candidate.UserInfo)) {
                return $candidate.GetLeftPart([UriPartial]::Authority) + '/account-eval/'
            }
        }
        $gateway = Get-ExtensionContainerInspect -Name 'sub2api-policy-gateway'
        $bindings = @($gateway.NetworkSettings.Ports.'8080/tcp')
        if ($bindings.Count -eq 0 -or $null -eq $bindings[0]) {
            throw '无法识别主站网关端口，请在 account-eval\.env 配置 EVAL_PUBLIC_URL。'
        }
        $hostName = 'localhost'
        $configs = @(Get-NetIPConfiguration -ErrorAction Stop | Where-Object {
            $null -ne $_.IPv4DefaultGateway -and $null -ne $_.IPv4Address
        })
        foreach ($config in $configs) {
            $addresses = @($config.IPv4Address | Where-Object {
                $_.IPAddress -notlike '169.254.*' -and $_.IPAddress -ne '127.0.0.1'
            })
            if ($addresses.Count -gt 0) { $hostName = [string]$addresses[0].IPAddress; break }
        }
        $value = 'http://{0}:{1}/account-eval/' -f $hostName, $bindings[0].HostPort
    }
    $uri = $null
    if (-not [Uri]::TryCreate($value, [UriKind]::Absolute, [ref]$uri) -or
            $uri.Scheme -notin @('http', 'https') -or $uri.AbsolutePath -ne '/account-eval/' -or
            -not [string]::IsNullOrEmpty($uri.UserInfo) -or
            -not [string]::IsNullOrEmpty($uri.Query) -or -not [string]::IsNullOrEmpty($uri.Fragment)) {
        throw 'EVAL_PUBLIC_URL 必须是主站 http(s) 地址加 /account-eval/，不能含账号、参数或片段。'
    }
    return $uri.AbsoluteUri
}

$existing = Read-ExtensionEnvFile -Path (Join-Path $runtimeRoot '.env')
$publicUrl = Get-EvalPublicUrl -Existing $existing
Build-ExtensionImage -Context $PSScriptRoot -Dockerfile (Join-Path $PSScriptRoot 'Dockerfile') `
    -Image 'sub2api-ext-account-eval:local' -Containers @('sub2api-account-eval')

New-Item -ItemType Directory -Path $runtimeRoot -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'compose.runtime.yml') -Destination (Join-Path $runtimeRoot 'docker-compose.yml') -Force
Copy-Item -LiteralPath (Join-Path $PSScriptRoot '..\scripts\manage-runtime.ps1') -Destination (Join-Path $runtimeRoot 'manage.ps1') -Force
Copy-Item -LiteralPath (Join-Path $PSScriptRoot '..\scripts\manage-runtime.bat') -Destination (Join-Path $runtimeRoot 'manage.bat') -Force
$settingsPath = Join-Path $runtimeRoot 'settings.env'
if (-not (Test-Path -LiteralPath $settingsPath -PathType Leaf)) {
    $localSettings = Join-Path $PSScriptRoot 'settings.env'
    $source = if (Test-Path -LiteralPath $localSettings -PathType Leaf) { $localSettings } else { Join-Path $PSScriptRoot 'settings.env.example' }
    Copy-Item -LiteralPath $source -Destination $settingsPath
}
$composeValues = [ordered]@{ SUB2API_NETWORK = $sub2api.Network }
if ($existing['EVAL_PUBLIC_URL']) { $composeValues['EVAL_PUBLIC_URL'] = [string]$existing['EVAL_PUBLIC_URL'] }
Write-ExtensionEnvFile -Path (Join-Path $runtimeRoot '.env') -Values $composeValues
Write-ExtensionEnvFile -Path (Join-Path $runtimeRoot 'database.runtime.env') -Values ([ordered]@{
    PGHOST = $sub2api.PostgresHost
    PGPORT = $sub2api.PostgresPort
    PGUSER = $sub2api.PostgresUser
    PGPASSWORD = $sub2api.PostgresPassword
    PGDATABASE = $sub2api.PostgresDatabase
    PGSSLMODE = 'disable'
    PGOPTIONS = '-c default_transaction_read_only=on'
})
Grant-ExtensionRuntimeAccess -Path $runtimeRoot -SensitiveFiles @(
    (Join-Path $runtimeRoot 'database.runtime.env'), $settingsPath
)
Start-ExtensionCompose -RuntimeRoot $runtimeRoot
Wait-ExtensionContainer -Name 'sub2api-account-eval'

# Update only gateway code. Its existing rules/configuration remain in ProgramData.
$gatewaySource = Join-Path $PSScriptRoot '..\deploy\windows\policy-gateway'
foreach ($name in @('Dockerfile', 'go.mod', 'main.go')) {
    Copy-Item -LiteralPath (Join-Path $gatewaySource $name) -Destination (Join-Path $mainContext.PolicyGatewayRuntimeRoot $name) -Force
}
Invoke-Sub2ApiCompose -Context $mainContext -Arguments @('up', '-d', '--no-deps', '--build', 'policy-gateway')
Wait-ExtensionContainer -Name 'sub2api-policy-gateway'
# Check the actual route, not just the evaluator's own health endpoint.
Invoke-ExtensionDocker -Arguments @('exec', 'sub2api-policy-gateway', 'wget', '-q', '-T', '5', '-O', '/dev/null', 'http://127.0.0.1:8080/account-eval/')

# Bootstrap credentials travel through stdin only. Never persist or log them.
$applicationEnv = Get-ExtensionContainerEnvironment -Inspect (Get-ExtensionContainerInspect -Name 'sub2api')
$credentials = @{
    email = [string]$applicationEnv['ADMIN_EMAIL']
    password = [string]$applicationEnv['ADMIN_PASSWORD']
} | ConvertTo-Json -Compress
$process = [Diagnostics.Process]::new()
$process.StartInfo = [Diagnostics.ProcessStartInfo]::new()
$process.StartInfo.FileName = (Get-Command docker.exe -ErrorAction Stop).Source
$process.StartInfo.Arguments = 'exec -i sub2api-account-eval /app/account-eval -install-menu -public-url "{0}"' -f $publicUrl
$process.StartInfo.UseShellExecute = $false
$process.StartInfo.CreateNoWindow = $true
$process.StartInfo.WindowStyle = [Diagnostics.ProcessWindowStyle]::Hidden
$process.StartInfo.RedirectStandardInput = $true
$process.StartInfo.RedirectStandardOutput = $true
$process.StartInfo.RedirectStandardError = $true
try {
    [void]$process.Start()
    $process.StandardInput.WriteLine($credentials)
    $process.StandardInput.Close()
    $outputTask = $process.StandardOutput.ReadToEndAsync()
    $errorTask = $process.StandardError.ReadToEndAsync()
    if (-not $process.WaitForExit(60000)) { $process.Kill(); throw '安装管理员入口超时。' }
    $output = $outputTask.GetAwaiter().GetResult()
    $errorOutput = $errorTask.GetAwaiter().GetResult()
    if ($process.ExitCode -ne 0) {
        throw "服务已部署，管理员菜单安装未完成。请登录主站后打开 $publicUrl，在入口设置中安装。$errorOutput"
    }
    Write-Host $output.Trim() -ForegroundColor Green
} finally {
    $credentials = $null
    $applicationEnv = $null
    $process.Dispose()
}

Write-Host "账户测评部署完成：$publicUrl" -ForegroundColor Green
Write-Host "运行目录：$runtimeRoot"
