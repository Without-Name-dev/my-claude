/**
 * Web-facing device APIs.
 *
 * These are the only endpoints the new chat-first UI needs: list devices,
 * browse their local projects/sessions, read local history without starting
 * a terminal, and ask for a terminal to be opened. Everything reports what is
 * known and when it was verified; nothing is inferred from a stale cache.
 */

import { Hono } from 'hono'
import type { Context } from 'hono'
import { uuidAuth } from '../../auth/middleware'
import { log } from '../../logger'
import {
  getCatalog,
  getDevice,
  getMapping,
  getRuntime,
  listDevices,
  requeueFailedOutbox,
  setCatalog,
  setMapping,
  setRuntime,
} from '../../device/state'
import { isDeviceOnline, requestDevice } from '../../device/hub'
import { isBridgeConnected } from '../../transport/ws-handler'
import {
  isBackfillComplete,
  listStoredMessages,
  markBackfillComplete,
  oldestTranscriptMessageId,
  saveTranscript,
  saveWebUserMessage,
} from '../../device/conversation-store'
import {
  deliverNow,
  ensureOpen,
  flushOutbox,
  getOperation,
  getOperationForSession,
  queueMessage,
  resolveSession,
} from '../../device/operations'
import {
  isLocalSessionId,
  type ProjectSummary,
  type SessionRuntimeState,
  type SessionSummary,
  type TranscriptMessage,
} from '../../device/protocol'

const app = new Hono()

type DeviceParams = { deviceId?: string; localSessionId?: string }

function errorJson(
  c: Context,
  status: 400 | 404 | 409 | 503,
  type: string,
  message: string,
  extra?: Record<string, unknown>,
) {
  return c.json({ error: { type, message, ...extra } }, status)
}

function deviceFromParam(c: Context): { id: string } | undefined {
  const id = c.req.param('deviceId')
  return id ? { id } : undefined
}

app.get('/devices', uuidAuth, c => {
  const devices = listDevices().map(device => {
    const catalog = getCatalog(device.id)
    return {
      id: device.id,
      name: device.name,
      platform: device.platform,
      configDir: device.configDir,
      capabilities: device.capabilities,
      online: isDeviceOnline(device.id),
      lastSeenAt: device.lastSeenAt,
      catalogUpdatedAt: catalog?.updatedAt ?? null,
    }
  })
  return c.json({ devices })
})

app.get('/devices/:deviceId/projects', uuidAuth, async c => {
  const device = deviceFromParam(c)
  if (!device) return errorJson(c, 400, 'bad_request', '缺少设备 ID')
  const record = getDevice(device.id)
  if (!record) return errorJson(c, 404, 'device_not_found', '设备不存在')

  if (!isDeviceOnline(device.id)) {
    const catalog = getCatalog(device.id)
    if (!catalog) {
      return errorJson(
        c,
        503,
        'device_offline',
        '设备离线，且没有可用的本地缓存',
      )
    }
    return c.json({
      projects: catalog.projects,
      cached: true,
      updatedAt: catalog.updatedAt,
    })
  }

  // The Spawner prewarms this catalog on connect; serving it while it is
  // fresh avoids a multi-second disk scan on the first page load.
  const warm = getCatalog(device.id)
  if (
    warm &&
    warm.projects.length > 0 &&
    Date.now() - warm.updatedAt < 45_000
  ) {
    return c.json({
      projects: warm.projects,
      cached: false,
      warm: true,
      updatedAt: warm.updatedAt,
    })
  }

  try {
    const result = (await requestDevice(device.id, 'list_projects')) as {
      projects?: ProjectSummary[]
    }
    const projects = Array.isArray(result.projects) ? result.projects : []
    const catalog = getCatalog(device.id)
    setCatalog(device.id, projects, catalog?.sessions ?? [])
    return c.json({ projects, cached: false, updatedAt: Date.now() })
  } catch (error) {
    const catalog = getCatalog(device.id)
    log(
      `[device] list_projects failed for ${device.id}: ${(error as Error).message}`,
    )
    if (!catalog) {
      return errorJson(c, 503, 'device_unavailable', (error as Error).message)
    }
    return c.json({
      projects: catalog.projects,
      cached: true,
      updatedAt: catalog.updatedAt,
      warning: (error as Error).message,
    })
  }
})

