/**
 * Device Spawner entry point.
 *
 * Responsibilities, deliberately narrow:
 * - stay connected to the RCS and report honest device/session state;
 * - list local projects/sessions and read local history on request;
 * - open a visible terminal for one historical session when asked.
 *
 * It never carries conversation content between the web and the terminal:
 * the spawned CLI's REPL Bridge does that directly.
 */

import {
  existsSync,
  mkdirSync,
  readFileSync,
  unlinkSync,
  watch,
  writeFileSync,
  type FSWatcher,
} from 'node:fs'
import { join } from 'node:path'
import type {
  DeviceCapability,
  DeviceEvent,
  DeviceServerMessage,
  SessionRuntimeState,
} from '../../remote-control-server/src/device/protocol'
import {
  isLocalSessionId,
  numberField,
  stringField,
} from '../../remote-control-server/src/device/protocol'
import {
  deviceWebSocketUrl,
  loadConfig,
  logDir,
  saveConfig,
  spawnerVersion,
  type SpawnerConfig,
} from './config'
import { errorLog, log } from './log'
import {
  isProcessAlive,
  listBunProcesses,
  readRegistryWithReal,
  verifySession,
  type BunProcess,
  type RegistryEntry,
} from './registry'
import { listProjects, listSessions, readSession } from './sessions'
import { listDirectory, uploadFiles } from './files'
import { managedSessions, openSession } from './open'
import { closeSession, openNewSession } from './control'

const CAPABILITIES: DeviceCapability[] = [
  'list_projects',
  'list_sessions',
  'read_session',
  'list_directory',
  'upload_files',
  'open_session',
  'open_new_session',
  'close_session',
  'verify_state',
  'heartbeat',
]

const eventQueue: string[] = []
const trackedSessions = new Set<string>()

let config: SpawnerConfig
let socket: WebSocket | undefined
let reconnectTimer: ReturnType<typeof setTimeout> | undefined
let heartbeatTimer: ReturnType<typeof setInterval> | undefined
let reconnectAttempt = 0
let stopping = false
let registryWatcher: FSWatcher | undefined
let stateReportTimer: ReturnType<typeof setTimeout> | undefined

/**
 * Watch the CLI's sessions directory so a terminal exit is reported within
 * ~300ms instead of waiting for the next heartbeat. Files are created on
 * startup and deleted on clean exit; either transition is useful evidence.
 */
function watchRegistry(): void {
  if (registryWatcher) return
  try {
    registryWatcher = watch(join(config.claudeConfigDir, 'sessions'), () => {
      if (stateReportTimer) return
      stateReportTimer = setTimeout(() => {
        stateReportTimer = undefined
        void reportTrackedStates()
      }, 300)
    })
    registryWatcher.on('error', () => {
      registryWatcher?.close()
      registryWatcher = undefined
    })
  } catch {
    // The sessions directory may not exist until the first CLI starts.
  }
}

let discovering = false

/**
 * Find terminals that are running right now, including ones the user started
 * by hand (never opened by this Spawner). Without this the server only knows
 * about Spawner-managed sessions, and a manual terminal looks "not running"
 * (or worse, "alive without a bridge") in the web UI.
 *
 * One WMI query + the CLI's own registry; the command line must mention the
 * session id, so a recycled PID is never reported as a CLI.
 */
async function discoverRunningSessions(): Promise<void> {
  if (discovering) return
  discovering = true
  try {
    const entries = await readRegistryWithReal(config.claudeConfigDir)
    const processes = await listBunProcesses()
    for (const entry of entries) {
      if (!isProcessAlive(entry.pid)) continue
      const proc = processes.get(entry.pid)
      // The registry file can outlive a crashed process and PIDs get reused.
      // Require a live CLI process whose start time matches the registry
      // entry. Fresh sessions have no `--resume <id>` in their command line,
      // so the session id cannot be the identity check here.
      if (!proc) continue
      const isAgentRuntime =
        /^(bun|node|claude)\.exe$/i.test(proc.name) ||
        /cli\.js/i.test(proc.commandLine)
      if (!isAgentRuntime) continue
      if (
        entry.startedAt &&
        proc.startedAt > 0 &&
        Math.abs(proc.startedAt - entry.startedAt) > 10_000
      ) {
        continue
      }
      trackedSessions.add(entry.sessionId)
      if (entry.bridgeSessionId && !managedSessions.has(entry.sessionId)) {
        managedSessions.set(entry.sessionId, {
          pid: entry.pid,
          cwd: entry.cwd,
          bridgeSessionId: entry.bridgeSessionId,
          startedAt: entry.startedAt,
          openedAt: entry.startedAt ?? Date.now(),
        })
      }
    }
    log(
      `[spawner] discovery: ${entries.length} registry entries, ` +
        `${processes.size} processes, ${trackedSessions.size} tracked`,
    )
  } finally {
    discovering = false
  }
}

