@echo off
setlocal

set "SCRIPT_DIR=%~dp0"
where python.exe >nul 2>&1
if not errorlevel 1 (
    python.exe "%SCRIPT_DIR%run.py" %*
) else (
    where py.exe >nul 2>&1
    if errorlevel 1 (
        echo Python 3.8 or newer was not found in PATH.
        set "EXIT_CODE=1"
        goto finish
    )
    py.exe -3 "%SCRIPT_DIR%run.py" %*
)
set "EXIT_CODE=%ERRORLEVEL%"

:finish
echo.
if "%EXIT_CODE%"=="0" (
    echo Pipeline completed.
) else (
    echo Pipeline exit code: %EXIT_CODE%
)
pause
exit /b %EXIT_CODE%
