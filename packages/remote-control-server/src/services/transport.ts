import { randomUUID } from 'node:crypto'
import { getEventBus, type SessionEvent } from '../transport/event-bus'

/**
 * Duplicate-tap guard.
 *
 * Prefer the message UUID: a retry of the same send keeps its UUID, while two
 * deliberate sends may legitimately contain identical text. Content remains
 * a short-window fallback for legacy clients that do not send an identifier.
 */
const USER_DEDUPE_WINDOW_MS = 2_500
const recentUserSends = new Map<
  string,
  { key: string; at: number; event: SessionEvent }
>()

/** FNV-1a over the full content; enough to compare two bursts cheaply. */
function hashContent(text: string): string {
  let hash = 0x811c9dc5
  for (let index = 0; index < text.length; index += 1) {
    hash ^= text.charCodeAt(index)
    hash = Math.imul(hash, 0x01000193)
  }
  return (hash >>> 0).toString(36)
}

function userContentKey(payload: unknown): string | undefined {
  if (!payload || typeof payload !== 'object') return undefined
  const record = payload as Record<string, unknown>
  if (typeof record.uuid === 'string' && record.uuid)
    return `uuid:${record.uuid}`
  const message = record.message as Record<string, unknown> | undefined
  const content = message?.content ?? record.content
  if (typeof content === 'string') return `s:${hashContent(content)}`
  if (Array.isArray(content)) return `a:${hashContent(JSON.stringify(content))}`
  return undefined
}

/**
 * Extract plain text from various message payload formats.
 * Handles:
 *   { content: "text" }
 *   { message: { role: "user", content: "text" } }
 *   { message: { content: [{type:"text",text:"..."}] } }
 */
function extractContent(payload: unknown): string {
  if (!payload || typeof payload !== 'object') {
    return typeof payload === 'string' ? payload : ''
  }

  const p = payload as Record<string, unknown>

  // Direct content field
  if (typeof p.content === 'string' && p.content) return p.content

  // message.content (child process format)
  const msg = p.message
  if (msg && typeof msg === 'object') {
    const mc = (msg as Record<string, unknown>).content
    if (typeof mc === 'string') return mc
    if (Array.isArray(mc)) {
      return mc
        .filter(
          (b: unknown) =>
            typeof b === 'object' &&
            b !== null &&
            (b as Record<string, unknown>).type === 'text',
        )
        .map(
          (b: Record<string, unknown>) =>
            (b as Record<string, unknown>).text || '',
        )
        .join('')
    }
  }

  return ''
}

/**
 * Normalize event payload into a flat structure with guaranteed `content` string.
 * Preserves original payload in `raw` field and keeps tool-specific fields.
 */
export function normalizePayload(
  type: string,
  payload: unknown,
): Record<string, unknown> {
  if (!payload || typeof payload !== 'object') {
    return { content: typeof payload === 'string' ? payload : '', raw: payload }
  }

  const p = payload as Record<string, unknown>
  const content = extractContent(payload)

  const normalized: Record<string, unknown> = {
    content,
    raw: payload,
  }

  if (typeof p.uuid === 'string' && p.uuid) normalized.uuid = p.uuid
  if (typeof p.isSynthetic === 'boolean') normalized.isSynthetic = p.isSynthetic
  if (typeof p.status === 'string') normalized.status = p.status
  if (typeof p.subtype === 'string') normalized.subtype = p.subtype

  // Preserve tool fields
  if (p.tool_name) normalized.tool_name = p.tool_name
  if (p.name) normalized.tool_name = p.name
  if (p.tool_input) normalized.tool_input = p.tool_input
  if (p.input) normalized.tool_input = p.input

  // Preserve permission fields
  if (p.request_id) normalized.request_id = p.request_id
  if (p.request) normalized.request = p.request
  if (p.approved !== undefined) normalized.approved = p.approved
  if (p.updated_input) normalized.updated_input = p.updated_input

  // Preserve message field for backward compat
  if (p.message) normalized.message = p.message

  if (type === 'task_state') {
    if (typeof p.task_list_id === 'string')
      normalized.task_list_id = p.task_list_id
    if (typeof p.taskListId === 'string') normalized.taskListId = p.taskListId
    if (Array.isArray(p.tasks)) normalized.tasks = p.tasks
  }

  return normalized
}

/** Publish an event to a session's bus (in-memory only) */
export function publishSessionEvent(
  sessionId: string,
  type: string,
  payload: unknown,
  direction: 'inbound' | 'outbound',
) {
  const bus = getEventBus(sessionId)

  // A reply from the CLI ends the burst: the next identical prompt is a new
  // instruction, not a duplicate tap.
  if (direction === 'inbound') recentUserSends.delete(sessionId)

  if (type === 'user' && direction === 'outbound') {
    const key = userContentKey(payload)
    const previous = recentUserSends.get(sessionId)
    if (
      key !== undefined &&
      previous &&
      previous.key === key &&
      Date.now() - previous.at < USER_DEDUPE_WINDOW_MS
    ) {
      // Slide the window: a burst of taps must not leak one through just
      // because it happened more than the window after the first one.
      previous.at = Date.now()
      return previous.event
    }
  }

  const eventId = randomUUID()

  const normalized = normalizePayload(type, payload)

  const event = bus.publish({
    id: eventId,
    sessionId,
    type,
    payload: normalized,
    direction,
  })

  if (type === 'user' && direction === 'outbound') {
    const key = userContentKey(payload)
    if (key !== undefined) {
      recentUserSends.set(sessionId, { key, at: Date.now(), event })
    }
  }

  return event
}
