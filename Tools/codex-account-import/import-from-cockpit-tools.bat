@echo off
setlocal

if not "%~1"=="" if /I not "%~1"=="/WhatIf" (
    echo Usage: double-click this BAT to import, or run it with /WhatIf to validate only.
    pause
    exit /b 2
)

set "CHECK_ONLY="
if /I "%~1"=="/WhatIf" set "CHECK_ONLY=-WhatIf"

where python.exe >nul 2>&1
if errorlevel 1 (
    echo Python 3 was not found in PATH. Install Python 3.8 or newer, then try again.
    pause
    exit /b 3
)

python.exe -c "import sys; raise SystemExit(0 if sys.version_info >= (3, 8) else 1)" >nul 2>&1
if errorlevel 1 (
    echo Python 3.8 or newer is required.
    python.exe --version 2>&1
    pause
    exit /b 3
)

python.exe -c "from cryptography.hazmat.primitives.ciphers.aead import AESGCM" >nul 2>&1
if errorlevel 1 (
    if not exist "%~dp0requirements.txt" (
        echo Dependency file was not found: %~dp0requirements.txt
        pause
        exit /b 3
    )

    echo Installing the required Python package...
    python.exe -m pip install --disable-pip-version-check --user -r "%~dp0requirements.txt"
    if errorlevel 1 (
        echo Failed to install the required Python package.
        pause
        exit /b 3
    )

    python.exe -c "from cryptography.hazmat.primitives.ciphers.aead import AESGCM" >nul 2>&1
    if errorlevel 1 (
        echo The Python package was installed but cannot be loaded.
        pause
        exit /b 3
    )
)

powershell.exe -NoProfile -ExecutionPolicy Bypass ^
    -File "%~dp0import-codex-accounts.ps1" ^
    -CockpitTools %CHECK_ONLY%

set "IMPORT_EXIT_CODE=%ERRORLEVEL%"
echo.
if "%IMPORT_EXIT_CODE%"=="0" (
    echo Finished successfully.
) else (
    echo Import failed with exit code %IMPORT_EXIT_CODE%.
)
pause
exit /b %IMPORT_EXIT_CODE%
