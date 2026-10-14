Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Load only the URL-discovery function; never execute deployment against real services.
$tokens = $null
$errors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile(
    (Join-Path $PSScriptRoot 'deploy.ps1'), [ref]$tokens, [ref]$errors)
if ($errors.Count -gt 0) { throw 'Deployment script did not parse.' }
$definition = $ast.Find({ param($node)
    $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Get-EvalPublicUrl'
}, $false)
. ([scriptblock]::Create($definition.Extent.Text))

$sub2api = [pscustomobject]@{ PostgresUser = 'test'; PostgresDatabase = 'test' }
$script:TestUrls = @()
function Invoke-ExtensionDocker { param($Arguments, [switch]$Capture) return $script:TestUrls }
function Get-ExtensionContainerInspect { param($Name)
    return [pscustomobject]@{ NetworkSettings = [pscustomobject]@{
        Ports = [pscustomobject]@{ '8080/tcp' = @([pscustomobject]@{ HostPort = '18080' }) }
    } }
}
function Get-NetIPConfiguration { param($ErrorAction)
    return [pscustomobject]@{ IPv4DefaultGateway = '192.168.1.1'; IPv4Address = @([pscustomobject]@{ IPAddress = '192.168.1.20' }) }
}

function Assert-Equal($Actual, $Expected) {
    if ($Actual -ne $Expected) { throw "Expected $Expected, got $Actual" }
}
Assert-Equal (Get-EvalPublicUrl @{}) 'http://192.168.1.20:18080/account-eval/'
$script:TestUrls = @('http://localhost:18080/', 'https://site.example/some-path')
Assert-Equal (Get-EvalPublicUrl @{}) 'https://site.example/account-eval/'
Assert-Equal (Get-EvalPublicUrl @{ EVAL_PUBLIC_URL = 'https://fixed.example/account-eval/' }) 'https://fixed.example/account-eval/'
foreach ($url in @('file:///account-eval/', 'http://user:password@site.example/account-eval/', 'https://site.example/account-eval/?token=x', 'https://site.example/other/')) {
    $rejected = $false
    try { Get-EvalPublicUrl @{ EVAL_PUBLIC_URL = $url } | Out-Null } catch { $rejected = $true }
    if (-not $rejected) { throw "Invalid public URL was accepted: $url" }
}
Write-Output 'PASS: account-eval URL discovery, fresh-machine LAN fallback, configured domain and unsafe URL rejection.'
