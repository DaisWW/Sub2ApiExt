@echo off
chcp 65001 >nul
setlocal EnableExtensions
cd /d "%~dp0"
set "SUB2API_FORCE_FAILURE_PAUSE=1"

if not exist "%~dp0deploy-all.bat" (
    echo Missing deploy-all.bat. Keep the complete deployment folder together.
    pause
    exit /b 1
)

call "%~dp0deploy-all.bat"
exit /b %ERRORLEVEL%
