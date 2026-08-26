@echo off
chcp 65001 >nul
setlocal
set "CONTAINER=sub2api-rate-sync"
set "MODE=follow"
if /I "%~1"=="account" set "CONTAINER=sub2api-rate-sync-account"
if /I "%~1"=="account-once" (
    set "CONTAINER=sub2api-rate-sync-account"
    set "MODE=once"
)
if /I "%~1"=="once" set "MODE=once"

where docker >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Docker command was not found.
    pause
    exit /b 1
)

docker inspect "%CONTAINER%" >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Container %CONTAINER% was not found. Run deploy.bat first.
    pause
    exit /b 1
)

echo.
docker inspect --format "status={{.State.Status}}  restarts={{.RestartCount}}  started={{.State.StartedAt}}" "%CONTAINER%"
echo.
echo Logs use [OK], [FAIL], [SKIP], [RUN], and [INFO] markers for quick scanning.
echo Set RATE_SYNC_LOG_COLOR=always in the container environment to enable ANSI marker colors.

if /I "%MODE%"=="once" goto once
echo Following the latest 200 lines. Press Ctrl+C to exit.
docker logs --tail 200 --follow "%CONTAINER%" 2>&1
exit /b %ERRORLEVEL%

:once
docker logs --tail 200 "%CONTAINER%" 2>&1
exit /b %ERRORLEVEL%
