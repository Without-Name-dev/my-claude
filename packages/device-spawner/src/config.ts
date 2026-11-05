/**
 * Spawner configuration and local identity.
 *
 * The file contains two secrets:
 * - `enrollKey` is the shared RCS API key, used once to enroll and then
 *   passed to each spawned CLI as CLAUDE_BRIDGE_OAUTH_TOKEN.
 * - `deviceSecret` is this machine's own token; the server stores only its
 *   SHA-256. It is generated here on first run and never printed.
 *
 * The file is created with owner-only permissions. It is never sent to the
 * browser, written to logs, or included in error messages.
 */

import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import { homedir, userInfo } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { log } from './log'

export type TerminalKind = 'console' | 'wt'

export type SpawnerConfig = {
  serverUrl: string
  enrollKey: string
  deviceId: string
  deviceSecret: string
  /** True after the server accepted this device's own token at least once. */
  enrolled: boolean
  name: string
  claudeConfigDir: string
  cliPath: string
  bunPath: string
  terminal: TerminalKind
  allowedRoots: string[]
  heartbeatMs: number
}

const repoRoot = fileURLToPath(new URL('../../../', import.meta.url))

export function configPath(): string {
  const override = process.env.SPAWNER_CONFIG
  if (override) return override
  const base = process.env.LOCALAPPDATA || join(homedir(), '.local', 'share')
  return join(base, 'CCRemote', 'spawner.json')
}

export function logDir(): string {
  const override = process.env.SPAWNER_LOG_DIR
  if (override) return override
  const base = process.env.LOCALAPPDATA || join(homedir(), '.local', 'share')
  return join(base, 'CCRemote')
}

export function defaultConfig(): SpawnerConfig {
  return {
    serverUrl: process.env.RCS_URL || 'http://127.0.0.1:3000',
    enrollKey: process.env.SPAWNER_ENROLL_KEY || process.env.RCS_API_KEY || '',
    deviceId: `dev_${randomBytes(12).toString('hex')}`,
    deviceSecret: randomBytes(32).toString('base64url'),
    enrolled: false,
    name: process.env.SPAWNER_NAME || userInfo().username,
    claudeConfigDir:
      process.env.CLAUDE_CONFIG_DIR || join(homedir(), '.claude'),
    cliPath: process.env.SPAWNER_CLI || join(repoRoot, 'dist', 'cli.js'),
    bunPath: process.execPath,
    terminal: 'console',
    allowedRoots: [],
    heartbeatMs: 15_000,
  }
}

function stringOr(value: unknown, fallback: string): string {
  return typeof value === 'string' && value.length > 0 ? value : fallback
}

function stringArray(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === 'string')
    : []
}

export async function loadConfig(): Promise<SpawnerConfig> {
  const path = configPath()
  const defaults = defaultConfig()
  let raw: Record<string, unknown> | undefined
  try {
    const text = await readFile(path, 'utf8')
    const parsed: unknown = JSON.parse(text)
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      raw = parsed as Record<string, unknown>
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') {
      throw new Error(
        `无法读取 Spawner 配置 ${path}: ${(error as Error).message}`,
      )
    }
  }

  const config: SpawnerConfig = {
    serverUrl: stringOr(raw?.serverUrl, defaults.serverUrl),
    enrollKey: stringOr(raw?.enrollKey, defaults.enrollKey),
    deviceId: stringOr(raw?.deviceId, defaults.deviceId),
    deviceSecret: stringOr(raw?.deviceSecret, defaults.deviceSecret),
    enrolled: raw?.enrolled === true,
    name: stringOr(raw?.name, defaults.name),
    claudeConfigDir: stringOr(raw?.claudeConfigDir, defaults.claudeConfigDir),
    cliPath: stringOr(raw?.cliPath, defaults.cliPath),
    bunPath: stringOr(raw?.bunPath, defaults.bunPath),
    terminal: raw?.terminal === 'wt' ? 'wt' : 'console',
    allowedRoots: stringArray(raw?.allowedRoots).map(item => resolve(item)),
    heartbeatMs:
      typeof raw?.heartbeatMs === 'number' && raw.heartbeatMs >= 5_000
        ? raw.heartbeatMs
        : defaults.heartbeatMs,
  }

  if (!config.enrollKey) {
    throw new Error(
      `Spawner 还没有 enrollment key。请编辑 ${path} 写入 RCS_API_KEYS 中的同一个值，` +
        '或者用环境变量 SPAWNER_ENROLL_KEY 启动。',
    )
  }
  try {
    new URL(config.serverUrl)
  } catch {
    throw new Error(`serverUrl 不是合法 URL: ${config.serverUrl}`)
  }

  await saveConfig(config)
  return config
}

export async function saveConfig(config: SpawnerConfig): Promise<void> {
  const path = configPath()
  await mkdir(dirname(path), { recursive: true })
  const tmp = `${path}.tmp-${process.pid}`
  await writeFile(tmp, `${JSON.stringify(config, null, 2)}\n`, 'utf8')
  await rename(tmp, path)
  hardenFile(path)
}

/**
 * Best-effort owner-only permissions. A failure here is logged but does not
 * stop the Spawner: on Windows the file usually already lives under the
 * user's profile, and refusing to start would make the failure worse.
 */
export function hardenFile(path: string): void {
  try {
    if (process.platform === 'win32') {
      const user = userInfo().username
      execFileSync(
        'icacls',
        [path, '/inheritance:r', '/grant:r', `${user}:F`],
        { stdio: 'ignore', windowsHide: true },
      )
    } else {
      execFileSync('chmod', ['600', path], { stdio: 'ignore' })
    }
  } catch (error) {
    log(
      `[spawner] could not tighten permissions on ${path}: ${(error as Error).message}`,
    )
  }
}

/** ws(s)://host/desktop/ws derived from the configured HTTP URL. */
export function deviceWebSocketUrl(serverUrl: string): string {
  const url = new URL(serverUrl)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  url.pathname = '/desktop/ws'
  url.search = ''
  url.hash = ''
  return url.toString()
}

export function spawnerVersion(): string {
  return '0.1.0'
}