async function reportTrackedStates(): Promise<void> {
  if (!isOpen()) return
  await discoverRunningSessions()
  if (trackedSessions.size === 0) return
  const states = await collectRuntimeStates()
  if (states.length > 0) sendEvent({ type: 'session_state', states })
}

function lockPath(): string {
  return join(logDir(), 'spawner.lock')
}

function acquireSingleInstanceLock(): void {
  const path = lockPath()
  mkdirSync(logDir(), { recursive: true })
  try {
    const raw = readFileSync(path, 'utf8')
    const parsed = JSON.parse(raw) as { pid?: unknown }
    if (
      typeof parsed.pid === 'number' &&
      parsed.pid !== process.pid &&
      isProcessAlive(parsed.pid)
    ) {
      log(`[spawner] already running (pid ${parsed.pid}); exiting`)
      process.exit(0)
    }
  } catch {
    // No lock or unreadable lock: take it.
  }
  writeFileSync(
    path,
    JSON.stringify({ pid: process.pid, startedAt: Date.now() }),
    'utf8',
  )
}

function releaseLock(): void {
  try {
    unlinkSync(lockPath())
  } catch {
    // Already gone.
  }
}

function isOpen(): boolean {
  return socket?.readyState === 1
}

function send(message: unknown): boolean {
  if (!isOpen()) return false
  try {
    socket?.send(JSON.stringify(message))
    return true
  } catch {
    return false
  }
}

function sendEvent(event: DeviceEvent): void {
  const message = JSON.stringify({ type: 'event', event })
  if (!send({ type: 'event', event })) {
    if (eventQueue.length < 100) eventQueue.push(message)
  }
}

function respond(id: string, result: unknown): void {
  send({ type: 'response', id, ok: true, result })
}

function respondError(id: string, code: string, message: string): void {
  send({ type: 'response', id, ok: false, error: { code, message } })
}

function sendHello(): void {
  send({
    type: 'hello',
    protocol: 1,
    deviceId: config.deviceId,
    name: config.name,
    configDir: config.claudeConfigDir,
    platform: process.platform,
    version: spawnerVersion(),
    capabilities: CAPABILITIES,
    secret: config.deviceSecret,
  })
  log(`[spawner] hello sent as ${config.deviceId}`)
}

/**
 * A managed session is only trusted when the PID still points at the same
 * process instance. PID reuse is real on Windows; the registry/start-time
 * checks used everywhere else must apply here too.
 */
function managedRuntimeState(
  localSessionId: string,
  entries: RegistryEntry[],
  processes: Map<number, BunProcess>,
): SessionRuntimeState | undefined {
  const managed = managedSessions.get(localSessionId)
  if (!managed) return undefined
  if (!isProcessAlive(managed.pid)) {
    managedSessions.delete(localSessionId)
    return undefined
  }
  const proc = processes.get(managed.pid)
  // If WMI failed we cannot prove identity; fall through to verifySession.
  if (!proc) return undefined
  const isAgentRuntime =
    /^(bun|node|claude)\.exe$/i.test(proc.name) ||
    /cli\.js/i.test(proc.commandLine)
  if (!isAgentRuntime) {
    managedSessions.delete(localSessionId)
    return undefined
  }
  if (
    managed.startedAt &&
    proc.startedAt > 0 &&
    Math.abs(proc.startedAt - managed.startedAt) > 10_000
  ) {
    managedSessions.delete(localSessionId)
    return undefined
  }
  const entry = entries.find(item => item.pid === managed.pid)
  return {
    localSessionId,
    cwd: managed.cwd,
    processAlive: true,
    pid: managed.pid,
    procStart: entry?.procStart,
    bridgeSessionId: entry?.bridgeSessionId ?? managed.bridgeSessionId,
    verifiedAt: Date.now(),
    source: 'managed',
  }
}

