@echo off
call "%~dp0..\run.bat" --script "%~dp0main.py" %*
exit /b %ERRORLEVEL%
