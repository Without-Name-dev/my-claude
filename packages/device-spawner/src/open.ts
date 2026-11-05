/**
 * Opening a visible interactive terminal for one historical session.
 *
 * The only supported launch is `--resume <sessionId> --remote-control` in the
 * session's own cwd. We never restart a terminal that is already running: if
 * it is alive without a bridge, the user is told to run /remote-control in
 * that terminal instead.
 *
 * The process is started through `cmd /c start` so Windows creates a new
 * visible console. The PID is discovered from the CLI's own registry file,
 * keyed by session id and start time, rather than guessed from the launcher.
 */

import { execFile, spawn } from 'node:child_process'
import { existsSync, mkdirSync, unlinkSync, writeFileSync } from 'node:fs'
import { join, resolve, sep } from 'node:path'
import { promisify } from 'node:util'
import { resolveSessionFilePath } from '../../../src/utils/sessionStoragePortable'
import type {
  DeviceEvent,
  OpenFailureCode,
  OpenOperationState,
} from '../../remote-control-server/src/device/protocol'
import { isBridgeSessionId } from '../../remote-control-server/src/device/protocol'
import { logDir, type SpawnerConfig } from './config'
import { log } from './log'
import {
  findLaunchPids,
  isProcessAlive,
  readRegistry,
  samePath,
  verifySession,
  type RegistryEntry,
} from './registry'

export type OperationEmitter = (
  event: Extract<DeviceEvent, { type: 'operation' }>,
) => void

export type OpenRequest = {
  operationId: string
  localSessionId: string
  cwd: string
  configDir: string
  /** Explicit user choice: replace a terminal running without remote control. */
  force?: boolean
}

const execFileAsync = promisify(execFile)

/**
 * Stop the verified session process before relaunching with remote control.
 * Only called for a PID that `verifySession` matched to this exact session.
 */
async function stopSessionProcess(pid: number): Promise<void> {
  if (process.platform === 'win32') {
    await execFileAsync('taskkill', ['/PID', String(pid), '/T', '/F'], {
      windowsHide: true,
      timeout: 10_000,
    }).catch(() => {})
    return
  }
  try {
    process.kill(pid, 'SIGTERM')
  } catch {
    // Already gone.
  }
}

export type ManagedSession = {
  pid: number
  cwd: string
  bridgeSessionId?: string
  /** Process creation time (ms), used to prove a reused PID is ours. */
  startedAt?: number
  openedAt: number
}

const inFlight = new Set<string>()
export const managedSessions = new Map<string, ManagedSession>()

function underAllowedRoot(cwd: string, roots: string[]): boolean {
  if (roots.length === 0) return true
  const normalized = resolve(cwd)
  return roots.some(root => {
    const base = resolve(root)
    return normalized === base || normalized.startsWith(`${base}${sep}`)
  })
}

function emit(
  out: OperationEmitter,
  request: OpenRequest,
  state: OpenOperationState,
  extra: {
    detail?: string
    failureCode?: OpenFailureCode
    bridgeSessionId?: string
    pid?: number
  } = {},
): void {
  out({
    type: 'operation',
    operationId: request.operationId,
    localSessionId: request.localSessionId,
    state,
    ...extra,
  })
}

function failure(
  out: OperationEmitter,
  request: OpenRequest,
  code: OpenFailureCode,
  detail: string,
): void {
  emit(out, request, 'failed', { failureCode: code, detail })
  log(`[spawner] open failed ${request.localSessionId}: ${code} ${detail}`)
}

function escapeCmd(value: string): string {
  if (value.includes('"')) {
    throw new Error('路径包含非法字符（"）')
  }
  // % would be expanded by cmd inside a batch file.
  return value.replace(/%/g, '%%')
}

/**
 * Write a tiny batch launcher and run it. Passing a quoted `start` command
 * directly through Node's spawn is unreliable on Windows (libuv escapes the
 * quotes for the CRT, not for cmd), which leaves cmd waiting on a malformed
 * command line. A batch file is unambiguous and `start` guarantees a new
 * visible console for the CLI.
 */
function writeLauncher(
  config: SpawnerConfig,
  cwd: string,
  localSessionId: string,
): string {
  const dir = join(logDir(), 'launchers')
  mkdirSync(dir, { recursive: true })
  const path = join(dir, `${localSessionId}-${Date.now()}.cmd`)
  const lines = [
    '@echo off',
    // Batch files are parsed in the OEM code page; switch to UTF-8 so
    // non-ASCII runtime/CLI paths survive.
    'chcp 65001 >nul',
    `cd /d "${escapeCmd(cwd)}"`,
    `start "" "${escapeCmd(config.bunPath)}" "${escapeCmd(config.cliPath)}" --resume ${localSessionId} --remote-control`,
    '',
  ]
  writeFileSync(path, lines.join('\r\n'), 'utf8')
  return path
}

