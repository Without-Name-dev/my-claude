import { randomUUID } from 'node:crypto'
import { getEventBus, type SessionEvent } from '../transport/event-bus'
import { isBridgeConnected, onBridgeAttached } from '../transport/ws-handler'
import { publishSessionEvent } from '../services/transport'
import {
  findMappingByBridgeId,
  flushState,
  getMapping,
  getSessionSettings,
  onSessionMapping,
  putSessionSettings,
  removeSessionSettings,
  type SessionSettings,
  type SessionSettingsValues,
} from './state'

type Field = keyof SessionSettingsValues
const fields: Field[] = ['model', 'permissionMode', 'effort', 'ultracode']
const applied = new Map<
  string,
  { deviceId: string; sessionId: string; revision: number }
>()
const applying = new Map<string, Promise<void>>()
const applyingOwner = new Map<string, { deviceId: string; sessionId: string }>()
const applyingFields = new Map<string, Set<Field>>()
const watched = new WeakSet<object>()
const observations = new Map<string, SessionSettingsValues>()
function appliedTo(bridge: string, deviceId: string, sessionId: string) {
  const value = applied.get(bridge)
  return value?.deviceId === deviceId && value.sessionId === sessionId
    ? value
    : undefined
}
/** Recheck synchronously at publication, after all asynchronous persistence. */
export function sessionSettingsConfirmed(
  deviceId: string,
  sessionId: string,
  bridge: string,
): boolean {
  if (getMapping(deviceId, sessionId)?.bridgeSessionId !== bridge) return false
  const settings = getSessionSettings(deviceId, sessionId)
  if (!settings) return true
  return (
    isBridgeConnected(bridge) &&
    settings.pending.length === 0 &&
    appliedTo(bridge, deviceId, sessionId)?.revision === settings.revision
  )
}
const readyListeners = new Set<
  (deviceId: string, sessionId: string, bridge: string) => void
>()
export function onSessionSettingsReady(
  listener: (deviceId: string, sessionId: string, bridge: string) => void,
): () => void {
  readyListeners.add(listener)
  return () => readyListeners.delete(listener)
}

function raw(event: SessionEvent): Record<string, unknown> {
  const payload = event.payload as Record<string, unknown> | null
  return (payload?.raw ?? payload ?? {}) as Record<string, unknown>
}

/** Validate at the trust boundary; never store credentials in session settings. */
export function validateSettings(input: unknown): SessionSettingsValues {
  if (!input || typeof input !== 'object' || Array.isArray(input))
    throw new Error('设置格式无效')
  const object = input as Record<string, unknown>
  const keys = Object.keys(object)
  if (!keys.length || keys.some(key => !fields.includes(key as Field)))
    throw new Error('设置项无效')
  if (
    'model' in object &&
    (typeof object.model !== 'string' ||
      !object.model ||
      object.model.length > 512)
  )
    throw new Error('模型无效')
  if (
    'effort' in object &&
    !['auto', 'low', 'medium', 'high', 'xhigh', 'max'].includes(
      String(object.effort),
    )
  )
    throw new Error('思考强度无效')
  if (
    'permissionMode' in object &&
    !['default', 'acceptEdits', 'plan', 'auto'].includes(
      String(object.permissionMode),
    )
  )
    throw new Error('权限模式无效')
  if ('ultracode' in object && typeof object.ultracode !== 'boolean')
    throw new Error('Ultracode 设置无效')
  return object as SessionSettingsValues
}

export async function updateSessionSettings(
  deviceId: string,
  sessionId: string,
  patch: SessionSettingsValues,
): Promise<SessionSettings> {
  const previous = getSessionSettings(deviceId, sessionId)
  const next: SessionSettings = {
    values: { ...previous?.values, ...patch },
    pending: [
      ...new Set([
        ...(previous?.pending ?? []),
        ...(Object.keys(patch) as Field[]),
      ]),
    ],
    revision: (previous?.revision ?? 0) + 1,
    updatedAt: Date.now(),
  }
  putSessionSettings(deviceId, sessionId, next)
  try {
    await flushState({ throwOnError: true })
  } catch (error) {
    // Roll back only our own revision; a later concurrent edit must survive.
    if (getSessionSettings(deviceId, sessionId)?.revision === next.revision) {
      if (previous) putSessionSettings(deviceId, sessionId, previous)
      else removeSessionSettings(deviceId, sessionId)
    }
    throw error
  }
  const bridge = getMapping(deviceId, sessionId)?.bridgeSessionId
  if (bridge && isBridgeConnected(bridge)) {
    try {
      await ensureSessionSettings(deviceId, sessionId, bridge)
    } catch {
      /* The saved error and pending fields are returned to the client. */
    }
  }
  return getSessionSettings(deviceId, sessionId) ?? next
}

