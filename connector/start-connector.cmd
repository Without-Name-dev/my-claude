@echo off
cd /d "%~dp0"
set CONFIG=%APPDATA%\Agents Anywhere\connector\connector.json
echo Starting patched Agents Anywhere connector...
echo Config: %CONFIG%
.venv\Scripts\python.exe -m connector.cli start --config "%CONFIG%" --debug
pause