function launchSession(
  config: SpawnerConfig,
  cwd: string,
  localSessionId: string,
): void {
  // Collect the CLI's own debug log (bridge lifecycle, ingress messages)
  // under CCRemote/logs/cli so failures in spawned terminals are diagnosable
  // without touching the user's real ~/.claude. Note: the CLI treats
  // CLAUDE_CODE_DEBUG_LOGS_DIR as a full file path, so give it one file per
  // session instead of a directory.
  const cliLogDir = join(logDir(), 'logs', 'cli')
  mkdirSync(cliLogDir, { recursive: true })
  const cliLogFile = join(cliLogDir, `${localSessionId}-${Date.now()}.txt`)
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    CLAUDE_BRIDGE_BASE_URL: config.serverUrl,
    CLAUDE_BRIDGE_OAUTH_TOKEN: config.enrollKey,
    CLAUDE_CONFIG_DIR: config.claudeConfigDir,
    DEBUG: '1',
    CLAUDE_CODE_DEBUG_LOGS_DIR: cliLogFile,
  }
  // Never inherit a stale remote-execution marker from whatever launched the
  // Spawner; this child is a normal interactive CLI.
  delete env.CLAUDE_CODE_REMOTE

  if (!existsSync(cwd)) {
    throw new Error(`工作目录不存在：${cwd}`)
  }
  const launcher = writeLauncher(config, cwd, localSessionId)
  const child = spawn('cmd.exe', ['/d', '/s', '/c', launcher], {
    cwd,
    env,
    detached: true,
    windowsHide: false,
    stdio: 'ignore',
  })
  child.on('error', error => {
    log(`[spawner] launcher failed for ${localSessionId}: ${error.message}`)
  })
  child.unref()
  setTimeout(() => {
    try {
      unlinkSync(launcher)
    } catch {
      // Already removed.
    }
  }, 30_000).unref?.()
}

function findEntry(
  entries: RegistryEntry[],
  localSessionId: string,
  spawnedAt: number,
): RegistryEntry | undefined {
  return entries
    .filter(
      entry =>
        entry.sessionId === localSessionId &&
        isProcessAlive(entry.pid) &&
        (entry.startedAt === undefined ||
          entry.startedAt >= spawnedAt - 10_000),
    )
    .sort((a, b) => (b.startedAt ?? 0) - (a.startedAt ?? 0))[0]
}

