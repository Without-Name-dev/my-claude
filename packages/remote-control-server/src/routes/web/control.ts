import { randomUUID } from 'node:crypto'
import { debug, error as logError } from '../../logger'
import { Hono } from 'hono'
import { uuidAuth } from '../../auth/middleware'
import { findMappingByBridgeId } from '../../device/state'
import { saveWebUserMessage } from '../../device/conversation-store'
import { deliverNow } from '../../device/operations'
import type { UserContent } from '../../device/protocol'
import {
  getSession,
  isSessionClosedStatus,
  resolveOwnedWebSessionId,
  updateSessionStatus,
} from '../../services/session'
import { publishSessionEvent } from '../../services/transport'
import { getEventBus } from '../../transport/event-bus'
import { isBridgeConnected } from '../../transport/ws-handler'

const app = new Hono()

type OwnershipCheckResult =
  | { error: true }
  | { error: true; reason: string }
  | {
      error: false
      session: NonNullable<ReturnType<typeof getSession>>
      sessionId: string
    }

function checkOwnership(
  c: { get: (key: string) => string | undefined },
  sessionId: string,
): OwnershipCheckResult {
  const uuid = c.get('uuid')!
  const resolvedSessionId = resolveOwnedWebSessionId(sessionId, uuid)
  if (!resolvedSessionId) {
    return { error: true }
  }
  const session = getSession(resolvedSessionId)
  if (!session) {
    return { error: true }
  }
  if (isSessionClosedStatus(session.status)) {
    return { error: true, reason: `Session is ${session.status}` }
  }
  return { error: false, session, sessionId: resolvedSessionId }
}

function closedSessionResponse(message: string) {
  return { error: { type: 'session_closed', message } }
}

/** POST /web/sessions/:id/events — Send user message to session */
app.post('/sessions/:id/events', uuidAuth, async c => {
  const requestedSessionId = c.req.param('id')!
  const ownership = checkOwnership(c, requestedSessionId)
  if (ownership.error) {
    const message =
      'reason' in ownership ? ownership.reason : 'Not your session'
    const status = 'reason' in ownership ? 409 : 403
    return c.json(
      'reason' in ownership
        ? closedSessionResponse(message)
        : { error: { type: 'forbidden', message } },
      status,
    )
  }
  const { sessionId } = ownership

  const body = await c.req.json()
  const eventType = body.type || 'user'
  // One identity from the start: the outbound event, the optimistic row below
  // and the CLI's echo are deduped by uuid. A client that omits it (older
  // builds) used to get two ids — the event's and a fresh randomUUID for the
  // stored row — so the phone rendered the same send twice after a reload.
  if (
    eventType === 'user' &&
    body.isSynthetic !== true &&
    (typeof body.uuid !== 'string' || body.uuid === '')
  ) {
    body.uuid = randomUUID()
  }
  // Everything published here (messages, slash commands, control frames) is
  // replayed to the bridge on its next attach — see handleWebSocketOpen — so
  // accepting while the transport is briefly down is queueing, not losing.
  debug(
    `[RC-DEBUG] web -> server: POST /web/sessions/${sessionId}/events type=${eventType}`,
  )
  const mapping =
    eventType === 'user' && body.isSynthetic !== true
      ? findMappingByBridgeId(sessionId)
      : undefined
  let event
  if (mapping) {
    if (typeof body.content !== 'string' && !Array.isArray(body.content)) {
      return c.json(
        { error: { type: 'bad_request', message: '消息内容无效' } },
        400,
      )
    }
    try {
      const delivered = await deliverNow(
        sessionId,
        mapping.deviceId,
        mapping.localSessionId,
        body.content as UserContent,
        body.uuid,
      )
      event = delivered.event
    } catch (error) {
      logError(`[RC-DEBUG] durable web message failed:`, error)
      return c.json(
        {
          error: { type: 'send_failed', message: '保存或发送消息失败，请重试' },
        },
        503,
      )
    }
  } else {
    event = publishSessionEvent(sessionId, eventType, body, 'outbound')
  }
  debug(
    `[RC-DEBUG] web -> server: outbound event id=${event?.id ?? 'existing'} type=${eventType} subscribers=${getEventBus(sessionId).subscriberCount()}`,
  )
  // A live-chat user message only lives in the rolling in-memory event bus —
  // a long streaming turn evicts it, so a reload mid-run made the sender's
  // own bubble vanish. Persist it under the client uuid: reloads restore it
  // from the conversation cache, and the CLI's transcript echo upserts the
  // same primary key when the message is actually submitted (mid-turn
  // consumption as a queued-command attachment never echoes, so this row is
  // the only durable record).
  if (eventType === 'user' && body.isSynthetic !== true) {
    if (mapping) {
      const uuid =
        typeof body.uuid === 'string' && body.uuid ? body.uuid : randomUUID()
      let text = ''
      let images: Array<{ mimeType: string; data: string }> | undefined
      const content = body.content
      if (typeof content === 'string') {
        text = content
      } else if (Array.isArray(content)) {
        for (const block of content) {
          if (!block || typeof block !== 'object') continue
          const record = block as Record<string, unknown>
          if (record.type === 'text' && typeof record.text === 'string') {
            text += record.text
          } else if (
            record.type === 'image' &&
            typeof (record.source as Record<string, unknown> | undefined)
              ?.media_type === 'string' &&
            typeof (record.source as Record<string, unknown>)?.data === 'string'
          ) {
            const source = record.source as { media_type: string; data: string }
            images ??= []
            images.push({ mimeType: source.media_type, data: source.data })
          }
        }
      }
      if (text.trim() || (images && images.length > 0)) {
        saveWebUserMessage(
          mapping.deviceId,
          mapping.localSessionId,
          uuid,
          text,
          images,
        )
      }
    }
  }
  return c.json({ status: 'ok', event }, 200)
})

