/**
 * Open-operation orchestration for device sessions.
 *
 * Starting a terminal is slow (process spawn → trust screen → bridge
 * handshake), so the HTTP request only acknowledges the operation. Progress
 * is pushed by the Spawner as `operation` events and the web UI polls or
 * follows it. State is deliberately honest: `ready` means the CLI itself
 * wrote a bridge session id into its registry; nothing here guesses.
 */

import { randomUUID } from 'node:crypto'
import { log } from '../logger'
import { isBridgeConnected, onBridgeAttached } from '../transport/ws-handler'
import { publishSessionEvent } from '../services/transport'
import { requestDevice } from './hub'
import {
  clearMapping,
  enqueueOutbox,
  findMappingByBridgeId,
  getCatalog,
  getMapping,
  listOutbox,
  listQueuedSessionIds,
  requeueFailedOutbox,
  setMapping,
  setRuntime,
  updateOutbox,
  type OutboxEntry,
} from './state'
import {
  isLocalSessionId,
  type DeviceEvent,
  type OpenFailureCode,
  type OpenOperationState,
  type SessionSummary,
} from './protocol'

export type OperationRecord = {
  id: string
  deviceId: string
  localSessionId: string
  cwd: string
  configDir: string
  state: OpenOperationState
  detail?: string
  failureCode?: OpenFailureCode
  bridgeSessionId?: string
  pid?: number
  createdAt: number
  updatedAt: number
  /**
   * For open_new_session: the real local session id the CLI minted, once the
   * spawner's ready event reports it. The web follows the operation until it
   * can navigate to /d/<device>/s/<this>.
   */
  resolvedLocalSessionId?: string
}

const operations = new Map<string, OperationRecord>()
const activeBySession = new Map<string, string>()
const OPERATION_TTL_MS = 10 * 60 * 1000
const OPERATION_TIMEOUT_MS = 120_000

function sessionKey(deviceId: string, localSessionId: string): string {
  return `${deviceId}\u0000${localSessionId}`
}

function prune(): void {
  const cutoff = Date.now() - OPERATION_TTL_MS
  for (const [id, record] of operations) {
    const terminal =
      record.state === 'ready' ||
      record.state === 'failed' ||
      record.state === 'waiting_for_user'
    if (terminal && record.updatedAt < cutoff) {
      operations.delete(id)
      if (
        activeBySession.get(
          sessionKey(record.deviceId, record.localSessionId),
        ) === id
      ) {
        activeBySession.delete(
          sessionKey(record.deviceId, record.localSessionId),
        )
      }
    }
  }
}

export function getOperation(id: string): OperationRecord | undefined {
  prune()
  return operations.get(id)
}

export function getOperationForSession(
  deviceId: string,
  localSessionId: string,
): OperationRecord | undefined {
  const id = activeBySession.get(sessionKey(deviceId, localSessionId))
  return id ? operations.get(id) : undefined
}

/**
 * Find a session in the last-known catalog, querying the device once when the
 * cache does not know it. Never invents cwd/configDir: an open request without
 * a verified session is refused.
 */
export async function resolveSession(
  deviceId: string,
  localSessionId: string,
): Promise<SessionSummary | undefined> {
  const cached = getCatalog(deviceId)?.sessions.find(
    session => session.localSessionId === localSessionId,
  )
  if (cached) return cached

  try {
    const result = (await requestDevice(deviceId, 'list_sessions', {
      limit: 500,
    })) as { sessions?: SessionSummary[] }
    return result.sessions?.find(
      session => session.localSessionId === localSessionId,
    )
  } catch {
    return undefined
  }
}

