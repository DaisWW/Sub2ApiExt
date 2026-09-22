@echo off
call "%~dp0run-python.bat" "%~dp0run.py" %*
exit /b %ERRORLEVEL%
