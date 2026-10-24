@echo off
setlocal
title Agents Anywhere pairing
set SERVER=http://8.166.138.207
if not "%~1"=="" set SERVER=%~1
set AA_CONNECTOR_DATA_DIR=%LOCALAPPDATA%\AgentsAnywhere
set AA_BRIDGE_DIR=%USERPROFILE%\.claude\phone-bridge
set AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE=1

echo.
echo   Server: %SERVER%
echo   The six-digit code will appear below.
echo   Keep this window open while you enter it in the web app.
echo.
"%~dp0aa-connector.exe" --pair --server %SERVER%
echo.
echo   Pairing finished. Now run start.bat and leave its window open.
pause
