/**
 * The CLI's own session registry (<configDir>/sessions/<pid>.json).
 *
 * This is the only trustworthy local statement that a process is running a
 * particular session, and that a bridge session id belongs to it. A PID alone
 * is not proof: PIDs are reused, and files can be left behind by a crash.
 * `verifySession` therefore also checks the live command line when it can.
 */

import { execFile, execFileSync } from 'node:child_process'
import { readdir, readFile } from 'node:fs/promises'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { promisify } from 'node:util'
import type { SessionRuntimeState } from '../../remote-control-server/src/device/protocol'

export type RegistryEntry = {
  pid: number
  sessionId: string
  cwd: string
  startedAt?: number
  procStart?: string
  bridgeSessionId?: string
  status?: string
  updatedAt?: number
  kind?: string
  entrypoint?: string
}

function registryDir(configDir: string): string {
  return join(configDir, 'sessions')
}

/**
 * The user's real Claude config. Spawned terminals use an isolated config dir
 * (so restarts never touch the user's settings), but manually started
 * terminals register themselves here. Reading both is what lets the web see
 * every terminal, not just the ones this Spawner launched.
 */
function realConfigDir(): string {
  return process.env.SPAWNER_REAL_CONFIG_DIR || join(homedir(), '.claude')
}

function isUuid(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
    value,
  )
}

export async function readRegistry(
  configDir: string,
): Promise<RegistryEntry[]> {
  return mergeRegistryEntries(
    await Promise.all([configDir].map(dir => readRegistryDir(dir))),
  )
}

/**
 * Registry view used by discovery: isolated config (spawned terminals) plus
 * the real config dir (terminals started by hand), deduped by pid. Kept
 * separate from readRegistry so the single-dir read stays hermetic for tests.
 */
export async function readRegistryWithReal(
  configDir: string,
): Promise<RegistryEntry[]> {
  const dirs = [...new Set([configDir, realConfigDir()])]
  const pages = await Promise.all(dirs.map(dir => readRegistryDir(dir)))
  return mergeRegistryEntries(pages)
}

function mergeRegistryEntries(pages: RegistryEntry[][]): RegistryEntry[] {
  const byPid = new Map<number, RegistryEntry>()
  for (const entry of pages.flat()) {
    const existing = byPid.get(entry.pid)
    if (!existing) {
      byPid.set(entry.pid, entry)
      continue
    }
    const existingAt = existing.updatedAt ?? 0
    const entryAt = entry.updatedAt ?? 0
    if (entryAt > existingAt) {
      byPid.set(entry.pid, entry)
    } else if (
      entryAt === existingAt &&
      !existing.bridgeSessionId &&
      entry.bridgeSessionId
    ) {
      byPid.set(entry.pid, entry)
    }
  }
  return [...byPid.values()]
}

async function readRegistryDir(dir: string): Promise<RegistryEntry[]> {
  let names: string[]
  try {
    names = await readdir(registryDir(dir))
  } catch {
    return []
  }
  const entries = await Promise.all(
    names
      .filter(name => /^\d+\.json$/.test(name))
      .map(async (name): Promise<RegistryEntry | null> => {
        try {
          const text = await readFile(join(registryDir(dir), name), 'utf8')
          const value: unknown = JSON.parse(text)
          if (!value || typeof value !== 'object' || Array.isArray(value))
            return null
          const record = value as Record<string, unknown>
          const pid = record.pid
          const sessionId = record.sessionId
          const cwd = record.cwd
          if (
            typeof pid !== 'number' ||
            typeof sessionId !== 'string' ||
            !isUuid(sessionId)
          ) {
            return null
          }
          return {
            pid,
            sessionId,
            cwd: typeof cwd === 'string' ? cwd : '',
            startedAt:
              typeof record.startedAt === 'number'
                ? record.startedAt
                : undefined,
            procStart:
              typeof record.procStart === 'string'
                ? record.procStart
                : undefined,
            bridgeSessionId:
              typeof record.bridgeSessionId === 'string' &&
              record.bridgeSessionId.length > 0
                ? record.bridgeSessionId
                : undefined,
            status:
              typeof record.status === 'string' ? record.status : undefined,
            updatedAt:
              typeof record.updatedAt === 'number'
                ? record.updatedAt
                : undefined,
            kind: typeof record.kind === 'string' ? record.kind : undefined,
            entrypoint:
              typeof record.entrypoint === 'string'
                ? record.entrypoint
                : undefined,
          }
        } catch {
          return null
        }
      }),
  )
  return entries.filter((entry): entry is RegistryEntry => entry !== null)
}

export function isProcessAlive(pid: number): boolean {
  if (!Number.isInteger(pid) || pid <= 0) return false
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    // EPERM means the process exists but belongs to another user.
    return (error as NodeJS.ErrnoException).code === 'EPERM'
  }
}

