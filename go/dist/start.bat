@echo off
setlocal
title Agents Anywhere connector
set AA_CONNECTOR_DATA_DIR=%LOCALAPPDATA%\AgentsAnywhere
set AA_BRIDGE_DIR=%USERPROFILE%\.claude\phone-bridge
set AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE=1
set AA_LOG_LEVEL=info
set NO_PROXY=%NO_PROXY%,8.166.138.207

if not exist "%AA_CONNECTOR_DATA_DIR%\connector.json" (
    echo.
    echo   This computer is not paired yet.
    echo   Run pair.bat first, enter the six-digit code in the web app,
    echo   then run this file again.
    echo.
    pause
    exit /b 1
)
if not exist "%AA_CONNECTOR_DATA_DIR%\logs" mkdir "%AA_CONNECTOR_DATA_DIR%\logs"

:loop
rem A newer build dropped next to this file replaces the running one.
if exist "%~dp0aa-connector.exe.new" (
    move /y "%~dp0aa-connector.exe.new" "%~dp0aa-connector.exe" >nul 2>&1
)
"%~dp0aa-connector.exe" --config "%AA_CONNECTOR_DATA_DIR%\connector.json" >> "%AA_CONNECTOR_DATA_DIR%\logs\connector.log" 2>&1
echo.
echo   Connector stopped. Restarting in 5 seconds.
echo   Close this window to stop it for good.
timeout /t 5 >nul
goto loop
