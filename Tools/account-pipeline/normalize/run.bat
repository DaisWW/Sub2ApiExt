@echo off
call "%~dp0..\run-python.bat" "%~dp0main.py" %*
exit /b %ERRORLEVEL%