export async function ensureOpen(
  deviceId: string,
  localSessionId: string,
  cwd: string,
  configDir: string,
  options: { force?: boolean } = {},
): Promise<OperationRecord> {
  if (!isLocalSessionId(localSessionId)) {
    throw new Error('无效的会话 ID')
  }
  // An explicit force restart is a retry: messages failed by an older run
  // become deliverable again once the fresh bridge attaches.
  if (options.force) requeueFailedOutbox(deviceId, localSessionId)

  const existingId = activeBySession.get(sessionKey(deviceId, localSessionId))
  if (existingId && !options.force) {
    const existing = operations.get(existingId)
    if (existing && existing.state !== 'failed') return existing
  }

  const existingMapping = getMapping(deviceId, localSessionId)
  const record: OperationRecord = {
    id: randomUUID(),
    deviceId,
    localSessionId,
    cwd,
    configDir,
    state: 'accepted',
    bridgeSessionId: existingMapping?.bridgeSessionId,
    createdAt: Date.now(),
    updatedAt: Date.now(),
  }
  operations.set(record.id, record)
  activeBySession.set(sessionKey(deviceId, localSessionId), record.id)

  // Belt-and-braces timeout independent of the device: an operation that
  // never reaches a terminal state must not stay "starting" forever.
  setTimeout(() => {
    const current = operations.get(record.id)
    if (!current) return
    if (current.state === 'ready' || current.state === 'failed') return
    current.state = 'failed'
    current.failureCode = 'bridge_timeout'
    current.detail = '等待终端与 Bridge 就绪超时'
    current.updatedAt = Date.now()
    const key = sessionKey(deviceId, localSessionId)
    if (activeBySession.get(key) === record.id) activeBySession.delete(key)
  }, OPERATION_TIMEOUT_MS).unref?.()

  try {
    await requestDevice(
      deviceId,
      'open_session',
      {
        localSessionId,
        cwd,
        configDir,
        operationId: record.id,
        ...(options.force ? { force: true } : {}),
      },
      10_000,
    )
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    // A timeout here does not prove the spawn failed — the device may still
    // report progress. Keep the operation alive unless the device is offline.
    if (message.includes('不在线')) {
      record.state = 'failed'
      record.failureCode = 'spawn_failed'
      record.detail = message
      record.updatedAt = Date.now()
      const key = sessionKey(deviceId, localSessionId)
      if (activeBySession.get(key) === record.id) activeBySession.delete(key)
    } else {
      record.detail = message
      record.updatedAt = Date.now()
    }
  }

  return record
}

/**
 * Open a brand-new terminal (`--remote-control`, no --resume) in a project
 * cwd. The local session id is unknown until the CLI registers itself, so the
 * operation runs under a placeholder id; the spawner's ready event carries the
 * real one (also stored as resolvedLocalSessionId for the web to follow).
 */
export async function ensureOpenNew(
  deviceId: string,
  cwd: string,
  configDir: string,
): Promise<OperationRecord> {
  const placeholder = randomUUID()
  const record: OperationRecord = {
    id: randomUUID(),
    deviceId,
    localSessionId: placeholder,
    cwd,
    configDir,
    state: 'accepted',
    createdAt: Date.now(),
    updatedAt: Date.now(),
  }
  operations.set(record.id, record)
  const key = sessionKey(deviceId, placeholder)
  activeBySession.set(key, record.id)

  setTimeout(() => {
    const current = operations.get(record.id)
    if (!current) return
    if (current.state === 'ready' || current.state === 'failed') return
    current.state = 'failed'
    current.failureCode = 'bridge_timeout'
    current.detail = '等待新终端与 Bridge 就绪超时'
    current.updatedAt = Date.now()
    if (activeBySession.get(key) === record.id) activeBySession.delete(key)
  }, OPERATION_TIMEOUT_MS).unref?.()

  try {
    await requestDevice(
      deviceId,
      'open_new_session',
      { cwd, configDir, operationId: record.id },
      10_000,
    )
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    if (message.includes('不在线')) {
      record.state = 'failed'
      record.failureCode = 'spawn_failed'
      record.detail = message
      record.updatedAt = Date.now()
      if (activeBySession.get(key) === record.id) activeBySession.delete(key)
    } else {
      record.detail = message
      record.updatedAt = Date.now()
    }
  }
  return record
}

/**
 * Stop the terminal of one local session. The spawner only ever kills a PID it
 * verified for this exact session; "already not running" still reports closed.
 */
export async function closeTerminal(
  deviceId: string,
  localSessionId: string,
): Promise<{ closed: boolean; detail?: string }> {
  const mapping = getMapping(deviceId, localSessionId)
  const result = (await requestDevice(deviceId, 'close_session', {
    localSessionId,
    ...(mapping?.cwd ? { cwd: mapping.cwd } : {}),
    ...(mapping?.bridgeSessionId
      ? { bridgeSessionId: mapping.bridgeSessionId }
      : {}),
  })) as { closed?: boolean; detail?: string }
  if (result.closed) {
    clearMapping(deviceId, localSessionId)
    setRuntime(deviceId, localSessionId, {
      localSessionId,
      cwd: mapping?.cwd ?? '',
      processAlive: false,
      verifiedAt: Date.now(),
      source: 'managed',
    })
  }
  return {
    closed: result.closed === true,
    detail: typeof result.detail === 'string' ? result.detail : undefined,
  }
}

