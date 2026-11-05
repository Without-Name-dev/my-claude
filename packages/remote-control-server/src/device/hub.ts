/**
 * Live Spawner connections.
 *
 * One WebSocket per device. Requests are correlated by id and always have a
 * timeout: a Spawner that accepted a request but stopped answering must not
 * leave an HTTP handler or an open operation hanging forever. Server→device
 * requests never carry conversation content, and the device's reply is
 * untrusted input that callers validate.
 */

import { randomUUID } from 'node:crypto'
import type { WSContext } from 'hono/ws'
import { log } from '../logger'
import type { DeviceEvent, DeviceMethod, DeviceServerMessage } from './protocol'
import type { DeviceRecord } from './state'
import { touchDevice } from './state'

type PendingRequest = {
  resolve: (value: unknown) => void
  reject: (error: Error) => void
  timer: ReturnType<typeof setTimeout>
  method: DeviceMethod
}

type DeviceConnection = {
  deviceId: string
  name: string
  ws: WSContext
  /**
   * Identifies this particular connection. The socket wrapper passed to
   * Hono's onClose callback is not guaranteed to be the same object stored at
   * attach time, so lifecycle callbacks match on this token instead of `ws`.
   */
  token: string
  connectedAt: number
  lastSeenAt: number
  pending: Map<string, PendingRequest>
}

const connections = new Map<string, DeviceConnection>()
const eventListeners = new Set<(deviceId: string, event: DeviceEvent) => void>()

/** Connections that stopped sending heartbeats are dropped, not trusted. */
const HEARTBEAT_SWEEP_MS = 15_000
const HEARTBEAT_STALE_MS = 60_000

let sweepTimer: ReturnType<typeof setInterval> | undefined

function ensureSweep(): void {
  if (sweepTimer) return
  sweepTimer = setInterval(() => {
    const now = Date.now()
    for (const connection of [...connections.values()]) {
      if (now - connection.lastSeenAt <= HEARTBEAT_STALE_MS) continue
      log(`[device] heartbeat timeout: ${connection.deviceId}`)
      closeConnection(connection, 4008, 'heartbeat timeout')
    }
  }, HEARTBEAT_SWEEP_MS)
}

export function isDeviceOnline(deviceId: string): boolean {
  return connections.has(deviceId)
}

export function onlineDeviceIds(): string[] {
  return [...connections.keys()]
}

export function connectionCount(): number {
  return connections.size
}

/**
 * Register a verified device connection. A second connection for the same
 * device replaces the first: the newest process is the one the operator is
 * actually running.
 */
export function attachDevice(
  ws: WSContext,
  device: DeviceRecord,
): { ok: true; token: string } | { ok: false; reason: string } {
  ensureSweep()
  const existing = connections.get(device.id)
  if (existing) {
    closeConnection(existing, 4009, 'replaced by a newer connection')
  }

  const connection: DeviceConnection = {
    deviceId: device.id,
    name: device.name,
    ws,
    token: randomUUID(),
    connectedAt: Date.now(),
    lastSeenAt: Date.now(),
    pending: new Map(),
  }
  connections.set(device.id, connection)
  touchDevice(device.id)
  log(`[device] connected: ${device.id} (${device.name})`)
  return { ok: true, token: connection.token }
}

/**
 * Detach only if the closing socket still owns the device entry. A replaced
 * connection's onClose must not tear down the newer connection.
 */
export function detachDevice(
  deviceId: string,
  token: string | undefined,
): void {
  const connection = connections.get(deviceId)
  if (!connection) return
  if (token !== undefined && connection.token !== token) return
  closeConnection(connection, 4000, 'device disconnected', false)
}

function closeConnection(
  connection: DeviceConnection,
  code: number,
  reason: string,
  closeSocket = true,
): void {
  const current = connections.get(connection.deviceId)
  if (current !== connection) return
  connections.delete(connection.deviceId)
  for (const [id, pending] of connection.pending) {
    clearTimeout(pending.timer)
    connection.pending.delete(id)
    pending.reject(new Error(`设备连接已断开（${pending.method}）`))
  }
  if (closeSocket) {
    try {
      connection.ws.close(code, reason)
    } catch {
      // Already closed.
    }
  }
  log(`[device] disconnected: ${connection.deviceId} (${reason})`)
}

export function markSeen(deviceId: string): void {
  const connection = connections.get(deviceId)
  if (!connection) return
  connection.lastSeenAt = Date.now()
  touchDevice(deviceId)
}

export function resolveRequest(
  deviceId: string,
  requestId: string,
  result: unknown,
  error?: { code: string; message: string },
): void {
  const connection = connections.get(deviceId)
  if (!connection) return
  const pending = connection.pending.get(requestId)
  if (!pending) return
  connection.pending.delete(requestId)
  clearTimeout(pending.timer)
  connection.lastSeenAt = Date.now()
  if (error) {
    pending.reject(new Error(error.message || error.code))
  } else {
    pending.resolve(result)
  }
}

/**
 * Send a request and wait for the device's response.
 *
 * `timeoutMs` is intentionally caller-controlled: list/read are quick, while
 * a caller that only wants an acknowledgement (open_session) should use a
 * short timeout because progress arrives as events.
 */
export function requestDevice(
  deviceId: string,
  method: DeviceMethod,
  params: Record<string, unknown> = {},
  timeoutMs = 20_000,
): Promise<unknown> {
  const connection = connections.get(deviceId)
  if (!connection) {
    return Promise.reject(new Error('设备不在线'))
  }
  if (connection.ws.readyState !== 1) {
    return Promise.reject(new Error('设备连接尚未就绪'))
  }

  const id = randomUUID()
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      connection.pending.delete(id)
      reject(new Error(`设备响应超时（${method}）`))
    }, timeoutMs)
    connection.pending.set(id, { resolve, reject, timer, method })
    const message: DeviceServerMessage = { type: 'request', id, method, params }
    try {
      connection.ws.send(JSON.stringify(message))
    } catch (error) {
      clearTimeout(timer)
      connection.pending.delete(id)
      reject(new Error(`无法发送设备请求：${(error as Error).message}`))
    }
  })
}

export function onDeviceEvent(
  listener: (deviceId: string, event: DeviceEvent) => void,
): () => void {
  eventListeners.add(listener)
  return () => eventListeners.delete(listener)
}

export function dispatchDeviceEvent(
  deviceId: string,
  event: DeviceEvent,
): void {
  for (const listener of eventListeners) {
    try {
      listener(deviceId, event)
    } catch (error) {
      log(`[device] event listener failed: ${(error as Error).message}`)
    }
  }
}

export function closeAllDeviceConnections(): void {
  for (const connection of [...connections.values()]) {
    closeConnection(connection, 1001, 'server shutdown')
  }
  if (sweepTimer) {
    clearInterval(sweepTimer)
    sweepTimer = undefined
  }
  eventListeners.clear()
}
