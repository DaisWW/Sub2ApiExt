@echo off
chcp 65001 >nul
setlocal EnableExtensions
cd /d "%~dp0"

if not exist "%~dp0deploy-all.bat" (
    echo Missing deploy-all.bat. Keep the complete deployment folder together.
    if not defined SUB2API_NO_PAUSE if not defined SUB2API_EXT_NO_PAUSE pause
    exit /b 1
)

call "%~dp0deploy-all.bat"
exit /b %ERRORLEVEL%