export function handleDeviceEvent(deviceId: string, event: DeviceEvent): void {
  switch (event.type) {
    case 'operation': {
      const record = operations.get(event.operationId)
      if (!record) {
        // The operation began before an RCS restart. A ready event still
        // carries enough information to repair the mapping.
        if (event.state === 'ready' && event.bridgeSessionId) {
          setMapping({
            deviceId,
            localSessionId: event.localSessionId,
            bridgeSessionId: event.bridgeSessionId,
            cwd: '',
            updatedAt: Date.now(),
          })
          // The pre-restart operation is gone, but its queued messages are
          // still valid: the bridge just became reachable.
          void flushOutbox(
            deviceId,
            event.localSessionId,
            event.bridgeSessionId,
          )
        }
        return
      }
      record.state = event.state
      record.detail = event.detail
      record.failureCode = event.failureCode
      if (event.bridgeSessionId) record.bridgeSessionId = event.bridgeSessionId
      if (event.pid) record.pid = event.pid
      // A fresh terminal reports the session id the CLI minted; remember it so
      // the web can navigate to the new conversation.
      if (
        event.localSessionId &&
        event.localSessionId !== record.localSessionId
      ) {
        record.resolvedLocalSessionId = event.localSessionId
      }
      record.updatedAt = Date.now()

      if (event.state === 'ready' && event.bridgeSessionId) {
        setMapping({
          deviceId,
          localSessionId: record.localSessionId,
          bridgeSessionId: event.bridgeSessionId,
          cwd: record.cwd,
          updatedAt: Date.now(),
        })
        void flushOutbox(deviceId, record.localSessionId, event.bridgeSessionId)
      } else if (event.state === 'failed') {
        void markQueuedFailed(
          deviceId,
          record.localSessionId,
          event.failureCode,
          event.detail,
        )
      }
      // `waiting_for_user` keeps the operation active: the terminal is still
      // starting and a second request must not spawn a duplicate window.
      if (event.state === 'ready' || event.state === 'failed') {
        const key = sessionKey(deviceId, record.localSessionId)
        if (activeBySession.get(key) === record.id) activeBySession.delete(key)
      }
      return
    }
    case 'session_state': {
      // Runtime evidence is stored by the route layer, which has the
      // device-scoped key handling; see routes/device.ts.
      return
    }
    case 'catalog_changed':
      return
  }
}

export async function flushOutbox(
  deviceId: string,
  localSessionId: string,
  bridgeSessionId: string,
): Promise<void> {
  // Only `queued` entries were never published. `unconfirmed` entries were
  // already published to the event bus, and the bridge's attach replay in
  // handleWebSocketOpen delivers them; re-publishing here would run the same
  // prompt twice in the terminal.
  const entries = listOutbox(deviceId, localSessionId).filter(
    entry => entry.state === 'queued',
  )
  if (entries.length === 0) return
  const connected = isBridgeConnected(bridgeSessionId)
  for (const entry of entries) {
    try {
      publishSessionEvent(bridgeSessionId, 'user', entry.payload, 'outbound')
      updateOutbox(entry.id, {
        state: connected ? 'sent' : 'unconfirmed',
        bridgeSessionId,
        detail: connected
          ? undefined
          : '已放入服务端队列，但尚未确认终端已连接',
      })
    } catch (error) {
      updateOutbox(entry.id, {
        state: 'failed',
        detail: `发送失败：${(error as Error).message}`,
      })
    }
  }
  log(
    `[device] outbox flush ${deviceId}/${localSessionId}: ${entries.length} message(s), ` +
      `bridge=${connected ? 'connected' : 'not connected'}`,
  )
}

/** The bridge replayed the outbound bus, so those messages are now received. */
export function markUnconfirmedSent(
  deviceId: string,
  localSessionId: string,
): void {
  for (const entry of listOutbox(deviceId, localSessionId)) {
    if (entry.state === 'unconfirmed') {
      updateOutbox(entry.id, { state: 'sent', detail: undefined })
    }
  }
}

