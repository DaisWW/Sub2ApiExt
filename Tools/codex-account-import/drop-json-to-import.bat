@echo off
setlocal

if "%~1"=="" (
    echo Drag one Codex JSON file onto this BAT.
    pause
    exit /b 2
)

if not "%~2"=="" if /I not "%~2"=="/WhatIf" (
    echo Only one JSON file can be imported at a time.
    pause
    exit /b 2
)

if not exist "%~f1" (
    echo Input file not found.
    pause
    exit /b 2
)

set "CHECK_ONLY="
if /I "%~2"=="/WhatIf" set "CHECK_ONLY=-WhatIf"

powershell.exe -NoProfile -ExecutionPolicy Bypass ^
    -File "%~dp0import-codex-accounts.ps1" ^
    -InputPath "%~f1" %CHECK_ONLY%

set "IMPORT_EXIT_CODE=%ERRORLEVEL%"
echo.
if "%IMPORT_EXIT_CODE%"=="0" (
    echo Finished successfully.
) else (
    echo Import failed with exit code %IMPORT_EXIT_CODE%.
)
pause
exit /b %IMPORT_EXIT_CODE%