export async function openSession(
  config: SpawnerConfig,
  request: OpenRequest,
  out: OperationEmitter,
): Promise<void> {
  if (inFlight.has(request.localSessionId)) {
    failure(out, request, 'busy', '该会话已经有一个启动请求在进行中')
    return
  }
  inFlight.add(request.localSessionId)
  try {
    if (!samePath(request.configDir, config.claudeConfigDir)) {
      failure(
        out,
        request,
        'config_dir_mismatch',
        `Spawner 管理的配置目录是 ${config.claudeConfigDir}，请求来自 ${request.configDir}`,
      )
      return
    }
    if (
      !resolve(request.cwd).startsWith(sep) &&
      !/^[A-Za-z]:[\\/]/.test(request.cwd)
    ) {
      failure(out, request, 'cwd_mismatch', '会话工作目录不是绝对路径')
      return
    }
    if (!underAllowedRoot(request.cwd, config.allowedRoots)) {
      failure(out, request, 'cwd_mismatch', '会话工作目录不在允许的根目录内')
      return
    }

    const resolved = await resolveSessionFilePath(
      request.localSessionId,
      request.cwd,
    )
    if (!resolved) {
      failure(out, request, 'session_not_found', '找不到该会话的记录文件')
      return
    }

    const existing = await verifySession(
      config.claudeConfigDir,
      request.localSessionId,
      request.cwd,
    )
    if (existing.state.processAlive && !request.force) {
      if (existing.state.bridgeSessionId) {
        emit(out, request, 'ready', {
          bridgeSessionId: existing.state.bridgeSessionId,
          pid: existing.state.pid,
          detail: '该会话已经在运行并连接了 Bridge',
        })
        return
      }
      failure(
        out,
        request,
        'already_running_without_bridge',
        `该会话已在终端中运行（PID ${existing.state.pid ?? '未知'}），但没有启用远程控制。请在那个终端执行 /remote-control，或选择“重启终端并启用远程控制”。`,
      )
      return
    }
    if (existing.state.processAlive && request.force) {
      // The operator explicitly chose to replace the terminal. Kill only the
      // PID that verifySession matched to this exact session, wait for it to
      // exit, then launch a fresh `--resume --remote-control` terminal.
      const pid = existing.state.pid
      if (pid && isProcessAlive(pid)) {
        emit(out, request, 'starting', {
          detail: `正在关闭未启用远程控制的终端（PID ${pid}）…`,
        })
        await stopSessionProcess(pid)
        for (let i = 0; i < 25 && isProcessAlive(pid); i += 1) {
          await new Promise(resolve => setTimeout(resolve, 200))
        }
        if (isProcessAlive(pid)) {
          failure(
            out,
            request,
            'spawn_failed',
            `无法结束旧终端进程（PID ${pid}）。请在电脑上手动关闭它后重试。`,
          )
          return
        }
      }
    }

    emit(out, request, 'starting', { detail: '正在打开终端' })
    const spawnedAt = Date.now()
    try {
      launchSession(config, request.cwd, request.localSessionId)
    } catch (error) {
      failure(out, request, 'spawn_failed', (error as Error).message)
      return
    }

    let currentPid: number | undefined
    let bridgeSessionId: string | undefined
    let waitingReported = false
    let lastPidScan = 0
    const deadline = spawnedAt + 90_000

    while (Date.now() < deadline) {
      const elapsed = Date.now() - spawnedAt

      // `cmd /c start` does not expose the child PID. Find the process by its
      // `--resume <id>` command line shortly after launch, then follow its
      // registry entry by PID: after resume the entry's sessionId can differ
      // from the requested one.
      if (
        currentPid === undefined &&
        elapsed > 500 &&
        elapsed - lastPidScan > 2000
      ) {
        lastPidScan = elapsed
        const pids = findLaunchPids(request.localSessionId).filter(
          isProcessAlive,
        )
        if (pids.length > 0) {
          currentPid = pids[0]
          emit(out, request, 'terminal_spawned', {
            pid: currentPid,
            detail: '终端已打开',
          })
          managedSessions.set(request.localSessionId, {
            pid: currentPid,
            cwd: request.cwd,
            openedAt: spawnedAt,
          })
        }
      }

      const entries = await readRegistry(config.claudeConfigDir)
      const entry =
        (currentPid !== undefined
          ? entries.find(
              item => item.pid === currentPid && isProcessAlive(item.pid),
            )
          : undefined) ?? findEntry(entries, request.localSessionId, spawnedAt)

      if (entry) {
        const managed = managedSessions.get(request.localSessionId)
        if (
          managed &&
          managed.pid === entry.pid &&
          managed.startedAt === undefined &&
          entry.startedAt !== undefined
        ) {
          managed.startedAt = entry.startedAt
        }
        if (currentPid === undefined) {
          currentPid = entry.pid
          emit(out, request, 'terminal_spawned', {
            pid: entry.pid,
            detail: '终端已打开',
          })
          managedSessions.set(request.localSessionId, {
            pid: entry.pid,
            cwd: request.cwd,
            startedAt: entry.startedAt,
            openedAt: spawnedAt,
          })
        }
        if (entry.bridgeSessionId && isBridgeSessionId(entry.bridgeSessionId)) {
          bridgeSessionId = entry.bridgeSessionId
          managedSessions.set(request.localSessionId, {
            pid: entry.pid,
            cwd: request.cwd,
            bridgeSessionId,
            startedAt: entry.startedAt,
            openedAt: spawnedAt,
          })
          break
        }
        if (!waitingReported && Date.now() - spawnedAt > 20_000) {
          waitingReported = true
          emit(out, request, 'waiting_for_user', {
            pid: entry.pid,
            detail:
              '终端已打开，但在等待本机确认（信任目录、权限或模型配置）。请在电脑上处理。',
          })
        }
      } else if (currentPid !== undefined && !isProcessAlive(currentPid)) {
        failure(out, request, 'process_exited', '终端在连接 Bridge 前退出了')
        return
      }
      // The registry is the cheap, authoritative signal; poll it quickly and
      // let the (blocking) PID scan run less often.
      await new Promise(resolve => setTimeout(resolve, 300))
    }

    if (bridgeSessionId && currentPid !== undefined) {
      emit(out, request, 'ready', {
        bridgeSessionId,
        pid: currentPid,
        detail: '终端与 Bridge 已就绪',
      })
      return
    }

    if (currentPid !== undefined && isProcessAlive(currentPid)) {
      failure(
        out,
        request,
        'bridge_timeout',
        '终端仍在运行，但 90 秒内没有连上 Bridge。它可能停在信任/权限提示，或模型配置有误。',
      )
      return
    }
    failure(out, request, 'spawn_failed', '没有检测到启动后的终端进程')
  } finally {
    inFlight.delete(request.localSessionId)
  }
}