/**
 * A Spawner just reconnected. Messages queued while it was offline still
 * need a terminal: open one for every session with a queued prompt. The
 * eventual `ready` event flushes the outbox.
 */
export async function resumeQueuedSessions(
  deviceId: string,
  configDir: string,
): Promise<void> {
  for (const localSessionId of listQueuedSessionIds(deviceId)) {
    if (activeBySession.has(sessionKey(deviceId, localSessionId))) continue
    const session = await resolveSession(deviceId, localSessionId)
    if (!session) {
      log(
        `[device] queued session ${localSessionId} is not in the catalog; ` +
          'will retry after the next catalog refresh',
      )
      continue
    }
    try {
      await ensureOpen(deviceId, localSessionId, session.cwd, configDir)
    } catch (error) {
      log(
        `[device] cannot open queued session ${localSessionId}: ${(error as Error).message}`,
      )
    }
  }
}

// Flush queued messages as soon as a bridge attaches. Waiting for an
// operation `ready` event strands messages when the terminal was started
// manually or the RCS restarted mid-open.
onBridgeAttached(bridgeSessionId => {
  const mapping = findMappingByBridgeId(bridgeSessionId)
  if (!mapping) return
  markUnconfirmedSent(mapping.deviceId, mapping.localSessionId)
  void flushOutbox(mapping.deviceId, mapping.localSessionId, bridgeSessionId)
})

/**
 * Failures that the user or a later reconnect can recover from. Destroying
 * the prompt here is what made "重启终端并启用" deliver nothing: the message
 * was already marked failed and the final flush only sends `queued` rows.
 */
const RETRYABLE_OPEN_FAILURES = new Set([
  'already_running_without_bridge',
  'bridge_timeout',
  'spawn_failed',
  'process_exited',
  'busy',
])

export async function markQueuedFailed(
  deviceId: string,
  localSessionId: string,
  failureCode: string | undefined,
  detail: string | undefined,
): Promise<void> {
  if (!failureCode || RETRYABLE_OPEN_FAILURES.has(failureCode)) {
    if (failureCode) {
      log(
        `[device] keeping queued messages for ${localSessionId}: ${failureCode}`,
      )
    }
    return
  }
  for (const entry of listOutbox(deviceId, localSessionId)) {
    // `unconfirmed` was already published to the bus and may still be
    // replayed by a bridge attach; never turn it into a failure.
    if (entry.state !== 'queued') continue
    updateOutbox(entry.id, {
      state: 'failed',
      detail: detail ?? '终端启动失败，消息未发送',
    })
  }
}

export function queueMessage(
  deviceId: string,
  localSessionId: string,
  text: string,
): OutboxEntry {
  const id = randomUUID()
  const entry: OutboxEntry = {
    id,
    deviceId,
    localSessionId,
    payload: {
      type: 'user',
      // Reuse the outbox id as the bridge message uuid: the CLI echoes that
      // uuid, so the conversation store can dedupe the optimistic web row
      // and the real transcript row by primary key.
      uuid: id,
      content: text,
      message: { content: text },
    },
    state: 'queued',
    createdAt: Date.now(),
    updatedAt: Date.now(),
    attempts: 0,
  }
  enqueueOutbox(entry)
  return entry
}

/** Deliver immediately through the bridge and report what is actually known. */
export function deliverNow(
  bridgeSessionId: string,
  deviceId: string,
  localSessionId: string,
  text: string,
): { state: 'sent' | 'unconfirmed'; entry: OutboxEntry } {
  const entry = queueMessage(deviceId, localSessionId, text)
  // A web SSE subscriber is not delivery evidence; only an attached bridge
  // transport means the CLI received the event.
  const connected = isBridgeConnected(bridgeSessionId)
  try {
    publishSessionEvent(bridgeSessionId, 'user', entry.payload, 'outbound')
    updateOutbox(entry.id, {
      state: connected ? 'sent' : 'unconfirmed',
      bridgeSessionId,
      detail: connected ? undefined : '已放入服务端队列，但尚未确认终端已连接',
    })
    return {
      state: connected ? 'sent' : 'unconfirmed',
      entry: { ...entry, state: connected ? 'sent' : 'unconfirmed' },
    }
  } catch (error) {
    updateOutbox(entry.id, {
      state: 'failed',
      detail: `发送失败：${(error as Error).message}`,
    })
    return { state: 'unconfirmed', entry: { ...entry, state: 'failed' } }
  }
}