async function collectRuntimeStates(): Promise<SessionRuntimeState[]> {
  const states: SessionRuntimeState[] = []
  // One registry read + one process scan per report, not one per session.
  const entries = await readRegistryWithReal(config.claudeConfigDir)
  const processes = await listBunProcesses()
  for (const localSessionId of trackedSessions) {
    const managedState = managedRuntimeState(localSessionId, entries, processes)
    if (managedState) {
      states.push(managedState)
      continue
    }
    const verified = await verifySession(
      config.claudeConfigDir,
      localSessionId,
      undefined,
      { verifyCommandLine: false, processes },
    )
    states.push(verified.state)
  }
  return states
}

function startHeartbeat(): void {
  // A missing sessions directory at startup used to disable the watcher
  // forever; retry on every heartbeat tick's setup.
  if (!registryWatcher) watchRegistry()
  if (heartbeatTimer) clearInterval(heartbeatTimer)
  const interval = Math.max(config.heartbeatMs, 5_000)
  heartbeatTimer = setInterval(() => {
    void (async () => {
      // Pick up terminals started since the last tick (including manual
      // ones), then report every tracked session.
      await discoverRunningSessions()
      const running = await collectRuntimeStates()
      send({ type: 'heartbeat', ts: Date.now(), running })
    })()
  }, interval)
}

function stopHeartbeat(): void {
  if (heartbeatTimer) {
    clearInterval(heartbeatTimer)
    heartbeatTimer = undefined
  }
}

function scheduleReconnect(reason: string): void {
  stopHeartbeat()
  if (stopping) return
  reconnectAttempt += 1
  const base = Math.min(30_000, 1_000 * 2 ** Math.min(reconnectAttempt - 1, 5))
  const delay = base + Math.floor(Math.random() * 500)
  log(`[spawner] disconnected (${reason}); reconnecting in ${delay}ms`)
  if (reconnectTimer) clearTimeout(reconnectTimer)
  reconnectTimer = setTimeout(connect, delay)
}

function connect(): void {
  if (stopping) return
  const url = deviceWebSocketUrl(config.serverUrl)
  // The first connection enrolls with the shared key; afterwards the device
  // token is the bearer. The secret is also sent in hello and verified there.
  const bearer = config.enrolled ? config.deviceSecret : config.enrollKey
  const protocol = `rcs.auth.${Buffer.from(bearer, 'utf8').toString('base64url')}`
  log(`[spawner] connecting to ${url}`)
  let next: WebSocket
  try {
    next = new WebSocket(url, [protocol])
  } catch (error) {
    errorLog('[spawner] WebSocket create failed', error)
    scheduleReconnect('create failed')
    return
  }
  socket = next

  next.onopen = () => {
    reconnectAttempt = 0
    sendHello()
  }
  next.onmessage = event => {
    const text =
      typeof event.data === 'string' ? event.data : String(event.data)
    void handleServerMessage(text)
  }
  next.onerror = () => {
    log('[spawner] websocket error')
  }
  next.onclose = event => {
    if (socket === next) socket = undefined
    if (event.code === 4003 && config.enrolled) {
      // The server no longer knows this device token (e.g. state reset).
      // Fall back to the enrollment key once and re-enroll.
      config.enrolled = false
      void saveConfig(config)
      log('[spawner] device token rejected; retrying with enrollment key')
    }
    scheduleReconnect(`close ${event.code}`)
  }
}

