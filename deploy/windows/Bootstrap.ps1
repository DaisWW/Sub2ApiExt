param(
    [switch]$Elevated,
    [switch]$NoPause
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Test-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Write-BootstrapLog {
    param([Parameter(Mandatory = $true)][string]$Message)

    try {
        $logRoot = Join-Path $env:ProgramData 'Sub2API\logs'
        New-Item -ItemType Directory -Force -Path $logRoot | Out-Null
        "[$(Get-Date -Format o)] $Message" | Out-File -LiteralPath (Join-Path $logRoot 'bootstrap.log') -Append -Encoding utf8
    } catch {
        # Logging must not hide the original bootstrap error.
    }
}

if (-not (Test-Administrator)) {
    try {
        $arguments = '-NoLogo -NoProfile -ExecutionPolicy Bypass -File "{0}" -Elevated' -f $PSCommandPath
        if ($NoPause) {
            $arguments += ' -NoPause'
        }
        $process = Start-Process -FilePath 'powershell.exe' -ArgumentList $arguments -Verb RunAs -Wait -PassThru
        if ($process.ExitCode -ne 0) {
            Write-Host "Elevated deployment window exited with code $($process.ExitCode). See C:\ProgramData\Sub2API\logs\bootstrap.log" -ForegroundColor Red
        }
        exit $process.ExitCode
    } catch {
        $message = $_.Exception.Message
        Write-BootstrapLog -Message $message
        Write-Host "Could not start the elevated deployment window: $message" -ForegroundColor Red
        exit 1
    }
}

try {
    $manager = Join-Path $PSScriptRoot 'Manager.ps1'
    if (-not (Test-Path -LiteralPath $manager -PathType Leaf)) {
        throw "Deployment manager is missing: $manager"
    }

    $managerArguments = '-NoLogo -NoProfile -ExecutionPolicy Bypass -File "{0}"' -f $manager
    $managerProcess = Start-Process -FilePath 'powershell.exe' -ArgumentList $managerArguments -NoNewWindow -Wait -PassThru
    $result = $managerProcess.ExitCode
} catch {
    $result = 1
    $message = $_ | Out-String
    Write-BootstrapLog -Message $message
    Write-Host "Deployment bootstrap failed: $($_.Exception.Message)" -ForegroundColor Red
}

if ($result -ne 0) {
    Write-Host "Deployment manager exited with code $result. See the latest C:\ProgramData\Sub2API\logs\manager-*.log" -ForegroundColor Red
}

if ($Elevated -and -not $NoPause) {
    if ($result -eq 0) {
        Read-Host 'Deployment finished. Press Enter to close this window' | Out-Null
    } else {
        Read-Host 'Deployment failed. Press Enter to close this window' | Out-Null
    }
}

exit $result