function captureInit(bridge: string, event: SessionEvent): void {
  if (event.type !== 'system' || event.direction !== 'inbound') return
  const m = raw(event)
  if (m.subtype !== 'init') return
  const mapping = findMappingByBridgeId(bridge)
  if (!mapping) return
  // During /resume the terminal can publish its new init before the spawner's
  // registry report retargets this bridge. Never save it into the old session.
  if (
    typeof m.session_id === 'string' &&
    m.session_id !== mapping.localSessionId
  )
    return
  const old = getSessionSettings(mapping.deviceId, mapping.localSessionId)
  const observed: SessionSettingsValues = {}
  if (typeof m.model_selection === 'string') observed.model = m.model_selection
  else if (typeof m.model === 'string' && m.model) observed.model = m.model
  if (typeof m.effort === 'string') observed.effort = m.effort
  if (typeof m.permissionMode === 'string')
    observed.permissionMode = m.permissionMode
  if (typeof m.ultracode_active === 'boolean')
    observed.ultracode = m.ultracode_active
  const previousObserved = observations.get(bridge)
  observations.set(bridge, observed)
  // After a rejected restore, an explicit change at the terminal can correct
  // a pending field. A repeated old snapshot is not evidence of a user edit.
  if (old?.error && previousObserved && !applying.has(bridge)) {
    const corrected = old.pending.filter(
      key =>
        observed[key] !== undefined &&
        previousObserved[key] !== undefined &&
        observed[key] !== previousObserved[key],
    )
    if (corrected.length) {
      const values = { ...old.values }
      for (const key of corrected)
        Object.assign(values, { [key]: observed[key] })
      putSessionSettings(mapping.deviceId, mapping.localSessionId, {
        ...old,
        values,
        pending: old.pending.filter(key => !corrected.includes(key)),
        revision: old.revision + 1,
        updatedAt: Date.now(),
        error: undefined,
      })
      queueMicrotask(
        () =>
          void ensureSessionSettings(
            mapping.deviceId,
            mapping.localSessionId,
            bridge,
          ).catch(() => {}),
      )
      return
    }
  }
  // A new bridge's startup snapshot may reflect defaults; restore the stored
  // values before accepting its snapshots as authoritative.
  if (old && !appliedTo(bridge, mapping.deviceId, mapping.localSessionId))
    return
  const values = { ...old?.values }
  for (const key of fields) {
    if (
      old?.pending.includes(key) ||
      applyingFields.get(bridge)?.has(key) ||
      observed[key] === undefined
    )
      continue
    Object.assign(values, { [key]: observed[key] })
  }
  if (JSON.stringify(values) === JSON.stringify(old?.values)) return
  const next: SessionSettings = {
    values,
    pending: old?.pending ?? [],
    revision: (old?.revision ?? 0) + 1,
    updatedAt: Date.now(),
    error: old?.error,
  }
  putSessionSettings(mapping.deviceId, mapping.localSessionId, next)
  if (!applying.has(bridge))
    applied.set(bridge, {
      deviceId: mapping.deviceId,
      sessionId: mapping.localSessionId,
      revision: next.revision,
    })
}

function watch(bridge: string): void {
  const bus = getEventBus(bridge)
  if (watched.has(bus)) return
  watched.add(bus)
  bus.subscribe(event => captureInit(bridge, event))
  for (const snapshot of bus.getStateSnapshots()) captureInit(bridge, snapshot)
}

function control(
  bridge: string,
  request: Record<string, unknown>,
  timeoutMs: number,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const id = randomUUID()
    const bus = getEventBus(bridge)
    const timer = setTimeout(() => {
      unsubscribe()
      reject(new Error('等待终端确认设置超时'))
    }, timeoutMs)
    const unsubscribe = bus.subscribe(event => {
      if (event.direction !== 'inbound' || event.type !== 'control_response')
        return
      const response = raw(event).response as
        | Record<string, unknown>
        | undefined
      if (response?.request_id !== id) return
      clearTimeout(timer)
      unsubscribe()
      if (response.subtype === 'error')
        reject(
          new Error(
            typeof response.error === 'string'
              ? response.error
              : '终端拒绝设置',
          ),
        )
      else if (response.subtype === 'success') resolve()
      else reject(new Error('终端返回了无效的设置确认'))
    })
    try {
      if (!isBridgeConnected(bridge)) throw new Error('终端连接已断开')
      publishSessionEvent(
        bridge,
        'control_request',
        { request_id: id, request },
        'outbound',
      )
    } catch (error) {
      clearTimeout(timer)
      unsubscribe()
      reject(error)
    }
  })
}