async function handleServerMessage(text: string): Promise<void> {
  let message: DeviceServerMessage
  try {
    const value: unknown = JSON.parse(text)
    if (!value || typeof value !== 'object' || Array.isArray(value)) return
    message = value as DeviceServerMessage
  } catch {
    return
  }

  if (message.type === 'ready') {
    if (!config.enrolled) {
      config.enrolled = true
      await saveConfig(config)
    }
    log('[spawner] server accepted the device')
    watchRegistry()
    startHeartbeat()
    // Sync terminals that were already running (including manual ones)
    // before the first heartbeat interval elapses.
    void reportTrackedStates()
    // Drain one at a time: a failed send keeps the remaining events queued
    // instead of discarding them with splice().
    while (eventQueue.length > 0) {
      const queued = eventQueue[0]!
      try {
        socket?.send(queued)
        eventQueue.shift()
      } catch {
        break
      }
    }
    return
  }
  if (message.type === 'heartbeat_ack') return
  if (message.type === 'error') {
    log(`[spawner] server error ${message.code}: ${message.message}`)
    return
  }
  if (message.type === 'request') {
    log(`[spawner] request ${message.method} ${message.id}`)
    await handleRequest(message.id, message.method, message.params)
    log(`[spawner] request handled ${message.method} ${message.id}`)
  }
}

function record(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined
}

async function handleRequest(
  id: string,
  method: string,
  rawParams: unknown,
): Promise<void> {
  const params = record(rawParams) ?? {}
  try {
    switch (method) {
      case 'list_projects': {
        respond(id, { projects: await listProjects() })
        return
      }
      case 'list_sessions': {
        const projectId = stringField(params, 'projectId', 2048)
        const projectPath = stringField(params, 'projectPath', 2048)
        const limit = numberField(params, 'limit')
        const offset = numberField(params, 'offset')
        const result = await listSessions({
          projectId,
          projectPath,
          limit,
          offset,
        })
        respond(id, result)
        return
      }
      case 'read_session': {
        const localSessionId = stringField(params, 'localSessionId', 64)
        if (!localSessionId || !isLocalSessionId(localSessionId)) {
          respondError(id, 'invalid_params', '无效的会话 ID')
          return
        }
        const cwd = stringField(params, 'cwd', 2048)
        const before = stringField(params, 'before', 200)
        const limit = numberField(params, 'limit')
        respond(id, await readSession({ localSessionId, cwd, before, limit }))
        return
      }
      case 'list_directory': {
        const path = stringField(params, 'path', 4096)
        respond(id, await listDirectory(config, path))
        return
      }
      case 'upload_files': {
        const files = Array.isArray(params.files)
          ? params.files.map(entry => {
              const file = record(entry) ?? {}
              return {
                name: typeof file.name === 'string' ? file.name : '',
                data: typeof file.data === 'string' ? file.data : '',
              }
            })
          : []
        const group = stringField(params, 'group', 64)
        respond(id, await uploadFiles(files, group))
        return
      }
      case 'open_new_session': {
        const operationId = stringField(params, 'operationId', 200)
        const cwd = stringField(params, 'cwd', 2048)
        const configDir = stringField(params, 'configDir', 2048)
        if (!operationId || !cwd || !configDir) {
          respondError(id, 'invalid_params', '启动参数不完整')
          return
        }
        respond(id, { accepted: true })
        const emit = (event: Extract<DeviceEvent, { type: 'operation' }>) => {
          if (event.state === 'ready') {
            trackedSessions.add(event.localSessionId)
          }
          sendEvent(event)
        }
        void openNewSession(
          config,
          { operationId, cwd, configDir },
          emit,
        ).catch(error => {
          errorLog(`[spawner] open_new_session crashed`, error)
          emit({
            type: 'operation',
            operationId,
            localSessionId: `pending-${operationId}`,
            state: 'failed',
            failureCode: 'spawn_failed',
            detail: (error as Error).message,
          })
        })
        return
      }
      case 'close_session': {
        const localSessionId = stringField(params, 'localSessionId', 64)
        if (!localSessionId || !isLocalSessionId(localSessionId)) {
          respondError(id, 'invalid_params', '无效的会话 ID')
          return
        }
        const cwd = stringField(params, 'cwd', 2048)
        const bridgeSessionId = stringField(params, 'bridgeSessionId', 200)
        respond(
          id,
          await closeSession(config, { localSessionId, cwd, bridgeSessionId }),
        )
        return
      }
      case 'verify_state': {
        const localSessionId = stringField(params, 'localSessionId', 64)
        if (!localSessionId || !isLocalSessionId(localSessionId)) {
          respondError(id, 'invalid_params', '无效的会话 ID')
          return
        }
        trackedSessions.add(localSessionId)
        const cwd = stringField(params, 'cwd', 2048)
        const bridgeSessionId = stringField(params, 'bridgeSessionId', 200)
        const entries = await readRegistryWithReal(config.claudeConfigDir)
        const processes = await listBunProcesses()
        const managedState = managedRuntimeState(
          localSessionId,
          entries,
          processes,
        )
        if (managedState) {
          respond(id, { state: managedState })
          return
        }
        const verified = await verifySession(
          config.claudeConfigDir,
          localSessionId,
          cwd,
          { bridgeSessionId, processes },
        )
        respond(id, { state: verified.state })
        return
      }
      case 'open_session': {
        const operationId = stringField(params, 'operationId', 200)
        const localSessionId = stringField(params, 'localSessionId', 64)
        const cwd = stringField(params, 'cwd', 2048)
        const configDir = stringField(params, 'configDir', 2048)
        const force = params.force === true
        if (
          !operationId ||
          !localSessionId ||
          !isLocalSessionId(localSessionId) ||
          !cwd ||
          !configDir
        ) {
          respondError(id, 'invalid_params', '启动参数不完整')
          return
        }
        trackedSessions.add(localSessionId)
        respond(id, { accepted: true })
        const emit = (event: Extract<DeviceEvent, { type: 'operation' }>) => {
          if (event.state === 'ready' || event.state === 'failed') {
            const managed = managedSessions.get(localSessionId)
            if (managed) trackedSessions.add(localSessionId)
          }
          sendEvent(event)
        }
        void openSession(
          config,
          { operationId, localSessionId, cwd, configDir, force },
          emit,
        ).catch(error => {
          errorLog(
            `[spawner] open_session crashed for ${localSessionId}`,
            error,
          )
          emit({
            type: 'operation',
            operationId,
            localSessionId,
            state: 'failed',
            failureCode: 'spawn_failed',
            detail: (error as Error).message,
          })
        })
        return
      }
      default:
        respondError(id, 'unsupported_method', `不支持的方法：${method}`)
        return
    }
  } catch (error) {
    errorLog(`[spawner] request ${method} failed`, error)
    respondError(id, 'internal_error', (error as Error).message)
  }
}

