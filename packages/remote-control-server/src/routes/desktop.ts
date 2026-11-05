/**
 * Device Spawner WebSocket endpoint.
 *
 * Mounted at /desktop/ws. Authentication is two-tier:
 * - a valid shared API key may enroll a new device (hello carries a secret);
 * - thereafter the device authenticates with its own token as the bearer.
 *
 * Every message from a device is untrusted input: validate shape and bounds
 * before it reaches state or the hub.
 */

import { Hono } from 'hono'
import type { WSContext, WSMessageReceive } from 'hono/ws'
import { validateApiKey } from '../auth/api-key'
import { apiKeyAuth, extractWebSocketAuthToken } from '../auth/middleware'
import { log } from '../logger'
import { upgradeWebSocket } from '../transport/ws-shared'
import { handleSizedWsPayload } from '../transport/ws-payload'
import {
  attachDevice,
  detachDevice,
  dispatchDeviceEvent,
  markSeen,
  resolveRequest,
} from '../device/hub'
import { warmCatalog } from '../device/prewarm'
import {
  authenticateDeviceToken,
  getDevice,
  hashSecret,
  listDevices,
  setRuntime,
  touchDevice,
  upsertDevice,
  verifySecret,
  type DeviceRecord,
} from '../device/state'
import {
  isBridgeSessionId,
  isDeviceId,
  isLocalSessionId,
  isRecord,
  numberField,
  stringField,
  type DeviceCapability,
  type DeviceClientMessage,
  type DeviceEvent,
  type OpenFailureCode,
  type OpenOperationState,
  type SessionRuntimeState,
} from '../device/protocol'
import {
  handleDeviceEvent,
  resumeQueuedSessions,
} from '../device/operations'

const app = new Hono()

const MAX_SECRET_LENGTH = 256
const MIN_SECRET_LENGTH = 32
const MAX_CAPABILITIES = 16

const OPERATION_STATES = new Set<OpenOperationState>([
  'accepted',
  'starting',
  'terminal_spawned',
  'bridge_connecting',
  'ready',
  'failed',
  'waiting_for_user',
])

const FAILURE_CODES = new Set<OpenFailureCode>([
  'session_not_found',
  'cwd_mismatch',
  'config_dir_mismatch',
  'already_running_without_bridge',
  'spawn_failed',
  'bridge_timeout',
  'process_exited',
  'busy',
  'unsupported',
])

function validRuntimeState(value: unknown): value is SessionRuntimeState {
  if (!isRecord(value)) return false
  return (
    typeof value.localSessionId === 'string' &&
    isLocalSessionId(value.localSessionId) &&
    typeof value.cwd === 'string' &&
    value.cwd.length <= 4096 &&
    typeof value.processAlive === 'boolean' &&
    typeof value.verifiedAt === 'number' &&
    (value.bridgeSessionId === undefined ||
      (typeof value.bridgeSessionId === 'string' &&
        isBridgeSessionId(value.bridgeSessionId))) &&
    (value.source === 'registry' ||
      value.source === 'managed' ||
      value.source === 'scan')
  )
}

function parseCapabilities(value: unknown): DeviceCapability[] {
  if (!Array.isArray(value)) return []
  const allowed = new Set<string>([
    'list_projects',
    'list_sessions',
    'read_session',
    'open_session',
    'verify_state',
    'heartbeat',
  ])
  const result: DeviceCapability[] = []
  for (const item of value.slice(0, MAX_CAPABILITIES)) {
    if (typeof item === 'string' && allowed.has(item)) {
      result.push(item as DeviceCapability)
    }
  }
  return result
}

function parseEvent(value: unknown): DeviceEvent | undefined {
  if (!isRecord(value) || typeof value.type !== 'string') return undefined
  switch (value.type) {
    case 'operation': {
      const operationId = stringField(value, 'operationId', 200)
      const localSessionId = stringField(value, 'localSessionId', 128)
      const rawState = stringField(value, 'state', 40)
      if (!operationId || !localSessionId || !rawState) return undefined
      if (!isLocalSessionId(localSessionId)) return undefined
      if (!OPERATION_STATES.has(rawState as OpenOperationState))
        return undefined
      const rawFailure = stringField(value, 'failureCode', 80)
      const failureCode =
        rawFailure && FAILURE_CODES.has(rawFailure as OpenFailureCode)
          ? (rawFailure as OpenFailureCode)
          : undefined
      return {
        type: 'operation',
        operationId,
        localSessionId,
        state: rawState as OpenOperationState,
        detail: stringField(value, 'detail', 2000),
        failureCode,
        bridgeSessionId: stringField(value, 'bridgeSessionId', 200),
        pid: numberField(value, 'pid'),
      }
    }
    case 'session_state': {
      if (!Array.isArray(value.states)) return undefined
      const states = value.states.filter(validRuntimeState)
      return { type: 'session_state', states }
    }
    case 'catalog_changed':
      return {
        type: 'catalog_changed',
        reason: stringField(value, 'reason', 200),
      }
    default:
      return undefined
  }
}

