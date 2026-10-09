@echo off
setlocal
set "SCRIPT=%~dp0main.py"
if not exist "%SCRIPT%" (
    echo Python script was not found: %SCRIPT%
    set "EXIT_CODE=2"
    goto finish
)
set "PYTHON="
if exist "%~dp0.venv\Scripts\python.exe" set "PYTHON="%~dp0.venv\Scripts\python.exe""
if not defined PYTHON (
    where python.exe >nul 2>&1
    if not errorlevel 1 set "PYTHON=python.exe"
)
if not defined PYTHON (
    where py.exe >nul 2>&1
    if not errorlevel 1 set "PYTHON=py.exe -3"
)
if not defined PYTHON (
    echo Python 3.10 or newer was not found in the local .venv or PATH.
    set "EXIT_CODE=3"
    goto finish
)
%PYTHON% -c "import sys; raise SystemExit(0 if sys.version_info >= (3, 10) else 1)" >nul 2>&1
if errorlevel 1 (
    echo Python 3.10 or newer is required.
    %PYTHON% --version 2>&1
    set "EXIT_CODE=3"
    goto finish
)
set "PYTHONUTF8=1"
%PYTHON% "%SCRIPT%" %*
set "EXIT_CODE=%ERRORLEVEL%"
:finish
echo.
if "%EXIT_CODE%"=="0" (echo Completed.) else (echo Exit code: %EXIT_CODE%)
pause
exit /b %EXIT_CODE%
