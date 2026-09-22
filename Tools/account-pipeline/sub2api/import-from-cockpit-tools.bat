@echo off
call "%~dp0..\run-python.bat" "%~dp0main.py" --cockpit-tools %*
exit /b %ERRORLEVEL%
