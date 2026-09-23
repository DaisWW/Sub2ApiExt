@echo off
setlocal
set "SCRIPT=%~dp0run.py"
if /I "%~1"=="--script" (
    if "%~2"=="" (
        echo Python launcher requires a script path.
        set "EXIT_CODE=2"
        goto finish
    )
    set "SCRIPT=%~f2"
    shift
    shift
)
if not exist "%SCRIPT%" (
    echo Python script was not found: %SCRIPT%
    set "EXIT_CODE=2"
    goto finish
)
set "PYTHON="
where python.exe >nul 2>&1
if not errorlevel 1 set "PYTHON=python.exe"
if not defined PYTHON (
    where py.exe >nul 2>&1
    if not errorlevel 1 set "PYTHON=py.exe -3"
)
if not defined PYTHON (
    echo Python 3.8 or newer was not found in PATH.
    set "EXIT_CODE=3"
    goto finish
)
%PYTHON% -c "import sys; raise SystemExit(0 if sys.version_info >= (3, 8) else 1)" >nul 2>&1
if errorlevel 1 (
    echo Python 3.8 or newer is required.
    %PYTHON% --version 2>&1
    set "EXIT_CODE=3"
    goto finish
)
%PYTHON% "%SCRIPT%" %*
set "EXIT_CODE=%ERRORLEVEL%"
:finish
echo.
if "%EXIT_CODE%"=="0" (echo Completed.) else (echo Exit code: %EXIT_CODE%)
pause
exit /b %EXIT_CODE%
