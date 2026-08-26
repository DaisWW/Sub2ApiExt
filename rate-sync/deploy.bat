@echo off
setlocal
cd /d "%~dp0"

where pwsh.exe >nul 2>&1
if errorlevel 1 (
    set "POWERSHELL_EXE=powershell.exe"
) else (
    set "POWERSHELL_EXE=pwsh.exe"
)

"%POWERSHELL_EXE%" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0deploy.ps1"
set "RESULT=%ERRORLEVEL%"

echo.
if "%RESULT%"=="0" (
    echo Rate sync deployment completed.
) else (
    echo Rate sync deployment failed with exit code %RESULT%.
)
if not defined SUB2API_EXT_NO_PAUSE pause
exit /b %RESULT%
