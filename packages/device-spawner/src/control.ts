/**
 * Terminal lifecycle beyond `open_session`:
 *
 *  - `openNewSession`: a fresh `--remote-control` terminal in a project's cwd
 *    (no --resume). The CLI mints a new session id; we discover it from the
 *    registry after spawn and report it on the operation's ready event.
 *  - `closeSession`: stop the verified process of one local session. Only a
 *    PID that `verifySession` matched to this exact session is ever killed —
 *    the same rule the force-restart path follows.
 */

import { execFile, spawn } from 'node:child_process'
import {
  existsSync,
  mkdirSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from 'node:fs'
import { isAbsolute, join } from 'node:path'
import { randomUUID } from 'node:crypto'
import { promisify } from 'node:util'
import type {
  DeviceEvent,
  OpenFailureCode,
} from '../../remote-control-server/src/device/protocol'
import { isBridgeSessionId } from '../../remote-control-server/src/device/protocol'
import { logDir, type SpawnerConfig } from './config'
import { log } from './log'
import { underAllowedRoot } from './path-policy'
import {
  isProcessAlive,
  readRegistryWithReal,
  samePath,
  verifySession,
  type RegistryEntry,
} from './registry'

const execFileAsync = promisify(execFile)

export type NewSessionRequest = {
  operationId: string
  cwd: string
  configDir: string
}

export type CloseSessionRequest = {
  localSessionId: string
  cwd?: string
  bridgeSessionId?: string
}

/** Select only the process launched with this operation's unique session ID. */
export function findNewSession(
  entries: RegistryEntry[],
  sessionId: string,
  cwd: string,
  spawnedAt: number,
): RegistryEntry | undefined {
  return entries
    .filter(
      entry =>
        isProcessAlive(entry.pid) &&
        entry.sessionId === sessionId &&
        samePath(entry.cwd, cwd) &&
        (entry.startedAt === undefined ||
          entry.startedAt >= spawnedAt - 10_000),
    )
    .sort((a, b) => (b.startedAt ?? 0) - (a.startedAt ?? 0))[0]
}

async function stopProcess(pid: number): Promise<void> {
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

function escapeCmd(value: string): string {
  if (value.includes('"')) {
    throw new Error('路径包含非法字符（"）')
  }
  return value.replace(/%/g, '%%')
}

/** Shared pre-flight for every terminal-spawning request. */
function validateSpawnTarget(
  config: SpawnerConfig,
  cwd: string,
  configDir: string,
): string | undefined {
  if (!samePath(configDir, config.claudeConfigDir)) {
    return `Spawner 管理的配置目录是 ${config.claudeConfigDir}，请求来自 ${configDir}`
  }
  if (!isAbsolute(cwd)) {
    return '会话工作目录不是绝对路径'
  }
  if (!underAllowedRoot(cwd, config.allowedRoots)) {
    return '会话工作目录不在允许的根目录内'
  }
  if (!existsSync(cwd)) {
    return `工作目录不存在：${cwd}`
  }
  try {
    if (!statSync(cwd).isDirectory()) return `工作目录不是目录：${cwd}`
  } catch {
    return `无法访问工作目录：${cwd}`
  }
  return undefined
}

/**
 * Stop the verified process behind one local session. Returns what happened;
 * "already not running" is a success (the goal state), not an error.
 */
export async function closeSession(
  config: SpawnerConfig,
  request: CloseSessionRequest,
): Promise<{ closed: boolean; pid?: number; detail?: string }> {
  const verified = await verifySession(
    config.claudeConfigDir,
    request.localSessionId,
    request.cwd,
    { bridgeSessionId: request.bridgeSessionId },
  )
  const pid = verified.state.pid
  if (!verified.state.processAlive || !pid || !isProcessAlive(pid)) {
    return { closed: true, detail: '终端已经不在运行' }
  }
  // Only the PID verifySession matched to this exact session — never a guess.
  await stopProcess(pid)
  for (let i = 0; i < 25 && isProcessAlive(pid); i += 1) {
    await new Promise(done => setTimeout(done, 200))
  }
  if (isProcessAlive(pid)) {
    return {
      closed: false,
      pid,
      detail: `无法结束终端进程（PID ${pid}）`,
    }
  }
  log(`[spawner] closed session ${request.localSessionId} (pid ${pid})`)
  return { closed: true, pid }
}

/**
 * Open a fresh `--remote-control` terminal. The new local session id is not
 * known until the CLI registers itself; the ready event carries it, and the
 * server maps (device, that id) → bridge from the same event.
 */
export async function openNewSession(
  config: SpawnerConfig,
  request: NewSessionRequest,
  out: (event: Extract<DeviceEvent, { type: 'operation' }>) => void,
): Promise<void> {
  const emit = (
    state: Extract<DeviceEvent, { type: 'operation' }>['state'],
    localSessionId: string,
    extra: {
      detail?: string
      failureCode?: OpenFailureCode
      bridgeSessionId?: string
      pid?: number
    } = {},
  ): void => {
    out({
      type: 'operation',
      operationId: request.operationId,
      localSessionId,
      state,
      ...extra,
    })
  }
  // Placeholder until the real id is discovered; operations keyed by the
  // operation id on the server, so this only affects event payloads.
  const pending = `pending-${request.operationId}`
  const fail = (code: OpenFailureCode, detail: string) => {
    emit('failed', pending, { failureCode: code, detail })
    log(`[spawner] open_new_session failed: ${code} ${detail}`)
  }

  const invalid = validateSpawnTarget(config, request.cwd, request.configDir)
  if (invalid) {
    fail('config_dir_mismatch', invalid)
    return
  }

  emit('starting', pending, { detail: '正在打开新终端' })
  const spawnedAt = Date.now()

  // Write the launcher inline (same rules as open.ts, minus --resume).
  const dir = join(logDir(), 'launchers')
  mkdirSync(dir, { recursive: true })
  const newSessionId = randomUUID()
  const launcherPath = join(dir, `new-${newSessionId}.cmd`)
  const lines = [
    '@echo off',
    'chcp 65001 >nul',
    `cd /d "${escapeCmd(request.cwd)}"`,
    `start "" "${escapeCmd(config.bunPath)}" "${escapeCmd(config.cliPath)}" --session-id ${newSessionId} --remote-control`,
    '',
  ]
  writeFileSync(launcherPath, lines.join('\r\n'), 'utf8')

  const env: NodeJS.ProcessEnv = {
    ...process.env,
    CLAUDE_BRIDGE_BASE_URL: config.serverUrl,
    CLAUDE_BRIDGE_OAUTH_TOKEN: config.enrollKey,
    CLAUDE_CONFIG_DIR: config.claudeConfigDir,
    DEBUG: '1',
    CLAUDE_CODE_DEBUG_LOGS_DIR: join(
      logDir(),
      'logs',
      'cli',
      `new-${Date.now()}.txt`,
    ),
    // Keep diagnostics, hide the "Debug mode enabled" banner: this terminal
    // was opened from the phone, not started for debugging.
    CLAUDE_CODE_HIDE_DEBUG_NOTICE: '1',
  }
  delete env.CLAUDE_CODE_REMOTE
  const child = spawn('cmd.exe', ['/d', '/s', '/c', launcherPath], {
    cwd: request.cwd,
    env,
    detached: true,
    windowsHide: false,
    stdio: 'ignore',
  })
  child.on('error', error => {
    log(`[spawner] new-session launcher failed: ${error.message}`)
  })
  child.unref()
  setTimeout(() => {
    try {
      unlinkSync(launcherPath)
    } catch {
      // Already removed.
    }
  }, 30_000).unref?.()

  // Match the UUID supplied to the CLI, not another process in the same cwd.
  let localSessionId: string | undefined
  let pid: number | undefined
  const deadline = spawnedAt + 90_000
  while (Date.now() < deadline) {
    const entries = await readRegistryWithReal(config.claudeConfigDir)
    const entry = findNewSession(entries, newSessionId, request.cwd, spawnedAt)
    if (entry) {
      if (localSessionId !== entry.sessionId || pid !== entry.pid) {
        localSessionId = entry.sessionId
        pid = entry.pid
        emit('terminal_spawned', localSessionId, {
          pid,
          detail: '新终端已打开',
        })
      }
      if (entry.bridgeSessionId && isBridgeSessionId(entry.bridgeSessionId)) {
        emit('ready', localSessionId, {
          bridgeSessionId: entry.bridgeSessionId,
          pid,
          detail: '新终端与 Bridge 已就绪',
        })
        return
      }
    } else if (pid !== undefined && !isProcessAlive(pid)) {
      fail('process_exited', '终端在连接 Bridge 前退出了')
      return
    }
    await new Promise(done => setTimeout(done, 300))
  }
  fail('bridge_timeout', '90 秒内没有检测到新终端连接 Bridge')
}