/** POST /web/sessions/:id/control — Send control request (permission approval etc) */
app.post('/sessions/:id/control', uuidAuth, async c => {
  const requestedSessionId = c.req.param('id')!
  const ownership = checkOwnership(c, requestedSessionId)
  if (ownership.error) {
    const message =
      'reason' in ownership ? ownership.reason : 'Not your session'
    const status = 'reason' in ownership ? 409 : 403
    return c.json(
      'reason' in ownership
        ? closedSessionResponse(message)
        : { error: { type: 'forbidden', message } },
      status,
    )
  }
  const { sessionId } = ownership

  // Permission answers queue like everything else: the bridge attach replay
  // delivers them, and a late answer to an expired request is ignored by the
  // CLI without side effects.
  const body = await c.req.json()
  const event = publishSessionEvent(
    sessionId,
    body.type || 'control_request',
    body,
    'outbound',
  )
  return c.json({ status: 'ok', event }, 200)
})

/** POST /web/sessions/:id/interrupt — Interrupt session */
app.post('/sessions/:id/interrupt', uuidAuth, async c => {
  const requestedSessionId = c.req.param('id')!
  const ownership = checkOwnership(c, requestedSessionId)
  if (ownership.error) {
    const message =
      'reason' in ownership ? ownership.reason : 'Not your session'
    const status = 'reason' in ownership ? 409 : 403
    return c.json(
      'reason' in ownership
        ? closedSessionResponse(message)
        : { error: { type: 'forbidden', message } },
      status,
    )
  }
  const { sessionId } = ownership

  // The interrupt itself queues like any other control frame (the attach
  // replay delivers it). But claiming "idle" with no live bridge was a lie the
  // status indicator inherited — the session state must reflect evidence.
  publishSessionEvent(
    sessionId,
    'interrupt',
    { action: 'interrupt' },
    'outbound',
  )
  if (isBridgeConnected(sessionId)) {
    updateSessionStatus(sessionId, 'idle')
  }
  return c.json({ status: 'ok' }, 200)
})

export default app
