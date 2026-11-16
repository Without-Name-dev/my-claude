<#
.SYNOPSIS
  Register the Device Spawner as a per-user logon task (Windows).

.DESCRIPTION
  The Spawner must run inside the operator's interactive session because it
  opens visible terminal windows; a Session-0 service cannot do that. This
  registers a Scheduled Task that starts the notification-area app at logon
  (Interactive, Limited privileges) and starts it immediately. The tray app
  starts the connector hidden; the Claude session terminals remain visible.

  Run AFTER starting the Spawner once manually, so
  %LOCALAPPDATA%\CCRemote\spawner.json exists with a valid enrollKey and the
  device is enrolled.

.PARAMETER Uninstall
  Remove the scheduled task instead of creating it.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts\install-device-spawner.ps1
  powershell -ExecutionPolicy Bypass -File scripts\install-device-spawner.ps1 -Uninstall
#>
param(
  [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'
$taskName = 'CCRemote Device Spawner'

if ($Uninstall) {
  if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    Write-Host "Removed scheduled task: $taskName"
  } else {
    Write-Host "Scheduled task not found: $taskName"
  }
  exit 0
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$entry = Join-Path $repoRoot 'packages\device-spawner\src\main.ts'
$trayEntry = Join-Path $PSScriptRoot 'device-spawner-tray.ps1'
$trayLauncher = Join-Path $PSScriptRoot 'device-spawner-tray.vbs'
$config = Join-Path $env:LOCALAPPDATA 'CCRemote\spawner.json'

if (-not (Test-Path $entry)) { throw "Spawner entry not found: $entry" }
if (-not (Test-Path $trayEntry) -or -not (Test-Path $trayLauncher)) { throw 'Spawner tray files not found.' }
if (-not (Test-Path $config)) {
  throw "Config not found: $config - start the Spawner once manually so it enrolls."
}

$bun = (Get-Command bun.exe -ErrorAction SilentlyContinue).Source
if (-not $bun) { $bun = Join-Path $env:APPDATA 'npm\node_modules\bun\bin\bun.exe' }
if (-not (Test-Path $bun)) { throw 'bun.exe not found (npm i -g bun).' }

$user = "$env:USERDOMAIN\$env:USERNAME"
$wscript = Join-Path $env:SystemRoot 'System32\wscript.exe'
if (-not (Test-Path -LiteralPath $wscript)) { throw 'wscript.exe not found.' }
$action = New-ScheduledTaskAction -Execute $wscript -Argument "`"$trayLauncher`"" -WorkingDirectory $repoRoot
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet `
  -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
  -MultipleInstances IgnoreNew -ExecutionTimeLimit ([TimeSpan]::Zero)

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger `
  -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName $taskName
Write-Host "Registered and started: $taskName"
Write-Host "  tray:   $trayLauncher"
Write-Host "  entry:  $entry"
Write-Host "  config: $config"
Write-Host "Uninstall: powershell -ExecutionPolicy Bypass -File `"$PSCommandPath`" -Uninstall"
