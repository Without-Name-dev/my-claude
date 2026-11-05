import { randomUUID } from 'node:crypto'
import type { WSContext } from 'hono/ws'
import { getEventBus } from './event-bus'
import type { SessionEvent } from './event-bus'
import { publishSessionEvent } from '../services/transport'
import { log, error as logError } from '../logger'
import { toClientPayload } from './client-payload'
import { config } from '../config'
import { recordBridgeEvent } from '../device/conversation-store'
import { findMappingByBridgeId } from '../device/state'
import { touchSession } from '../services/session'

// Per-connection cleanup, keyed by sessionId (only one WS per session)
interface CleanupEntry {
  unsub: () => void
  keepalive: ReturnType<typeof setInterval>
  ws: WSContext
  /** Identifies this connection; onClose wrappers are not reference-equal. */
  token: string
  openTime: number
}
const cleanupBySession = new Map<string, CleanupEntry>()
const lastDeliveredByBus = new WeakMap<object, number>()

// Track all active WS connections for graceful shutdown
const activeConnections = new Set<WSContext>()

// Device-side hooks (outbox flush, mapping updates) stay out of this module to
// avoid a ws-handler ⇄ operations import cycle.
const attachListeners = new Set<(bridgeSessionId: string) => void>()

export function onBridgeAttached(
  listener: (bridgeSessionId: string) => void,
): () => void {
  attachListeners.add(listener)
  return () => attachListeners.delete(listener)
}

// Server-side keepalive interval (configurable via RCS_WS_KEEPALIVE_INTERVAL).
// Sends data frames to keep reverse proxies from closing idle connections.
const SERVER_KEEPALIVE_INTERVAL_MS = (config.wsKeepaliveInterval || 20) * 1000

/**
 * Convert internal EventBus event -> SDK message for bridge client.
 */
function toSDKMessage(event: SessionEvent): string {
  // NDJSON format: each message MUST end with \n so the child process's
  // line-based parser can split messages correctly.
  return JSON.stringify(toClientPayload(event)) + '\n'
}

/**
 * Whether a bridge transport is currently attached to this session. This is
 * immediate evidence (the WS close removes the entry), unlike the Spawner's
 * periodic process report.
 */
export function isBridgeConnected(sessionId: string): boolean {
  return cleanupBySession.has(sessionId)
}

/** Called from onOpen — subscribes to event bus, forwards outbound events to bridge WS.
 *  Returns a connection token that the caller must pass back on close. */