app.get('/devices/:deviceId/sessions', uuidAuth, async c => {
  const device = deviceFromParam(c)
  if (!device) return errorJson(c, 400, 'bad_request', '缺少设备 ID')
  if (!getDevice(device.id)) {
    return errorJson(c, 404, 'device_not_found', '设备不存在')
  }
  const projectId = c.req.query('projectId')
  const limitRaw = Number(c.req.query('limit') ?? '200')
  const limit = Number.isFinite(limitRaw)
    ? Math.min(Math.max(Math.trunc(limitRaw), 1), 500)
    : 200

  if (!isDeviceOnline(device.id)) {
    const catalog = getCatalog(device.id)
    if (!catalog) {
      return errorJson(
        c,
        503,
        'device_offline',
        '设备离线，且没有可用的本地缓存',
      )
    }
    const sessions = projectId
      ? catalog.sessions.filter(session => session.projectId === projectId)
      : catalog.sessions
    return c.json({
      sessions: sessions.slice(0, limit),
      cached: true,
      updatedAt: catalog.updatedAt,
    })
  }

  try {
    const result = (await requestDevice(device.id, 'list_sessions', {
      projectId,
      limit,
    })) as { sessions?: SessionSummary[]; hasMore?: boolean }
    const sessions = Array.isArray(result.sessions) ? result.sessions : []
    const catalog = getCatalog(device.id)
    setCatalog(
      device.id,
      catalog?.projects ?? [],
      mergeCatalogSessions(catalog?.sessions ?? [], sessions),
    )
    return c.json({
      sessions,
      hasMore: Boolean(result.hasMore),
      cached: false,
      updatedAt: Date.now(),
    })
  } catch (error) {
    const catalog = getCatalog(device.id)
    if (!catalog) {
      return errorJson(c, 503, 'device_unavailable', (error as Error).message)
    }
    const sessions = projectId
      ? catalog.sessions.filter(session => session.projectId === projectId)
      : catalog.sessions
    return c.json({
      sessions: sessions.slice(0, limit),
      cached: true,
      updatedAt: catalog.updatedAt,
      warning: (error as Error).message,
    })
  }
})

function mergeCatalogSessions(
  existing: SessionSummary[],
  fresh: SessionSummary[],
): SessionSummary[] {
  const byId = new Map(
    existing.map(session => [session.localSessionId, session]),
  )
  for (const session of fresh) byId.set(session.localSessionId, session)
  return [...byId.values()].sort((a, b) => b.updatedAt - a.updatedAt)
}

app.get('/devices/:deviceId/sessions/:localSessionId', uuidAuth, async c => {
  const device = deviceFromParam(c)
  const localSessionId = c.req.param('localSessionId')
  if (!device || !localSessionId) {
    return errorJson(c, 400, 'bad_request', '缺少设备或会话 ID')
  }
  if (!isLocalSessionId(localSessionId)) {
    return errorJson(c, 400, 'bad_request', '无效的会话 ID')
  }
  if (!getDevice(device.id)) {
    return errorJson(c, 404, 'device_not_found', '设备不存在')
  }

  const online = isDeviceOnline(device.id)
  const session = await resolveSession(device.id, localSessionId)
  if (!session) {
    return errorJson(c, 404, 'session_not_found', '找不到该本地会话')
  }
  const mapping = getMapping(device.id, localSessionId)
  const runtime = getRuntime(device.id, localSessionId)
  const operation = getOperationForSession(device.id, localSessionId)
  return c.json({
    deviceId: device.id,
    online,
    session,
    bridgeSessionId: mapping?.bridgeSessionId ?? null,
    // Immediate evidence from the bridge transport. A stale mapping must not
    // make a closed terminal look reachable.
    bridgeConnected: mapping
      ? isBridgeConnected(mapping.bridgeSessionId)
      : false,
    runtime: runtime
      ? { ...runtime.state, reportedAt: runtime.reportedAt }
      : null,
    operation: operation ?? null,
  })
})

const backfilling = new Set<string>()

/**
 * Pull older transcript pages into the store in the background. The first
 * (newest) page is cached synchronously by the route; this fills in the rest
 * so the conversation is still readable when the machine is offline later.
 */
