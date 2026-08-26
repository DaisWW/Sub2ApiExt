[CmdletBinding()]
param(
    [ValidateSet('start', 'restart', 'stop', 'status', 'logs')]
    [string]$Action = 'status'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Push-Location $PSScriptRoot
try {
    switch ($Action) {
        'start' {
            & docker compose up -d
        }
        'restart' {
            & docker compose restart
        }
        'stop' {
            & docker compose stop
        }
        'logs' {
            & docker compose logs --tail 200 --follow
        }
        default {
            & docker compose ps
        }
    }
    if ($LASTEXITCODE -ne 0) {
        throw "docker compose $Action failed."
    }
}
finally {
    Pop-Location
}
