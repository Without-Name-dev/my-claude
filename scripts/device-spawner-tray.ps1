param([switch]$ReplaceExisting)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

$created = $false
$mutex = New-Object System.Threading.Mutex($true, 'Local\CCRemote.DeviceSpawner.Tray', [ref]$created)
if (-not $created) { $mutex.Dispose(); exit 0 }

$repoRoot = Split-Path -Parent $PSScriptRoot
$entry = Join-Path $repoRoot 'packages\device-spawner\src\main.ts'
$dataDir = if ($env:SPAWNER_LOG_DIR) { $env:SPAWNER_LOG_DIR } else { Join-Path $env:LOCALAPPDATA 'CCRemote' }
$lockPath = Join-Path $dataDir 'spawner.lock'
$logPath = Join-Path $dataDir 'spawner.log'
$trayLog = Join-Path $dataDir 'spawner-tray.log'
$script:exiting = $false
$script:lastStart = [DateTime]::MinValue

function Write-TrayLog([string]$message) {
  try {
    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
    Add-Content -LiteralPath $trayLog -Encoding UTF8 -Value "[$([DateTime]::Now.ToString('s'))] $message"
  } catch { }
}

function Get-SpawnerProcess {
  try {
    $lock = Get-Content -LiteralPath $lockPath -Raw | ConvertFrom-Json
    if (-not $lock.pid -or [int]$lock.pid -le 0) { return $null }
    $p = Get-CimInstance Win32_Process -Filter "ProcessId = $([int]$lock.pid)" -ErrorAction Stop
    if ($p.Name -notmatch '^bun(\.exe)?$' -or
        $p.CommandLine.IndexOf($entry, [StringComparison]::OrdinalIgnoreCase) -lt 0) { return $null }
    return Get-Process -Id $p.ProcessId -ErrorAction Stop
  } catch { return $null }
}

function Stop-VerifiedSpawner {
  $p = Get-SpawnerProcess
  if ($null -eq $p) { return }
  try {
    Stop-Process -Id $p.Id -ErrorAction Stop
    if (-not $p.WaitForExit(5000)) { throw 'The connector did not stop in time.' }
    Write-TrayLog "Stopped connector PID $($p.Id)"
  } catch {
    Write-TrayLog "Could not stop connector: $($_.Exception.Message)"
    throw
  }
}

function Start-Spawner {
  if ($script:exiting -or (Get-SpawnerProcess)) { return }
  if (([DateTime]::Now - $script:lastStart).TotalSeconds -lt 10) { return }
  $script:lastStart = [DateTime]::Now
  try {
    if (-not (Test-Path -LiteralPath $entry)) { throw 'Spawner entry not found.' }
    $bunCommand = Get-Command bun.exe -ErrorAction SilentlyContinue
    $bun = if ($bunCommand) { $bunCommand.Source } else { Join-Path $env:APPDATA 'npm\node_modules\bun\bin\bun.exe' }
    if (-not (Test-Path -LiteralPath $bun)) { throw 'bun.exe not found.' }
    $p = Start-Process -FilePath $bun -ArgumentList @("`"$entry`"") -WorkingDirectory $repoRoot -WindowStyle Hidden -PassThru
    Write-TrayLog "Started connector PID $($p.Id)"
  } catch { Write-TrayLog "Could not start connector: $($_.Exception.Message)" }
}

# The multi-resolution ICO uses the same artwork as the iOS Capacitor app.
$iconPath = Join-Path $PSScriptRoot 'assets\device-spawner.ico'
try {
  $icon = New-Object System.Drawing.Icon($iconPath)
} catch {
  Write-TrayLog "Could not load tray icon: $($_.Exception.Message)"
  $icon = [System.Drawing.SystemIcons]::Application.Clone()
}

$menu = New-Object System.Windows.Forms.ContextMenuStrip
$statusItem = $menu.Items.Add('连接器启动中')
$statusItem.Enabled = $false
$null = $menu.Items.Add('-')
$logItem = $menu.Items.Add('打开日志')
$restartItem = $menu.Items.Add('重新启动连接器')
$exitItem = $menu.Items.Add('退出连接器')
$tray = New-Object System.Windows.Forms.NotifyIcon
$tray.Icon = $icon
$tray.Text = 'My Claude'
$tray.ContextMenuStrip = $menu
$tray.Visible = $true

$logItem.Add_Click({
  try {
    if (Test-Path -LiteralPath $logPath) { Start-Process -FilePath notepad.exe -ArgumentList "`"$logPath`"" }
    else { Start-Process -FilePath explorer.exe -ArgumentList "`"$dataDir`"" }
  } catch { Write-TrayLog "Could not open log: $($_.Exception.Message)" }
})
$restartItem.Add_Click({
  try { Stop-VerifiedSpawner; $script:lastStart = [DateTime]::MinValue; Start-Spawner }
  catch { [System.Windows.Forms.MessageBox]::Show('无法重新启动连接器，请查看日志。', 'My Claude') | Out-Null }
})
$exitItem.Add_Click({
  $script:exiting = $true
  try { Stop-VerifiedSpawner }
  catch { $script:exiting = $false; return }
  [System.Windows.Forms.Application]::ExitThread()
})

$timer = New-Object System.Windows.Forms.Timer
$timer.Interval = 5000
$timer.Add_Tick({
  if ($script:exiting) { return }
  if (Get-SpawnerProcess) {
    $statusItem.Text = '连接器运行中'
    $tray.Text = 'My Claude'
  } else {
    $statusItem.Text = '连接器启动中或未运行'
    $tray.Text = 'My Claude'
    Start-Spawner
  }
})

try {
  if ($ReplaceExisting) { Stop-VerifiedSpawner }
  Start-Spawner
  $timer.Start()
  [System.Windows.Forms.Application]::Run()
} catch {
  Write-TrayLog "Tray failed: $($_.Exception.Message)"
} finally {
  $timer.Stop(); $timer.Dispose()
  $tray.Visible = $false; $tray.Dispose(); $menu.Dispose()
  $icon.Dispose()
  $mutex.ReleaseMutex(); $mutex.Dispose()
}