/** A recycled PID starts after the old registry record, not before it. */
export function startedAfterRegistration(
  processStartedAt: number,
  registeredAt?: number,
): boolean {
  return (
    registeredAt !== undefined &&
    processStartedAt > 0 &&
    processStartedAt > registeredAt + 10_000
  )
}

/**
 * Read the live command line for a PID. Returns undefined when the process
 * is gone or the lookup fails/times out. Used as evidence, never as the only
 * check: callers still require the registry entry and liveness.
 */
export function processCommandLine(pid: number): string | undefined {
  if (process.platform !== 'win32') {
    try {
      const raw = execFileSync('ps', ['-p', String(pid), '-o', 'command='], {
        encoding: 'utf8',
        timeout: 3000,
      })
      return raw.trim() || undefined
    } catch {
      return undefined
    }
  }
  try {
    const raw = execFileSync(
      'powershell.exe',
      [
        '-NoProfile',
        '-NonInteractive',
        '-Command',
        `(Get-CimInstance Win32_Process -Filter "ProcessId=${pid}").CommandLine`,
      ],
      { encoding: 'utf8', timeout: 4000, windowsHide: true },
    )
    const line = raw.trim()
    return line.length > 0 ? line : undefined
  } catch {
    return undefined
  }
}

/**
 * Find CLI processes launched for a specific local session.
 *
 * This is how `open_session` learns the PID: `cmd /c start` does not return
 * one, and the CLI's PID file may carry a different sessionId after resume.
 * The command line always contains `--resume <localSessionId>`.
 *
 * Async: PowerShell+CIM takes seconds and the sync version stalled the
 * single-threaded Spawner, stretching heartbeat handling and the launch
 * poll loop far past their intended cadence.
 */
export async function findLaunchPids(
  localSessionId: string,
): Promise<number[]> {
  try {
    if (process.platform === 'win32') {
      const { stdout } = await execFileAsync(
        'powershell.exe',
        [
          '-NoProfile',
          '-NonInteractive',
          '-Command',
          `Get-CimInstance Win32_Process -Filter "Name='bun.exe' OR Name='node.exe' OR Name='claude.exe'" | Where-Object { $_.CommandLine -like '*--resume ${localSessionId}*' } | ForEach-Object { $_.ProcessId }`,
        ],
        { encoding: 'utf8', timeout: 6000, windowsHide: true },
      )
      return stdout
        .split(/\r?\n/)
        .map(line => Number.parseInt(line.trim(), 10))
        .filter(pid => Number.isInteger(pid) && pid > 0)
    }
    const { stdout } = await execFileAsync('ps', ['-eo', 'pid=,args='], {
      encoding: 'utf8',
      timeout: 4000,
    })
    return stdout
      .split(/\r?\n/)
      .filter(
        line => line.includes('--resume') && line.includes(localSessionId),
      )
      .map(line => Number.parseInt(line.trim().split(/\s+/)[0] ?? '', 10))
      .filter(pid => Number.isInteger(pid) && pid > 0)
  } catch {
    return []
  }
}

/**
 * Running bun processes with their command line and start time.
 *
 * One WMI query instead of one PowerShell process per session makes it cheap
 * to discover manually started terminals when the Spawner connects. The start
 * time guards against recycled PIDs: a registry entry only matches a process
 * that started at (roughly) the same moment.
 */
export type BunProcess = {
  name: string
  commandLine: string
  startedAt: number
}

export const execFileAsync = promisify(execFile)

export async function listBunProcesses(): Promise<Map<number, BunProcess>> {
  const result = new Map<number, BunProcess>()
  if (process.platform !== 'win32') return result
  try {
    const { stdout } = await execFileAsync(
      'powershell.exe',
      [
        '-NoProfile',
        '-NonInteractive',
        '-Command',
        `Get-CimInstance Win32_Process -Filter "Name='bun.exe' OR Name='node.exe' OR Name='claude.exe'" | ForEach-Object { [PSCustomObject]@{ pid=[int]$_.ProcessId; name=[string]$_.Name; cmd=[string]$_.CommandLine; started=[int64]([DateTimeOffset]$_.CreationDate).ToUnixTimeMilliseconds() } } | ConvertTo-Json -Compress`,
      ],
      { encoding: 'utf8', timeout: 6000, windowsHide: true },
    )
    const parsed: unknown = JSON.parse(String(stdout))
    const items = Array.isArray(parsed) ? parsed : [parsed]
    for (const item of items) {
      if (!item || typeof item !== 'object') continue
      const record = item as Record<string, unknown>
      if (typeof record.pid === 'number' && typeof record.cmd === 'string') {
        result.set(record.pid, {
          name: typeof record.name === 'string' ? record.name : '',
          commandLine: record.cmd,
          startedAt: typeof record.started === 'number' ? record.started : 0,
        })
      }
    }
  } catch {
    // The lookup is best-effort; tracked sessions are still reported.
  }
  return result
}

