[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$InputPath,

    [ValidateRange(10, 300)]
    [int]$WaitSeconds = 60
)

$ErrorActionPreference = "Stop"
$maxImportBytes = 8 * 1024 * 1024
$listener = $null
$client = $null
$reader = $null

try {
    if ([string]::IsNullOrWhiteSpace($InputPath)) {
        Add-Type -AssemblyName System.Windows.Forms
        $dialog = New-Object System.Windows.Forms.OpenFileDialog
        $dialog.Filter = "JSON files (*.json;*.jsonl)|*.json;*.jsonl|All files (*.*)|*.*"
        $dialog.Multiselect = $false
        if ($dialog.ShowDialog() -ne [System.Windows.Forms.DialogResult]::OK) {
            Write-Host "Import cancelled."
            exit 2
        }
        $InputPath = $dialog.FileName
    }

    $resolvedInput = (Resolve-Path -LiteralPath $InputPath).ProviderPath
    if (-not (Test-Path -LiteralPath $resolvedInput -PathType Leaf)) {
        throw "Input path is not a file: $resolvedInput"
    }

    $rawBytes = [System.IO.File]::ReadAllBytes($resolvedInput)
    if ($rawBytes.Length -eq 0) {
        throw "The input file is empty."
    }
    if ($rawBytes.Length -gt $maxImportBytes) {
        throw "The input file exceeds Cockpit Tools' 8 MiB import limit."
    }

    $strictUtf8 = New-Object System.Text.UTF8Encoding($false, $true)
    try {
        $jsonText = $strictUtf8.GetString($rawBytes).TrimStart([char]0xFEFF)
    } catch {
        throw "The input file must be UTF-8 encoded."
    }
    if ([string]::IsNullOrWhiteSpace($jsonText)) {
        throw "The input file is empty."
    }
    $payloadBytes = $strictUtf8.GetBytes($jsonText)

    $candidates = @(
        Get-CimInstance Win32_Process -Filter "Name = 'cockpit-tools.exe'" -ErrorAction SilentlyContinue |
            ForEach-Object { $_.ExecutablePath }
    )
    $registryPaths = @(
        "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*",
        "HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*",
        "HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*"
    )
    $entries = Get-ItemProperty -Path $registryPaths -ErrorAction SilentlyContinue |
        Where-Object { $_.DisplayName -eq "Cockpit Tools" }
    foreach ($entry in $entries) {
        $installLocation = ([string]$entry.InstallLocation).Trim().Trim('"')
        if ($installLocation) {
            $candidates += Join-Path $installLocation "cockpit-tools.exe"
        }
    }
    $candidates += Join-Path $env:LOCALAPPDATA "Cockpit Tools\cockpit-tools.exe"
    $cockpitExecutable = $candidates |
        Where-Object { $_ -and (Test-Path -LiteralPath $_ -PathType Leaf) } |
        Select-Object -First 1
    if (-not $cockpitExecutable) {
        throw "Cockpit Tools was not found. Install it for the current Windows user and try again."
    }

    $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, 0)
    $listener.ExclusiveAddressUse = $true
    $listener.Start()
    $port = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    $secretPath = "/$([guid]::NewGuid().ToString('N'))"
    $importUrl = "http://127.0.0.1:$port$secretPath"
    $deepLink = "cockpit-tools://import?provider=codex" +
        "&import_url=$([uri]::EscapeDataString($importUrl))" +
        "&auto_import=true&min_app_version=0.22.19"

    Write-Host "Opening Cockpit Tools and delivering the import bundle..."
    Start-Process -FilePath $cockpitExecutable -ArgumentList $deepLink | Out-Null

    $acceptTask = $listener.AcceptTcpClientAsync()
    if (-not $acceptTask.Wait([TimeSpan]::FromSeconds($WaitSeconds))) {
        throw "Cockpit Tools did not request the import bundle within $WaitSeconds seconds."
    }
    $client = $acceptTask.GetAwaiter().GetResult()
    $stream = $client.GetStream()
    $stream.ReadTimeout = 5000
    $stream.WriteTimeout = 5000
    $reader = New-Object System.IO.StreamReader($stream, [System.Text.Encoding]::ASCII, $false, 1024, $true)

    $requestLine = $reader.ReadLine()
    do {
        $headerLine = $reader.ReadLine()
    } while ($null -ne $headerLine -and $headerLine.Length -gt 0)

    if ($requestLine -notmatch '^GET\s+([^\s?]+)(?:\?\S*)?\s+HTTP/1\.[01]$' -or $Matches[1] -ne $secretPath) {
        throw "Cockpit Tools requested an unexpected local URL. Run the BAT again."
    }

    $header = "HTTP/1.1 200 OK`r`n" +
        "Content-Type: application/json; charset=utf-8`r`n" +
        "Content-Length: $($payloadBytes.Length)`r`n" +
        "Cache-Control: no-store`r`n" +
        "Connection: close`r`n`r`n"
    $headerBytes = [System.Text.Encoding]::ASCII.GetBytes($header)
    $stream.Write($headerBytes, 0, $headerBytes.Length)
    $stream.Write($payloadBytes, 0, $payloadBytes.Length)
    $stream.Flush()
    Write-Host "Import bundle delivered. Cockpit Tools is processing the accounts."
} catch {
    [Console]::Error.WriteLine("ERROR: " + $_.Exception.Message)
    exit 1
} finally {
    if ($reader) { $reader.Dispose() }
    if ($client) { $client.Dispose() }
    if ($listener) { $listener.Stop() }
}
