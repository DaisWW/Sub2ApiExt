@echo off
setlocal

if not "%~2"=="" (
    echo Only one card-code file can be used at a time.
    pause
    exit /b 2
)

where python.exe >nul 2>&1
if errorlevel 1 (
    echo Python 3 was not found in PATH.
    pause
    exit /b 3
)

python.exe -c "import sys; raise SystemExit(0 if sys.version_info >= (3, 8) else 1)" >nul 2>&1
if errorlevel 1 (
    echo Python 3.8 or newer is required.
    python.exe --version 2>&1
    pause
    exit /b 3
)

if "%~1"=="" (
    python.exe "%~dp0redeem-account-import.py"
) else (
    python.exe "%~dp0redeem-account-import.py" "%~f1"
)
set "REDEEM_EXIT_CODE=%ERRORLEVEL%"
echo.
if "%REDEEM_EXIT_CODE%"=="0" (
    echo Finished successfully.
) else (
    echo Redeem failed with exit code %REDEEM_EXIT_CODE%.
)
pause
exit /b %REDEEM_EXIT_CODE%
