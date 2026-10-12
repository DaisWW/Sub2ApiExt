@echo off
setlocal EnableExtensions
where powershell.exe >nul 2>&1
if errorlevel 1 (
    set "POWERSHELL_EXE=pwsh.exe"
) else (
    set "POWERSHELL_EXE=powershell.exe"
)
"%POWERSHELL_EXE%" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0cleanup-docker.ps1" %*
set "RESULT=%ERRORLEVEL%"
echo.
if "%RESULT%"=="0" (echo Completed.) else (echo Docker cleanup failed with exit code %RESULT%.)
if not defined SUB2API_NO_PAUSE pause
exit /b %RESULT%
