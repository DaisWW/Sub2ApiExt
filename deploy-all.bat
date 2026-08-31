@echo off
setlocal EnableExtensions
chcp 65001 >nul
cd /d "%~dp0"

where pwsh.exe >nul 2>&1
if errorlevel 1 (
    set "POWERSHELL_EXE=powershell.exe"
) else (
    set "POWERSHELL_EXE=pwsh.exe"
)

"%POWERSHELL_EXE%" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0deploy-all.ps1"
set "RESULT=%ERRORLEVEL%"

echo.
if "%RESULT%"=="0" (
    echo All Sub2API services and extensions were deployed.
) else (
    echo Integrated deployment failed with exit code %RESULT%.
)
if not defined SUB2API_NO_PAUSE if not defined SUB2API_EXT_NO_PAUSE pause
exit /b %RESULT%