function scheduleHistoryBackfill(
  deviceId: string,
  localSessionId: string,
): void {
  const key = `${deviceId}\u0000${localSessionId}`
  if (backfilling.has(key)) return
  backfilling.add(key)
  void (async () => {
    try {
      let cursor = oldestTranscriptMessageId(deviceId, localSessionId)
      for (let page = 0; page < 10 && cursor; page += 1) {
        if (!isDeviceOnline(deviceId)) break
        const result = (await requestDevice(
          deviceId,
          'read_session',
          { localSessionId, limit: 200, before: cursor },
          20_000,
        )) as { messages?: TranscriptMessage[]; before?: string; hasMore?: boolean }
        const messages = Array.isArray(result.messages) ? result.messages : []
        if (messages.length === 0) {
          markBackfillComplete(deviceId, localSessionId)
          break
        }
        saveTranscript(deviceId, localSessionId, messages, 'older')
        if (!result.hasMore || !result.before || result.before === cursor) {
          markBackfillComplete(deviceId, localSessionId)
          break
        }
        cursor = result.before
      }
    } catch (error) {
      log(
        `[device] history backfill stopped for ${localSessionId}: ${(error as Error).message}`,
      )
    } finally {
      backfilling.delete(key)
    }
  })()
}

type NewestPage = {
  messages?: TranscriptMessage[]
  hasMore?: boolean
  total?: number
  before?: string
}

async function readNewestPage(
  deviceId: string,
  localSessionId: string,
): Promise<NewestPage> {
  return (await requestDevice(
    deviceId,
    'read_session',
    { localSessionId, limit: 200 },
    20_000,
  )) as NewestPage
}

const lastNewestRefresh = new Map<string, number>()

/**
 * The opening page is answered from SQLite immediately when it is already
 * cached; this refreshes it from the machine in the background. A cold cache
 * still waits for the device (there is nothing else to show).
 */
function scheduleNewestRefresh(
  deviceId: string,
  localSessionId: string,
): void {
  const key = `${deviceId} ${localSessionId}`
  const now = Date.now()
  if (now - (lastNewestRefresh.get(key) ?? 0) < 20_000) return
  lastNewestRefresh.set(key, now)
  void (async () => {
    try {
      const result = await readNewestPage(deviceId, localSessionId)
      if (Array.isArray(result.messages)) {
        saveTranscript(deviceId, localSessionId, result.messages, 'newest')
      }
      scheduleHistoryBackfill(deviceId, localSessionId)
    } catch {
      // The cached page is already on screen; retry on the next open.
    }
  })()
}

app.get(
  '/devices/:deviceId/sessions/:localSessionId/history',
  uuidAuth,
  async c => {
    const device = deviceFromParam(c)
    const localSessionId = c.req.param('localSessionId')
    if (!device || !localSessionId) {
      return errorJson(c, 400, 'bad_request', '缺少设备或会话 ID')
    }
    if (!isLocalSessionId(localSessionId)) {
      return errorJson(c, 400, 'bad_request', '无效的会话 ID')
    }
    if (!getDevice(device.id)) {
      return errorJson(c, 404, 'device_not_found', '设备不存在')
    }
    const limitRaw = Number(c.req.query('limit') ?? '100')
    const limit = Number.isFinite(limitRaw)
      ? Math.min(Math.max(Math.trunc(limitRaw), 1), 200)
      : 100
    const before = c.req.query('before')
    const online = isDeviceOnline(device.id)
    let warning: string | undefined

    const getPage = () =>
      listStoredMessages(device.id, localSessionId, limit, before)
    let page: ReturnType<typeof listStoredMessages>
    try {
      page = getPage()
    } catch (error) {
      log(`[device] conversation cache read failed: ${(error as Error).message}`)
      if (online && !before) {
        try {
          const result = await readNewestPage(device.id, localSessionId)
          if (Array.isArray(result.messages)) {
            saveTranscript(device.id, localSessionId, result.messages, 'newest')
          }
          return c.json({
            messages: result.messages ?? [],
            hasMore: result.hasMore === true,
            total:
              typeof result.total === 'number'
                ? result.total
                : (result.messages?.length ?? 0),
            cached: false,
            online,
            warning: '本地缓存读取失败，已直接返回设备数据',
          })
        } catch (readError) {
          warning = (readError as Error).message
        }
      }
      return errorJson(
        c,
        503,
        'conversation_unavailable',
        '会话历史暂时不可用，请稍后重试',
      )
    }

    if (online && !before) {
      if (page.total === 0) {
        // Cold cache: the device is the only source.
        try {
          const result = await readNewestPage(device.id, localSessionId)
          if (Array.isArray(result.messages)) {
            saveTranscript(device.id, localSessionId, result.messages, 'newest')
            page = getPage()
            scheduleHistoryBackfill(device.id, localSessionId)
          }
        } catch (error) {
          warning = (error as Error).message
          log(
            `[device] read_session failed for ${device.id}/${localSessionId}: ${warning}`,
          )
        }
      } else {
        // Warm cache: never make the user wait for a transcript scan.
        scheduleNewestRefresh(device.id, localSessionId)
      }
    }

    if (page.total === 0 && !online && !warning) {
      warning = '设备离线，这个会话还没有缓存的消息'
    }
    return c.json({
      messages: page.messages,
      ...(page.before ? { before: page.before } : {}),
      hasMore: page.hasMore,
      total: page.total,
      cached: !online,
      online,
      ...(warning ? { warning } : {}),
    })
  },
)

