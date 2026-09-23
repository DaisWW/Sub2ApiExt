@echo off
call "%~dp0run.bat" --refresh-tokens %*
exit /b %ERRORLEVEL%
