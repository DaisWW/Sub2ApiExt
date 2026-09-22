@echo off
call "%~dp0run.bat" --incremental %*
exit /b %ERRORLEVEL%
