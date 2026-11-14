import { debug, error as logError } from '../logger'
import type { Context } from 'hono'
import type { SessionEvent } from './event-bus'
import { getEventBus } from './event-bus'
import { toClientPayload } from './client-payload'
import { storeGetSession } from '../store'

export interface SSEWriter {
  send(event: SessionEvent): void
  close(): void
}

/** Keep large image blocks once in the web event rather than in both the
 * normalized message and its raw copy. Other raw metadata is retained. */
function webEventPayload(event: SessionEvent): unknown {
  if (
    event.type !== 'user' ||
    !event.payload ||
    typeof event.payload !== 'object'
  )
    return event.payload
  const payload = event.payload as Record<string, unknown>
  const message = payload.message
  const raw = payload.raw
  if (
    !message ||
    typeof message !== 'object' ||
    !raw ||
    typeof raw !== 'object'
  )
    return payload
  const content = (message as Record<string, unknown>).content
  if (
    !Array.isArray(content) ||
    !content.some(
      block => block && typeof block === 'object' && block.type === 'image',
    )
  )
    return payload
  const rawRecord = raw as Record<string, unknown>
  if (rawRecord.message !== message) return payload
  const { message: _duplicate, ...rest } = rawRecord
  return { ...payload, raw: rest }
}

export function createSSEWriter(c: Context): SSEWriter {
  const stream = new ReadableStream({
    start(controller) {
      const encoder = new TextEncoder()
      c.req.raw.signal.addEventListener('abort', () => {
        controller.close()
      })

      // Store encoder and controller for later use
      ;(c as any)._sseEncoder = encoder
      ;(c as any)._sseController = controller
    },
  })

  return {
    send(event: SessionEvent) {
      const encoder = (c as any)._sseEncoder as TextEncoder
      const controller = (c as any)
        ._sseController as ReadableStreamDefaultController
      if (!encoder || !controller) return
      const data = JSON.stringify({
        type: event.type,
        payload: webEventPayload(event),
        direction: event.direction,
        seqNum: event.seqNum,
      })
      const msg = `id: ${event.seqNum}\nevent: message\ndata: ${data}\n\n`
      controller.enqueue(encoder.encode(msg))
    },
    close() {
      const controller = (c as any)
        ._sseController as ReadableStreamDefaultController
      controller?.close()
    },
  }
}

/** Create SSE response stream for a session */
export function createSSEStream(c: Context, sessionId: string, fromSeqNum = 0) {
  const bus = getEventBus(sessionId)

  const stream = new ReadableStream({
    start(controller) {
      const encoder = new TextEncoder()

      // Send historical events if reconnecting. When the cursor cannot be
      // replayed in full (evicted buffer, restarted bus) say so explicitly:
      // silently streaming the surviving tail leaves a hole in the transcript
      // that no later event can fill.
      if (fromSeqNum > 0) {
        if (bus.hasGapFrom(fromSeqNum)) {
          const data = JSON.stringify({ seqNum: bus.getLastSeqNum() })
          controller.enqueue(encoder.encode(`event: resync\ndata: ${data}\n\n`))
        } else {
          const missed = bus.getEventsSince(fromSeqNum)
          for (const event of missed) {
            const data = JSON.stringify({
              type: event.type,
              payload: webEventPayload(event),
              direction: event.direction,
              seqNum: event.seqNum,
            })
            controller.enqueue(
              encoder.encode(
                `id: ${event.seqNum}\nevent: message\ndata: ${data}\n\n`,
              ),
            )
          }
        }
      }

      // Send initial keepalive
      controller.enqueue(encoder.encode(': keepalive\n\n'))

      // A subscriber that attaches mid-turn missed the session_status event
      // that started it; without this snapshot the web stays "idle" until the
      // next activity event, which a long quiet tool call may not produce for
      // minutes. Deliberately carries no id/seqNum: it must not move the
      // client's replay cursor, and duplicate-seqNum filtering would drop it
      // on reconnects that replayed up to the bus tail.
      const record = storeGetSession(sessionId)
      if (record?.status) {
        const data = JSON.stringify({
          type: 'session_status',
          payload: {
            status: record.status,
            // updatedAt moves on every bridge message (touchSession); only
            // statusChangedAt is the true start of the current status, so the
            // reloaded page shows the turn's real elapsed time.
            since: (record.statusChangedAt ?? record.updatedAt).getTime(),
          },
          direction: 'inbound',
        })
        controller.enqueue(encoder.encode(`event: message\ndata: ${data}\n\n`))
      }

      // Subscribe to new events
      const unsub = bus.subscribe(event => {
        const data = JSON.stringify({
          type: event.type,
          payload: webEventPayload(event),
          direction: event.direction,
          seqNum: event.seqNum,
        })
        try {
          debug(
            `[RC-DEBUG] SSE -> web: sessionId=${sessionId} type=${event.type} dir=${event.direction} seq=${event.seqNum}`,
          )
          controller.enqueue(
            encoder.encode(
              `id: ${event.seqNum}\nevent: message\ndata: ${data}\n\n`,
            ),
          )
        } catch {
          unsub()
        }
      })

      // Keepalive interval
      const keepalive = setInterval(() => {
        try {
          controller.enqueue(encoder.encode(': keepalive\n\n'))
        } catch {
          clearInterval(keepalive)
          unsub()
        }
      }, 15000)

      // Cleanup on abort
      c.req.raw.signal.addEventListener('abort', () => {
        unsub()
        clearInterval(keepalive)
        try {
          controller.close()
        } catch {
          // already closed
        }
      })
    },
  })

  return new Response(stream, {
    headers: {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      Connection: 'keep-alive',
      'X-Accel-Buffering': 'no',
    },
  })
}