/** All message delivery paths wait for the same per-bridge reconciliation. */
export function ensureSessionSettings(
  deviceId: string,
  sessionId: string,
  bridge: string,
): Promise<void> {
  watch(bridge)
  const existing = applying.get(bridge)
  if (existing) {
    const owner = applyingOwner.get(bridge)
    if (owner?.deviceId === deviceId && owner.sessionId === sessionId)
      return existing
    // A /resume can reuse the bridge while a previous session is still being
    // confirmed. The old result never authorizes the new session's messages.
    return existing
      .catch(() => {})
      .then(() => ensureSessionSettings(deviceId, sessionId, bridge))
  }
  applyingOwner.set(bridge, { deviceId, sessionId })
  const job = (async () => {
    // Keep the entire handshake below the web client's 30s request timeout.
    // Otherwise a timed-out send could still reach the terminal afterwards.
    const deadline = Date.now() + 20_000
    while (true) {
      if (getMapping(deviceId, sessionId)?.bridgeSessionId !== bridge)
        throw new Error('终端连接已切换，请重试')
      const settings = getSessionSettings(deviceId, sessionId)
      if (!settings) {
        if (isBridgeConnected(bridge))
          applied.set(bridge, { deviceId, sessionId, revision: 0 })
        return
      }
      if (!isBridgeConnected(bridge)) throw new Error('终端连接已断开')
      if (
        appliedTo(bridge, deviceId, sessionId)?.revision ===
          settings.revision &&
        settings.pending.length === 0
      )
        return
      const revision = settings.revision
      const values = { ...settings.values }
      // A new bridge needs the complete snapshot. On an already synchronized
      // bridge, apply only edited fields so a web effort change cannot undo a
      // simultaneous terminal model or permission change.
      const selected = new Set<Field>(
        appliedTo(bridge, deviceId, sessionId) ? settings.pending : fields,
      )
      if (selected.has('model') && values.ultracode !== undefined)
        selected.add('ultracode')
      applyingFields.set(bridge, selected)
      const requests: Record<string, unknown>[] = []
      if (selected.has('model') && values.model !== undefined)
        requests.push({ subtype: 'set_model', model: values.model })
      if (selected.has('permissionMode') && values.permissionMode !== undefined)
        requests.push({
          subtype: 'set_permission_mode',
          mode: values.permissionMode,
        })
      if (selected.has('effort') && values.effort !== undefined)
        requests.push({ subtype: 'set_effort', effort: values.effort })
      if (selected.has('ultracode') && values.ultracode !== undefined)
        requests.push({ subtype: 'set_ultracode', enabled: values.ultracode })
      for (const request of requests) {
        if (getMapping(deviceId, sessionId)?.bridgeSessionId !== bridge)
          throw new Error('终端连接已切换，请重试')
        const remaining = deadline - Date.now()
        if (remaining <= 0) throw new Error('等待终端确认设置超时')
        await control(bridge, request, Math.min(12_000, remaining))
      }
      if (getMapping(deviceId, sessionId)?.bridgeSessionId !== bridge)
        throw new Error('终端连接已切换，请重试')
      const current = getSessionSettings(deviceId, sessionId)
      if (!current) return
      if (current.revision !== revision) continue
      putSessionSettings(deviceId, sessionId, {
        ...current,
        pending: [],
        error: undefined,
        updatedAt: Date.now(),
      })
      await flushState({ throwOnError: true })
      applied.set(bridge, { deviceId, sessionId, revision })
      for (const listener of readyListeners) {
        try {
          listener(deviceId, sessionId, bridge)
        } catch {
          /* Other listeners must still run. */
        }
      }
      return
    }
  })()
    .catch(error => {
      const current = getSessionSettings(deviceId, sessionId)
      if (
        current &&
        getMapping(deviceId, sessionId)?.bridgeSessionId === bridge
      )
        putSessionSettings(deviceId, sessionId, {
          ...current,
          error: error instanceof Error ? error.message : '恢复设置失败',
        })
      throw error
    })
    .finally(() => {
      applying.delete(bridge)
      applyingOwner.delete(bridge)
      applyingFields.delete(bridge)
    })
  applying.set(bridge, job)
  return job
}

function connected(bridge: string): void {
  watch(bridge)
  const mapping = findMappingByBridgeId(bridge)
  if (mapping) {
    const owner = applyingOwner.get(bridge)
    const inFlight = applying.get(bridge)
    if (
      inFlight &&
      owner &&
      (owner.deviceId !== mapping.deviceId ||
        owner.sessionId !== mapping.localSessionId)
    ) {
      void inFlight
        .catch(() => {})
        .then(() => {
          const current = findMappingByBridgeId(bridge)
          if (
            current?.deviceId === mapping.deviceId &&
            current.localSessionId === mapping.localSessionId &&
            isBridgeConnected(bridge)
          )
            connected(bridge)
        })
      return
    }
    for (const snapshot of getEventBus(bridge).getStateSnapshots())
      captureInit(bridge, snapshot)
    void ensureSessionSettings(
      mapping.deviceId,
      mapping.localSessionId,
      bridge,
    ).catch(() => {})
  }
}
onBridgeAttached(connected)
onSessionMapping(mapping => {
  if (isBridgeConnected(mapping.bridgeSessionId))
    connected(mapping.bridgeSessionId)
})