export type VerifiedSession = {
  entry?: RegistryEntry
  state: SessionRuntimeState
}

/**
 * Determine whether a local session is currently running, using the registry
 * plus (when available) the live command line. Never invents a bridge id.
 */
export async function verifySession(
  configDir: string,
  localSessionId: string,
  expectedCwd?: string,
  options: {
    verifyCommandLine?: boolean
    bridgeSessionId?: string
    pid?: number
    /** Reuse one process scan across many verifications. */
    processes?: Map<number, BunProcess>
    /** Reuse the same registry snapshot across a state report. */
    entries?: RegistryEntry[]
  } = {},
): Promise<VerifiedSession> {
  const entries = options.entries ?? (await readRegistryWithReal(configDir))
  const processes = options.processes ?? (await listBunProcesses())
  // After `--resume`, the CLI can leave the startup session id in its PID
  // file while switching the in-memory session. Match the launched process
  // by pid or by the bridge id it actually connected with before falling
  // back to sessionId/cwd matching.
  let candidates = entries.filter(entry => entry.sessionId === localSessionId)
  if (candidates.length === 0 && options.pid !== undefined) {
    candidates = entries.filter(entry => entry.pid === options.pid)
  }
  if (candidates.length === 0 && options.bridgeSessionId) {
    candidates = entries.filter(
      entry => entry.bridgeSessionId === options.bridgeSessionId,
    )
  }
  if (candidates.length === 0 && options.verifyCommandLine !== false) {
    // The terminal was launched with `--resume <localSessionId>`, so the
    // command line identifies it even when the PID file carries a different
    // sessionId. Never guess from cwd alone: two sessions can share a cwd and
    // that would attach the wrong bridge.
    const pids = (await findLaunchPids(localSessionId)).filter(isProcessAlive)
    if (pids.length > 0) {
      candidates = entries.filter(entry => pids.includes(entry.pid))
    }
  }
  candidates = candidates.sort(
    (a, b) => (b.startedAt ?? 0) - (a.startedAt ?? 0),
  )

  for (const entry of candidates) {
    if (!isProcessAlive(entry.pid)) continue
    if (expectedCwd && entry.cwd && !samePath(entry.cwd, expectedCwd)) continue

    const proc = processes.get(entry.pid)
    // Start time is the only identity a fresh native CLI exposes; check it
    // even on the cheap heartbeat path. A stale registry file pointing at a
    // recycled PID will have a wildly different start time.
    if (proc && startedAfterRegistration(proc.startedAt, entry.startedAt)) {
      continue
    }
    if (options.verifyCommandLine !== false) {
      // The native CLI (`claude.exe`) has no `--resume` in its command line,
      // so require an agent runtime name and fall back to start-time identity.
      if (!proc) continue
      const isAgentRuntime =
        /^(bun|node|claude)\.exe$/i.test(proc.name) ||
        /cli\.js/i.test(proc.commandLine)
      if (!isAgentRuntime) continue
      const resume = proc.commandLine.match(/--resume\s+([0-9a-f-]{36})/i)
      if (
        resume?.[1] &&
        resume[1].toLowerCase() !== localSessionId.toLowerCase()
      ) {
        continue
      }
    }

    return {
      entry,
      state: {
        localSessionId,
        cwd: entry.cwd || expectedCwd || '',
        processAlive: true,
        pid: entry.pid,
        procStart: entry.procStart,
        bridgeSessionId: entry.bridgeSessionId,
        verifiedAt: Date.now(),
        source: 'registry',
      },
    }
  }

  return {
    state: {
      localSessionId,
      cwd: expectedCwd ?? '',
      processAlive: false,
      verifiedAt: Date.now(),
      source: 'registry',
    },
  }
}

export function samePath(a: string, b: string): boolean {
  const normalize = (value: string) =>
    process.platform === 'win32'
      ? value.replace(/\//g, '\\').replace(/\\+$/, '').toLowerCase()
      : value.replace(/\/+$/, '')
  return normalize(a) === normalize(b)
}

export async function waitForBridge(
  configDir: string,
  pid: number,
  localSessionId: string,
  onTick: (state: {
    elapsedMs: number
    alive: boolean
    entry?: RegistryEntry
  }) => void,
  timeoutMs = 90_000,
): Promise<RegistryEntry | undefined> {
  const startedAt = Date.now()
  while (Date.now() - startedAt < timeoutMs) {
    const alive = isProcessAlive(pid)
    const entries = await readRegistryWithReal(configDir)
    const entry = entries.find(item => item.pid === pid)
    onTick({ elapsedMs: Date.now() - startedAt, alive, entry })
    if (!alive) return undefined
    if (entry?.bridgeSessionId) return entry
    await new Promise(resolve => setTimeout(resolve, 600))
  }
  return undefined
}
