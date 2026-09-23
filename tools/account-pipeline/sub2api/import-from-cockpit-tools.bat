@echo off
call "%~dp0..\run.bat" --script "%~dp0main.py" --cockpit-tools %*
exit /b %ERRORLEVEL%