function toWorkerClientPayload(event: SessionEvent): Record<string, unknown> {
  if (
    event.type === 'permission_response' ||
    event.type === 'control_response' ||
    event.type === 'control_request' ||
    event.type === 'interrupt'
  ) {
    return toClientPayload(event)
  }

  const normalized =
    event.payload && typeof event.payload === 'object'
      ? (event.payload as Record<string, unknown>)
      : undefined
  const raw =
    normalized?.raw &&
    typeof normalized.raw === 'object' &&
    !Array.isArray(normalized.raw)
      ? (normalized.raw as Record<string, unknown>)
      : undefined
  const payload: Record<string, unknown> = {
    ...(raw ?? normalized ?? {}),
    type: event.type,
  }

  if (event.type === 'user') {
    const message = payload.message
    if (!message || typeof message !== 'object' || !('content' in message)) {
      const content =
        typeof normalized?.content === 'string'
          ? normalized.content
          : typeof payload.content === 'string'
            ? payload.content
            : typeof event.payload === 'string'
              ? event.payload
              : ''
      payload.content = content
      payload.message = { content }
    }
  }

  return payload
}

function toWorkerClientFrame(event: SessionEvent): string {
  const data = JSON.stringify({
    event_id: event.id,
    sequence_num: event.seqNum,
    event_type: event.type,
    source: 'client',
    payload: toWorkerClientPayload(event),
    created_at: new Date(event.createdAt).toISOString(),
  })
  return `id: ${event.seqNum}\nevent: client_event\ndata: ${data}\n\n`
}

/** Create CCR worker SSE stream (client_event frames, outbound events only). */
export function createWorkerEventStream(
  c: Context,
  sessionId: string,
  fromSeqNum = 0,
) {
  const bus = getEventBus(sessionId)

  const stream = new ReadableStream({
    start(controller) {
      const encoder = new TextEncoder()

      if (fromSeqNum > 0) {
        const missed = bus
          .getEventsSince(fromSeqNum)
          .filter(event => event.direction === 'outbound')
        for (const event of missed) {
          controller.enqueue(encoder.encode(toWorkerClientFrame(event)))
        }
      }

      controller.enqueue(encoder.encode(': keepalive\n\n'))

      const unsub = bus.subscribe(event => {
        if (event.direction !== 'outbound') {
          return
        }
        try {
          controller.enqueue(encoder.encode(toWorkerClientFrame(event)))
        } catch {
          unsub()
        }
      })

      const keepalive = setInterval(() => {
        try {
          controller.enqueue(encoder.encode(': keepalive\n\n'))
        } catch {
          clearInterval(keepalive)
          unsub()
        }
      }, 15000)

      c.req.raw.signal.addEventListener('abort', () => {
        unsub()
        clearInterval(keepalive)
        try {
          controller.close()
        } catch {
          // already closed
        }
      })
    },
  })

  return new Response(stream, {
    headers: {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      Connection: 'keep-alive',
      'X-Accel-Buffering': 'no',
    },
  })
}