async function main(): Promise<void> {
  config = await loadConfig()
  process.env.CLAUDE_CONFIG_DIR = config.claudeConfigDir
  acquireSingleInstanceLock()

  log(
    `[spawner] starting device=${config.deviceId} name=${config.name} ` +
      `config.rcs=${config.serverUrl} configDir=${config.claudeConfigDir}`,
  )
  if (!existsSync(config.cliPath)) {
    log(`[spawner] WARNING cliPath does not exist: ${config.cliPath}`)
  }
  if (!existsSync(config.bunPath)) {
    log(`[spawner] WARNING bunPath does not exist: ${config.bunPath}`)
  }
  watchRegistry()

  process.on('SIGINT', () => {
    stopping = true
    stopHeartbeat()
    registryWatcher?.close()
    socket?.close(1000, 'spawner stopping')
    releaseLock()
    process.exit(0)
  })
  process.on('SIGTERM', () => {
    stopping = true
    stopHeartbeat()
    registryWatcher?.close()
    if (stateReportTimer) clearTimeout(stateReportTimer)
    socket?.close(1000, 'spawner stopping')
    releaseLock()
    process.exit(0)
  })

  connect()
}

process.on('unhandledRejection', reason => {
  errorLog('[spawner] unhandled rejection', reason)
})

void main().catch(error => {
  errorLog('[spawner] fatal', error)
  process.exit(1)
})

// Keep the module explicitly side-effectful for readers: the Spawner runs
// until SIGINT/SIGTERM, driven by the WebSocket connection above.
export {}