app.post(
  '/devices/:deviceId/sessions/:localSessionId/open',
  uuidAuth,
  async c => {
    const device = deviceFromParam(c)
    const localSessionId = c.req.param('localSessionId')
    if (!device || !localSessionId) {
      return errorJson(c, 400, 'bad_request', '缺少设备或会话 ID')
    }
    if (!isLocalSessionId(localSessionId)) {
      return errorJson(c, 400, 'bad_request', '无效的会话 ID')
    }
    const record = getDevice(device.id)
    if (!record) return errorJson(c, 404, 'device_not_found', '设备不存在')
    if (!isDeviceOnline(device.id)) {
      return errorJson(c, 503, 'device_offline', '设备离线，无法启动终端')
    }

    const session = await resolveSession(device.id, localSessionId)
    if (!session) {
      return errorJson(c, 404, 'session_not_found', '找不到该本地会话')
    }

    // Explicit user choice to replace a terminal that is running without
    // remote control. Never inferred: it stops the existing process.
    const body = (await c.req.json().catch(() => null)) as {
      force?: unknown
    } | null
    const force = body?.force === true

    // A fresh bridge report is enough to reuse; otherwise verify before
    // deciding to spawn, so a closed terminal is never treated as running.
    const cached = getRuntime(device.id, localSessionId)
    if (
      cached &&
      cached.state.bridgeSessionId &&
      cached.state.processAlive !== false &&
      isBridgeConnected(cached.state.bridgeSessionId) &&
      Date.now() - cached.reportedAt < 120_000
    ) {
      if (force) {
        requeueFailedOutbox(device.id, localSessionId)
        void flushOutbox(
          device.id,
          localSessionId,
          cached.state.bridgeSessionId,
        )
      }
      return c.json({
        state: 'ready',
        operationId: null,
        bridgeSessionId: cached.state.bridgeSessionId,
      })
    }

    let state: SessionRuntimeState | undefined
    try {
      const knownBridge = getMapping(device.id, localSessionId)?.bridgeSessionId
      const verified = (await requestDevice(
        device.id,
        'verify_state',
        {
          localSessionId,
          cwd: session.cwd,
          ...(knownBridge ? { bridgeSessionId: knownBridge } : {}),
        },
        8_000,
      )) as { state?: SessionRuntimeState }
      state = verified.state
      if (state) setRuntime(device.id, localSessionId, state)
    } catch {
      // Verification failure is not proof of anything; fall through to open.
    }

    if (state?.processAlive && !state.bridgeSessionId) {
      if (!force) {
        return errorJson(
          c,
          409,
          'already_running_without_bridge',
          '该会话已在终端中运行，但没有启用远程控制。请在那个终端执行 /remote-control，或选择“重启终端并启用远程控制”。',
        )
      }
    }
    if (state?.processAlive && state.bridgeSessionId) {
      setMapping({
        deviceId: device.id,
        localSessionId,
        bridgeSessionId: state.bridgeSessionId,
        cwd: session.cwd,
        updatedAt: Date.now(),
      })
      // The registry can report a bridge id for a process whose transport is
      // not attached right now (reconnecting, or closed minutes ago). Only
      // call it ready when the transport is actually live.
      if (isBridgeConnected(state.bridgeSessionId)) {
        if (force) {
          requeueFailedOutbox(device.id, localSessionId)
          void flushOutbox(
            device.id,
            localSessionId,
            state.bridgeSessionId,
          )
        }
        return c.json({
          state: 'ready',
          operationId: null,
          bridgeSessionId: state.bridgeSessionId,
        })
      }
      if (force) {
        return c.json({
          state: 'starting',
          operationId: (
            await ensureOpen(
              device.id,
              localSessionId,
              session.cwd,
              record.configDir,
              { force: true },
            )
          ).id,
          bridgeSessionId: state.bridgeSessionId,
          detail: '正在重启终端并启用远程控制',
        })
      }
      return c.json({
        state: 'bridge_disconnected',
        operationId: null,
        bridgeSessionId: state.bridgeSessionId,
        detail:
          '终端进程在运行，但远程控制连接尚未建立。请在该终端执行 /remote-control，或稍后重试。',
      })
    }

    try {
      const operation = await ensureOpen(
        device.id,
        localSessionId,
        session.cwd,
        record.configDir,
        { force },
      )
      return c.json({
        state: operation.state,
        operationId: operation.id,
        bridgeSessionId: operation.bridgeSessionId ?? null,
        detail: operation.detail ?? null,
      })
    } catch (error) {
      return errorJson(c, 503, 'device_unavailable', (error as Error).message)
    }
  },
)