app.get(
  '/ws',
  upgradeWebSocket(c => {
    const token = extractWebSocketAuthToken(c)
    const sharedKeyValid = validateApiKey(token)
    const enrolled = token ? authenticateDeviceToken(token) : undefined
    if (!sharedKeyValid && !enrolled) {
      return {
        onOpen(_event: Event, ws: WSContext) {
          ws.close(4003, 'unauthorized')
        },
      }
    }

    let deviceId: string | undefined
    let connectionToken: string | undefined

    return {
      onMessage(event: { data: WSMessageReceive }, ws: WSContext) {
        handleSizedWsPayload(ws, '[Device]', 'device', event.data, data => {
          let message: DeviceClientMessage
          try {
            const value: unknown = JSON.parse(data)
            if (!isRecord(value) || typeof value.type !== 'string') return
            message = value as DeviceClientMessage
          } catch {
            return
          }

          if (!deviceId) {
            if (message.type !== 'hello') return
            const hello = message
            if (hello.protocol !== 1) {
              ws.close(4000, 'unsupported protocol')
              return
            }
            if (!isDeviceId(hello.deviceId)) {
              ws.close(4000, 'invalid device id')
              return
            }
            const helloRecord = hello as unknown as Record<string, unknown>
            const name = stringField(helloRecord, 'name', 128)
            if (!name || !name.trim()) {
              ws.close(4000, 'invalid device name')
              return
            }
            const configDir = stringField(helloRecord, 'configDir', 1024)
            if (!configDir) {
              ws.close(4000, 'invalid config dir')
              return
            }

            const existing = getDevice(hello.deviceId)
            if (enrolled) {
              // Authenticated as this device already; the claimed id must match.
              if (enrolled.id !== hello.deviceId) {
                ws.close(4003, 'device id mismatch')
                return
              }
            } else if (!sharedKeyValid) {
              ws.close(4003, 'unauthorized')
              return
            }

            if (existing) {
              if (hello.secret !== undefined) {
                if (
                  typeof hello.secret !== 'string' ||
                  !verifySecret(existing, hello.secret)
                ) {
                  ws.close(4003, 'invalid device secret')
                  return
                }
              } else if (!enrolled) {
                ws.close(4003, 'device secret required for enrollment key')
                return
              }
              const updated: DeviceRecord = {
                ...existing,
                name: name.trim(),
                platform: hello.platform || existing.platform,
                configDir,
                capabilities: parseCapabilities(hello.capabilities),
                lastSeenAt: Date.now(),
              }
              upsertDevice(updated)
            } else {
              if (!sharedKeyValid) {
                ws.close(4003, 'unknown device')
                return
              }
              if (
                typeof hello.secret !== 'string' ||
                hello.secret.length < MIN_SECRET_LENGTH ||
                hello.secret.length > MAX_SECRET_LENGTH
              ) {
                ws.close(4000, 'device secret required')
                return
              }
              upsertDevice({
                id: hello.deviceId,
                name: name.trim(),
                platform: hello.platform || 'unknown',
                configDir,
                secretHash: hashSecret(hello.secret),
                secretPrefix: hello.secret.slice(0, 6),
                capabilities: parseCapabilities(hello.capabilities),
                createdAt: Date.now(),
                lastSeenAt: Date.now(),
              })
              log(`[device] enrolled: ${hello.deviceId} (${name.trim()})`)
            }

            const record = getDevice(hello.deviceId)
            if (!record) {
              ws.close(4000, 'enrollment failed')
              return
            }
            const attached = attachDevice(ws, record)
            if (!attached.ok) {
              ws.close(4009, attached.reason)
              return
            }
            deviceId = record.id
            connectionToken = attached.token
            touchDevice(record.id)
            ws.send(
              JSON.stringify({
                type: 'ready',
                deviceId: record.id,
                serverTime: Date.now(),
              }),
            )
            // The first web request should not pay for a cold disk scan.
            warmCatalog(record.id)
            // Messages sent while the machine was offline: open their
            // terminals now that it is back.
            void resumeQueuedSessions(record.id, record.configDir)
            return
          }

          switch (message.type) {
            case 'heartbeat': {
              markSeen(deviceId)
              if (Array.isArray(message.running)) {
                for (const state of message.running) {
                  if (validRuntimeState(state)) {
                    setRuntime(deviceId, state.localSessionId, state)
                  }
                }
              }
              ws.send(JSON.stringify({ type: 'heartbeat_ack', ts: Date.now() }))
              return
            }
            case 'response': {
              if (typeof message.id !== 'string') return
              if (message.ok) {
                resolveRequest(deviceId, message.id, message.result)
              } else {
                const error = isRecord(message.error) ? message.error : {}
                resolveRequest(deviceId, message.id, undefined, {
                  code: stringField(error, 'code', 200) ?? 'device_error',
                  message:
                    stringField(error, 'message', 2000) ?? '设备请求失败',
                })
              }
              return
            }
            case 'event': {
              const parsed = parseEvent(message.event)
              if (!parsed) return
              markSeen(deviceId)
              if (parsed.type === 'session_state') {
                for (const state of parsed.states) {
                  setRuntime(deviceId, state.localSessionId, state)
                }
              }
              if (parsed.type === 'operation') {
                handleDeviceEvent(deviceId, parsed)
              }
              dispatchDeviceEvent(deviceId, parsed)
              return
            }
            case 'hello':
              // A second hello on the same socket is not a supported upgrade.
              return
            default:
              return
          }
        })
      },
      onClose() {
        if (deviceId) detachDevice(deviceId, connectionToken)
        deviceId = undefined
        connectionToken = undefined
      },
      onError() {
        if (deviceId) detachDevice(deviceId, connectionToken)
        deviceId = undefined
        connectionToken = undefined
      },
    }
  }),
)

/** Enrolled devices; ops tooling only, so it requires the shared API key. */
app.get('/devices', apiKeyAuth, c =>
  c.json(listDevices().map(({ secretHash: _hash, ...rest }) => rest)),
)

export default app