export function handleWebSocketOpen(ws: WSContext, sessionId: string): string {
  const openTime = Date.now()
  const token = randomUUID()
  log(`[RC-DEBUG] [WS] Open session=${sessionId}`)
  activeConnections.add(ws)

  // If there's an existing connection for this session, clean it up first
  const existing = cleanupBySession.get(sessionId)
  if (existing) {
    log(`[WS] Replacing existing connection for session=${sessionId}`)
    existing.unsub()
    clearInterval(existing.keepalive)
    activeConnections.delete(existing.ws)
    // The old CLI process is gone (or reconnecting). Close its socket so it
    // cannot keep writing into this session through a half-open connection.
    try {
      existing.ws.close(4009, 'replaced by a newer bridge connection')
    } catch {
      // Already closed.
    }
  }

  const bus = getEventBus(sessionId)

  // The bridge writes over HTTP and reads over WS. Replaying inbound events
  // feeds its own replies back into the bridge on every reconnect.
  const missed = bus.getEventsSince(lastDeliveredByBus.get(bus) ?? 0)
  if (missed.length > 0) {
    log(`[WS] Replaying ${missed.length} missed event(s)`)
    for (const event of missed) {
      if (ws.readyState !== 1) break
      if (event.direction !== 'outbound') continue
      try {
        ws.send(toSDKMessage(event))
        lastDeliveredByBus.set(bus, event.seqNum)
      } catch {
        // ignore send errors during replay
      }
    }
  }

  const unsub = bus.subscribe((event: SessionEvent) => {
    if (ws.readyState !== 1) return
    if (event.direction !== 'outbound') return
    try {
      const sdkMsg = toSDKMessage(event)
      log(
        `[RC-DEBUG] [WS] -> bridge (outbound): type=${event.type} len=${sdkMsg.length} msg=${sdkMsg.slice(0, 300)}`,
      )
      ws.send(sdkMsg)
      lastDeliveredByBus.set(bus, event.seqNum)
    } catch (err) {
      logError('[RC-DEBUG] [WS] send error:', err)
    }
  })

  const keepalive = setInterval(() => {
    if (ws.readyState !== 1) {
      clearInterval(keepalive)
      return
    }
    try {
      ws.send('{"type":"keep_alive"}\n')
    } catch {
      clearInterval(keepalive)
    }
  }, SERVER_KEEPALIVE_INTERVAL_MS)

  cleanupBySession.set(sessionId, {
    unsub,
    keepalive,
    ws,
    token,
    openTime,
  })
  // Tell web subscribers immediately that the terminal is reachable, so the
  // UI never has to poll before sending a message.
  publishSessionEvent(sessionId, 'bridge_state', { connected: true }, 'inbound')
  // A live bridge is proof the session is active; the disconnect monitor must
  // not mark it inactive based on an old `updatedAt`.
  touchSession(sessionId)
  // Flush queued messages and confirm replayed ones. Deferred so this call
  // finishes before listeners publish into the bus they are subscribing to.
  queueMicrotask(() => {
    for (const listener of attachListeners) {
      try {
        listener(sessionId)
      } catch (error) {
        logError('[WS] bridge attach listener failed:', error)
      }
    }
  })
  return token
}

/**
 * Called from onMessage — bridge sends newline-delimited JSON.
 */
export function handleWebSocketMessage(
  ws: WSContext,
  sessionId: string,
  data: string,
  token?: string,
) {
  // A replaced socket must not keep feeding this session. When the caller can
  // pass the connection token we check ownership; older callers keep working.
  if (token !== undefined) {
    const current = cleanupBySession.get(sessionId)
    if (!current || current.token !== token) {
      log(`[WS] Dropping message from a replaced connection session=${sessionId}`)
      return
    }
  }
  const lines = data.split('\n').filter(l => l.trim())
  for (const line of lines) {
    try {
      ingestBridgeMessage(sessionId, JSON.parse(line))
    } catch (err) {
      logError('[WS] parse error:', err)
    }
  }
}

/** Called from onClose — unsubscribes from event bus */
export function handleWebSocketClose(
  ws: WSContext,
  sessionId: string,
  code?: number,
  reason?: string,
  token?: string,
) {
  activeConnections.delete(ws)

  const entry = cleanupBySession.get(sessionId)
  const duration = entry ? Math.round((Date.now() - entry.openTime) / 1000) : -1

  log(
    `[WS] Close session=${sessionId} code=${code ?? 'none'} reason=${reason || '(none)'} duration=${duration}s`,
  )

  // Prefer the connection token: Hono may hand each callback a different
  // WSContext wrapper, so identity alone can miss the close and leave
  // `isBridgeConnected` stuck true. The readyState fallback keeps older
  // callers/tests working.
  const matches = token
    ? entry?.token === token
    : entry?.ws === ws || entry?.ws.readyState !== 1
  if (entry && matches) {
    entry.unsub()
    clearInterval(entry.keepalive)
    cleanupBySession.delete(sessionId)
    publishSessionEvent(
      sessionId,
      'bridge_state',
      { connected: false },
      'inbound',
    )
  }
}

/**
 * Derive event type from a child process message that may lack an explicit
 * `type` field. The child's --print --output-format stream-json mode sends:
 *   {"message":{"role":"user",...},"uuid":"..."}       → type "user"
 *   {"message":{"role":"assistant",...},"uuid":"..."}  → type "assistant"
 *   {"subtype":"success","uuid":"...","result":"..."}  → type "result"
 */