app.post(
  '/devices/:deviceId/sessions/:localSessionId/messages',
  uuidAuth,
  async c => {
    const device = deviceFromParam(c)
    const localSessionId = c.req.param('localSessionId')
    if (!device || !localSessionId) {
      return errorJson(c, 400, 'bad_request', '缺少设备或会话 ID')
    }
    if (!isLocalSessionId(localSessionId)) {
      return errorJson(c, 400, 'bad_request', '无效的会话 ID')
    }
    const record = getDevice(device.id)
    if (!record) return errorJson(c, 404, 'device_not_found', '设备不存在')
    const body: unknown = await c.req.json().catch(() => null)
    if (
      !body ||
      typeof body !== 'object' ||
      !('text' in body) ||
      typeof body.text !== 'string' ||
      !body.text.trim() ||
      body.text.length > 100_000
    ) {
      return errorJson(c, 400, 'bad_request', '消息内容无效')
    }
    const text = body.text

    const session = await resolveSession(device.id, localSessionId)
    if (!session) {
      return errorJson(c, 404, 'session_not_found', '找不到该本地会话')
    }

    const mapping = getMapping(device.id, localSessionId)
    const runtime = getRuntime(device.id, localSessionId)
    if (
      mapping?.bridgeSessionId &&
      runtime?.state.bridgeSessionId === mapping.bridgeSessionId &&
      runtime.state.processAlive !== false &&
      // Immediate transport evidence: a closed terminal must fall through to
      // the queue + reopen path, not be reported as delivered.
      isBridgeConnected(mapping.bridgeSessionId) &&
      Date.now() - runtime.reportedAt < 120_000
    ) {
      const delivered = deliverNow(
        mapping.bridgeSessionId,
        device.id,
        localSessionId,
        text,
      )
      saveWebUserMessage(
        device.id,
        localSessionId,
        delivered.entry.id,
        text,
      )
      return c.json({
        state: delivered.state,
        messageId: delivered.entry.id,
        bridgeSessionId: mapping.bridgeSessionId,
      })
    }

    const entry = queueMessage(device.id, localSessionId, text)
    // Store it now: the terminal may take seconds to start, a laptop may be
    // asleep, and a reload in between must still show what was sent.
    saveWebUserMessage(device.id, localSessionId, entry.id, text)

    if (!isDeviceOnline(device.id)) {
      // Chatting must not dead-end when the machine is asleep. The message
      // stays queued; when the Spawner reconnects it opens the terminal and
      // the outbox flushes automatically.
      return c.json({
        state: 'queued',
        messageId: entry.id,
        operationId: null,
        detail: '设备离线，消息已保存，设备上线后会自动发送',
      })
    }

    try {
      const operation = await ensureOpen(
        device.id,
        localSessionId,
        session.cwd,
        record.configDir,
      )
      return c.json({
        state: 'queued',
        messageId: entry.id,
        operationId: operation.id,
      })
    } catch (error) {
      return c.json({
        state: 'queued',
        messageId: entry.id,
        operationId: null,
        detail: `消息已保存，但启动终端失败：${(error as Error).message}`,
      })
    }
  },
)

app.get('/operations/:operationId', uuidAuth, c => {
  const id = c.req.param('operationId')
  if (!id) return errorJson(c, 400, 'bad_request', '缺少操作 ID')
  const operation = getOperation(id)
  if (!operation) return errorJson(c, 404, 'operation_not_found', '操作不存在')
  return c.json(operation)
})

export default app
