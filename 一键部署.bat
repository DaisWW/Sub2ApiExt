@echo off
chcp 65001 >nul
setlocal
cd /d "%~dp0"

call "%~dp0deploy-all.bat"
exit /b %ERRORLEVEL%