function deriveEventType(msg: Record<string, unknown>): string {
  if (msg.type && typeof msg.type === 'string') return msg.type

  // Child process stream-json format: message.role determines type
  const message = msg.message as Record<string, unknown> | undefined
  if (message && typeof message.role === 'string') {
    return message.role // "user", "assistant", "system"
  }

  // Result message
  if (msg.subtype || msg.result !== undefined) return 'result'

  // System/init message
  if (msg.session_id) return 'system'

  return 'unknown'
}

/**
 * Parse a single SDK message from bridge -> publish to EventBus as inbound.
 */
export function ingestBridgeMessage(
  sessionId: string,
  msg: Record<string, unknown>,
) {
  if (msg.type === 'keep_alive') return

  // Evidence of life: prevents the disconnect sweep from closing a session
  // that is actively producing messages but has no other status updates.
  touchSession(sessionId)

  const eventType = deriveEventType(msg)

  log(
    `[RC-DEBUG] [WS] <- bridge (inbound): sessionId=${sessionId} type=${eventType}${msg.uuid ? ` uuid=${msg.uuid}` : ''} msg=${JSON.stringify(msg).slice(0, 300)}`,
  )

  let payload: unknown

  if (eventType === 'assistant' || eventType === 'partial_assistant') {
    const message = msg.message as Record<string, unknown> | undefined
    const content = message?.content
    // Extract text from content blocks for simple display
    let text = ''
    if (typeof content === 'string') {
      text = content
    } else if (Array.isArray(content)) {
      text = content
        .filter(
          (b: unknown) =>
            b &&
            typeof b === 'object' &&
            'type' in (b as Record<string, unknown>) &&
            (b as Record<string, unknown>).type === 'text',
        )
        .map(
          (b: Record<string, unknown>) =>
            (b as Record<string, unknown>).text || '',
        )
        .join('')
    }
    payload = { message: msg.message, uuid: msg.uuid, content: text }
  } else if (eventType === 'user') {
    payload = {
      message: msg.message,
      uuid: msg.uuid,
      ...(typeof msg.isSynthetic === 'boolean'
        ? { isSynthetic: msg.isSynthetic }
        : {}),
    }
  } else if (eventType === 'system') {
    // system/init carries the model, permission mode, model list and command
    // details the web renders pickers from. Keep the full message instead of
    // reducing it to {message, uuid}.
    payload = { ...msg }
  } else if (eventType === 'control_request') {
    payload = { request_id: msg.request_id, request: msg.request }
  } else if (eventType === 'control_response') {
    payload = { response: msg.response }
  } else if (eventType === 'result' || eventType === 'result_success') {
    payload = { subtype: msg.subtype, uuid: msg.uuid, result: msg.result }
  } else {
    payload = msg
  }

  // Persist the conversation under the *device* session it belongs to, so the
  // web can still read it after the terminal (and the in-memory event bus)
  // are gone. `--resume` may write new transcript messages under a different
  // local id, but the mapping keeps the stable device-session key.
  if (eventType === 'user' || eventType === 'assistant') {
    const mapping = findMappingByBridgeId(sessionId)
    if (mapping && payload && typeof payload === 'object') {
      try {
        recordBridgeEvent(
          mapping.deviceId,
          mapping.localSessionId,
          eventType,
          payload as Record<string, unknown>,
        )
      } catch {
        // Conversation persistence must never break live delivery.
      }
    }
  }

  publishSessionEvent(sessionId, eventType, payload, 'inbound')
}

/**
 * Gracefully close all active WebSocket connections.
 */
export function closeAllConnections(): void {
  const count = activeConnections.size
  if (count === 0) return

  log(`[WS] Gracefully closing ${count} active connection(s)...`)
  for (const [sessionId, entry] of cleanupBySession) {
    try {
      entry.unsub()
      clearInterval(entry.keepalive)
      if (entry.ws.readyState === 1) {
        entry.ws.close(1001, 'server_shutdown')
      }
    } catch {
      // ignore errors during shutdown
    }
  }
  cleanupBySession.clear()
  activeConnections.clear()
  log('[WS] All connections closed')
}
