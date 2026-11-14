/**
 * Durable conversation store for device sessions.
 *
 * ---------------------------------------------------------------------------
 * Why SQLite here (and not for everything)
 * ---------------------------------------------------------------------------
 * The RCS has two very different kinds of state:
 *
 *   1. Control-plane state — device enrollment, token hashes, the
 *      local→bridge session mapping, the catalog cache, the outbox. It is
 *      tiny (a handful of devices, a few hundred small rows), rewritten
 *      wholesale, and lost safely if the Spawner reconnects. It stays in the
 *      JSON document in `state.ts`, where a 250ms debounce + atomic rename is
 *      simpler than a schema.
 *
 *   2. Conversation content — every user/assistant message, indefinitely
 *      large, append-heavy, and queried by (device, session) with pagination.
 *      This is exactly what SQLite is for: WAL writes, indexed range reads,
 *      dedupe by primary key, and bounded retention. Keeping it in the JSON
 *      document would mean rewriting the entire conversation on every token.
 *
 * The store is the server-side source of truth for *displayed* conversation:
 *
 *   - local transcripts read from the Spawner are cached here;
 *   - live bridge user/assistant messages are appended here;
 *   - messages accepted from the web are recorded immediately, so a queued
 *     message survives a reload even if the terminal has not started yet.
 *
 * Rows are keyed by (device, local session), which stays stable across
 * `--resume` even when the CLI writes new messages under a different local
 * transcript id.
 *
 * ---------------------------------------------------------------------------
 * Ordering
 * ---------------------------------------------------------------------------
 * `seq` is insertion order, not conversation order: backfilling older
 * transcript pages inserts old messages *after* newer ones. All reads
 * therefore sort by `sort_ms` (the message's own timestamp, with missing
 * timestamps interpolated inside the page) and use `seq` only as a tiebreak.
 * The pagination cursor is `"<sort_ms>|<seq>"`, opaque to clients.
 */

import { Database } from 'bun:sqlite'
import type { Statement } from 'bun:sqlite'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { log } from '../logger'
import type {
  TranscriptBlock,
  TranscriptMessage,
  TranscriptUsage,
} from './protocol'
import { dataDir } from './state'

export type StoredMessage = TranscriptMessage & {
  source: 'transcript' | 'bridge' | 'web'
}

export type StoredPage = {
  messages: StoredMessage[]
  before?: string
  hasMore: boolean
  total: number
}

type Row = {
  seq: number
  sort_ms: number
  message_id: string
  role: string
  text: string
  timestamp: string | null
  blocks: string
  usage?: string | null
  source: string
}

const CONVERSATION_DB = 'conversations.db'
const MAX_STORED_PER_SESSION = 5000

let db: Database | undefined
let insertStmt: Statement | undefined
let insertIfAbsentStmt: Statement | undefined
let listStmt: Statement | undefined
let listBeforeStmt: Statement | undefined
let countStmt: Statement | undefined
let minSortStmt: Statement | undefined
let maxSortStmt: Statement | undefined
let listSeqBeforeStmt: Statement | undefined
let backfillDoneStmt: Statement | undefined
let markBackfillStmt: Statement | undefined
let oldestStmt: Statement | undefined
let trimStmt: Statement | undefined

function ensureDb(): Database {
  if (db) return db
  const dir = dataDir()
  mkdirSync(dir, { recursive: true })
  db = new Database(join(dir, CONVERSATION_DB), { create: true })
  db.exec('PRAGMA journal_mode = WAL')
  db.exec('PRAGMA synchronous = NORMAL')
  db.exec(`
    CREATE TABLE IF NOT EXISTS conversation_messages (
      seq INTEGER PRIMARY KEY AUTOINCREMENT,
      device_id TEXT NOT NULL,
      local_session_id TEXT NOT NULL,
      message_id TEXT NOT NULL,
      role TEXT NOT NULL,
      text TEXT NOT NULL DEFAULT '',
      timestamp TEXT,
      sort_ms INTEGER NOT NULL DEFAULT 0,
      blocks TEXT NOT NULL DEFAULT '[]',
      usage TEXT,
      source TEXT NOT NULL,
      created_at INTEGER NOT NULL,
      UNIQUE (device_id, local_session_id, message_id)
    );
    CREATE INDEX IF NOT EXISTS idx_conversation_session
      ON conversation_messages (device_id, local_session_id, sort_ms, seq);
    CREATE INDEX IF NOT EXISTS idx_conversation_backfill
      ON conversation_messages (device_id, local_session_id, source, sort_ms);
    CREATE TABLE IF NOT EXISTS backfill_state (
      device_id TEXT NOT NULL,
      local_session_id TEXT NOT NULL,
      completed_at INTEGER NOT NULL,
      PRIMARY KEY (device_id, local_session_id)
    );
  `)
  // Databases created before the usage column: add it in place. A nullable
  // column with no default keeps existing rows valid without a rebuild.
  const columns = db
    .query('PRAGMA table_info(conversation_messages)')
    .all() as { name: string }[]
  if (!columns.some(column => column.name === 'usage')) {
    db.exec('ALTER TABLE conversation_messages ADD COLUMN usage TEXT')
  }

  // A transcript page may later fill in richer text/tool blocks for a row the
  // bridge already stored (or vice versa). Never downgrade non-empty content.
  // Blocks compare by encoded length: a text-only re-read of a row the bridge
  // stored with image blocks is "non-empty" but strictly poorer, and the old
  // <> '[]' test let it strip the images back out.
  insertStmt = db.query(`
    INSERT INTO conversation_messages
      (device_id, local_session_id, message_id, role, text, timestamp, sort_ms, blocks, usage, source, created_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    ON CONFLICT(device_id, local_session_id, message_id) DO UPDATE SET
      text = CASE WHEN excluded.text <> '' THEN excluded.text ELSE text END,
      timestamp = COALESCE(excluded.timestamp, timestamp),
      sort_ms = excluded.sort_ms,
      blocks = CASE WHEN length(excluded.blocks) > length(blocks) THEN excluded.blocks ELSE blocks END,
      usage = COALESCE(excluded.usage, usage),
      source = excluded.source
  `)
  // The web's optimistic row is the weakest source: if the CLI echoed it
  // first (same uuid), keep the bridge row's richer content untouched.
  insertIfAbsentStmt = db.query(`
    INSERT INTO conversation_messages
      (device_id, local_session_id, message_id, role, text, timestamp, sort_ms, blocks, usage, source, created_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    ON CONFLICT(device_id, local_session_id, message_id) DO NOTHING
  `)
  listStmt = db.query(`
    SELECT seq, sort_ms, message_id, role, text, timestamp, blocks, usage, source
    FROM conversation_messages
    WHERE device_id = ? AND local_session_id = ?
    ORDER BY sort_ms DESC, seq DESC LIMIT ?
  `)
  listBeforeStmt = db.query(`
    SELECT seq, sort_ms, message_id, role, text, timestamp, blocks, usage, source
    FROM conversation_messages
    WHERE device_id = ? AND local_session_id = ?
      AND (sort_ms < ? OR (sort_ms = ? AND seq < ?))
    ORDER BY sort_ms DESC, seq DESC LIMIT ?
  `)
  countStmt = db.query(`
    SELECT COUNT(*) AS n FROM conversation_messages
    WHERE device_id = ? AND local_session_id = ?
  `)
  minSortStmt = db.query(`
    SELECT MIN(sort_ms) AS n FROM conversation_messages
    WHERE device_id = ? AND local_session_id = ?
  `)
  maxSortStmt = db.query(`
    SELECT MAX(sort_ms) AS n FROM conversation_messages
    WHERE device_id = ? AND local_session_id = ?
  `)
  listSeqBeforeStmt = db.query(`
    SELECT seq, sort_ms, message_id, role, text, timestamp, blocks, source
    FROM conversation_messages
    WHERE device_id = ? AND local_session_id = ? AND seq < ?
    ORDER BY seq DESC LIMIT ?
  `)
  backfillDoneStmt = db.query(`
    SELECT 1 AS ok FROM backfill_state
    WHERE device_id = ? AND local_session_id = ?
  `)
  markBackfillStmt = db.query(`
    INSERT INTO backfill_state (device_id, local_session_id, completed_at)
    VALUES (?, ?, ?)
    ON CONFLICT(device_id, local_session_id) DO NOTHING
  `)
  oldestStmt = db.query(`
    SELECT message_id FROM conversation_messages
    WHERE device_id = ? AND local_session_id = ? AND source = 'transcript'
    ORDER BY sort_ms ASC, seq ASC LIMIT 1
  `)
  trimStmt = db.query(`
    DELETE FROM conversation_messages
    WHERE seq IN (
      SELECT seq FROM conversation_messages
      WHERE device_id = ? AND local_session_id = ?
      ORDER BY sort_ms DESC, seq DESC LIMIT -1 OFFSET ?
    )
  `)
  return db
}

/** Open the database at startup so configuration errors surface immediately. */
export function initConversationStore(): void {
  try {
    ensureDb()
    log(`[conversation] SQLite ready at ${join(dataDir(), CONVERSATION_DB)}`)
  } catch (error) {
    log(`[conversation] SQLite unavailable: ${(error as Error).message}`)
  }
}

function safeJson(value: unknown): string {
  try {
    return JSON.stringify(value)
  } catch {
    return '[]'
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function blocksFromContent(content: unknown): TranscriptBlock[] {
  if (typeof content === 'string') {
    return content.length > 0 ? [{ type: 'text', text: content }] : []
  }
  if (!Array.isArray(content)) return []
  const blocks: TranscriptBlock[] = []
  for (const item of content) {
    if (!isRecord(item)) continue
    const type = item.type
    if (type === 'text' && typeof item.text === 'string' && item.text.length) {
      blocks.push({ type: 'text', text: item.text })
    } else if (
      type === 'thinking' &&
      typeof item.thinking === 'string' &&
      item.thinking.length
    ) {
      blocks.push({ type: 'thinking', text: item.thinking })
    } else if (type === 'tool_use') {
      blocks.push({
        type: 'tool_use',
        ...(typeof item.id === 'string' ? { id: item.id } : {}),
        name: typeof item.name === 'string' ? item.name : 'tool',
        input: item.input,
      })
    } else if (type === 'tool_result') {
      const text =
        typeof item.content === 'string'
          ? item.content
          : Array.isArray(item.content)
            ? item.content
                .filter(
                  part =>
                    isRecord(part) &&
                    part.type === 'text' &&
                    typeof part.text === 'string',
                )
                .map(part => (part as Record<string, unknown>).text as string)
                .join('')
            : ''
      if (text) {
        blocks.push({
          type: 'tool_result',
          ...(typeof item.tool_use_id === 'string'
            ? { toolUseId: item.tool_use_id }
            : {}),
          text,
          ...(item.is_error === true ? { isError: true } : {}),
        })
      }
    } else if (type === 'image' && isRecord(item.source)) {
      const source = item.source
      // Photos sent from the phone live in the transcript as base64 blocks;
      // dropping them here made every image vanish from history on reload.
      if (
        source.type === 'base64' &&
        typeof source.data === 'string' &&
        source.data.length > 0 &&
        source.data.length <= 6_000_000
      ) {
        blocks.push({
          type: 'image',
          mimeType:
            typeof source.media_type === 'string'
              ? source.media_type
              : 'image/png',
          data: source.data,
        })
      }
    }
  }
  return blocks
}

function textFromBlocks(blocks: TranscriptBlock[]): string {
  return blocks
    .filter(
      (block): block is { type: 'text'; text: string } => block.type === 'text',
    )
    .map(block => block.text)
    .join('')
}

function textFromContent(content: unknown): string {
  return textFromBlocks(blocksFromContent(content))
}

function writeRow(
  deviceId: string,
  localSessionId: string,
  message: {
    id: string
    role: 'user' | 'assistant'
    text: string
    timestamp?: string
    sortMs: number
    blocks: TranscriptBlock[]
    usage?: TranscriptUsage | null
    source: StoredMessage['source']
  },
  ifAbsent = false,
): void {
  try {
    ensureDb()
    ;(ifAbsent ? insertIfAbsentStmt! : insertStmt!).run(
      deviceId,
      localSessionId,
      message.id,
      message.role,
      message.text,
      message.timestamp ?? null,
      message.sortMs,
      safeJson(message.blocks),
      message.usage ? safeJson(message.usage) : null,
      message.source,
      Date.now(),
    )
  } catch (error) {
    log(`[conversation] write failed: ${(error as Error).message}`)
  }
}

/** Fill missing timestamps from neighbours so page order is preserved. */
function effectiveSortTimes(
  messages: TranscriptMessage[],
  fallbackMs: number,
): number[] {
  const times = messages.map(message => {
    if (!message.timestamp) return Number.NaN
    const parsed = Date.parse(message.timestamp)
    return Number.isFinite(parsed) ? parsed : Number.NaN
  })

  let last = Number.NaN
  for (let index = 0; index < times.length; index += 1) {
    if (!Number.isNaN(times[index]!)) last = times[index]!
    else if (!Number.isNaN(last)) times[index] = last
  }
  let next = Number.NaN
  for (let index = times.length - 1; index >= 0; index -= 1) {
    if (!Number.isNaN(times[index]!)) next = times[index]!
    else if (!Number.isNaN(next)) times[index] = next
  }
  for (let index = 0; index < times.length; index += 1) {
    if (Number.isNaN(times[index]!)) {
      times[index] = fallbackMs - (times.length - index)
    }
  }
  return times
}

/** Cache a page read from the Spawner. Safe to call repeatedly. */
export function saveTranscript(
  deviceId: string,
  localSessionId: string,
  messages: TranscriptMessage[],
  position: 'auto' | 'newest' | 'older' = 'auto',
): void {
  if (messages.length === 0) return
  const database = ensureDb()
  try {
    // Missing timestamps are interpolated relative to the page's position:
    // a newest-page refresh must stay after the newest cached row, while a
    // backfilled older page must stay before the oldest. The old blanket
    // "before the oldest" anchor inverted timestamp-less newest pages.
    const minRow = minSortStmt!.get(deviceId, localSessionId) as {
      n: number | null
    } | null
    const maxRow = maxSortStmt!.get(deviceId, localSessionId) as {
      n: number | null
    } | null
    const min = typeof minRow?.n === 'number' && minRow.n > 0 ? minRow.n : 0
    const max = typeof maxRow?.n === 'number' && maxRow.n > 0 ? maxRow.n : 0
    const fallback =
      position === 'newest'
        ? (max || Date.now()) + messages.length + 1
        : position === 'older' && min > 0
          ? min
          : min > 0
            ? min
            : Date.now()
    const times = effectiveSortTimes(messages, fallback)

    database.exec('BEGIN')
    messages.forEach((message, index) => {
      if (!message.id) return
      insertStmt!.run(
        deviceId,
        localSessionId,
        message.id,
        message.role,
        message.text ?? '',
        message.timestamp ?? null,
        times[index]!,
        safeJson(message.blocks ?? []),
        message.usage ? safeJson(message.usage) : null,
        'transcript',
        Date.now(),
      )
    })
    database.exec('COMMIT')
    trim(deviceId, localSessionId)
  } catch (error) {
    try {
      database.exec('ROLLBACK')
    } catch {
      // ignore
    }
    log(`[conversation] transcript cache failed: ${(error as Error).message}`)
  }
}

/**
 * Record a message the web accepted for delivery. It is stored before the
 * terminal exists, so the user can reload and still see what they sent.
 */
export function saveWebUserMessage(
  deviceId: string,
  localSessionId: string,
  messageId: string,
  text: string,
  images?: Array<{ mimeType: string; data: string }>,
): void {
  // `messageId` is the outbox entry id, and the outbox reuses it as the
  // bridge message uuid. The CLI echoes that uuid, so both writes land on the
  // same primary key and the upsert dedupes them — no text matching needed.
  try {
    writeRow(
      deviceId,
      localSessionId,
      {
        id: messageId,
        role: 'user',
        text,
        timestamp: new Date().toISOString(),
        sortMs: Date.now(),
        blocks: [
          ...(text ? [{ type: 'text' as const, text }] : []),
          ...(images ?? []).map(image => ({
            type: 'image' as const,
            mimeType: image.mimeType,
            data: image.data,
          })),
        ],
        source: 'web',
      },
      true,
    )
    trim(deviceId, localSessionId)
  } catch (error) {
    // Persistence must never reject a message that was already accepted.
    log(`[conversation] web message cache failed: ${(error as Error).message}`)
  }
}

/**
 * Record a live bridge event. Only user/assistant conversation messages are
 * stored; control frames, partials and keepalives are transport noise.
 */
export function recordBridgeEvent(
  deviceId: string,
  localSessionId: string,
  eventType: string,
  payload: Record<string, unknown>,
): void {
  const message = payload.message
  const content = isRecord(message) ? message.content : undefined
  const uuid =
    typeof payload.uuid === 'string'
      ? payload.uuid
      : isRecord(message) && typeof message.id === 'string'
        ? message.id
        : undefined

  if (eventType === 'user') {
    // Synthetic user turns are tool results, not something a human typed.
    if (payload.isSynthetic === true) return
    const blocks = blocksFromContent(content)
    const text = textFromBlocks(blocks)
    // Image-only turns carry no text; requiring it made every photo sent from
    // the phone vanish from history on the next reload.
    const hasImage = blocks.some(block => block.type === 'image')
    if ((!text.trim() && !hasImage) || !uuid) return
    // Same uuid as the web-accepted row (the outbox reuses its id), so the
    // upsert merges them instead of adding a duplicate bubble.
    writeRow(deviceId, localSessionId, {
      id: uuid,
      role: 'user',
      text,
      timestamp: new Date().toISOString(),
      sortMs: Date.now(),
      blocks,
      source: 'bridge',
    })
    trim(deviceId, localSessionId)
    return
  }

  if (eventType === 'assistant') {
    // A synthetic turn ("No response requested." after an interrupt) is
    // already persisted by the transcript backfill under its stable uuid;
    // storing the live echo too rendered it twice in history.
    if (isRecord(message) && message.model === '<synthetic>') return
    const blocks = blocksFromContent(content)
    const text = textFromBlocks(blocks)
    if ((!text.trim() && blocks.length === 0) || !uuid) return
    writeRow(deviceId, localSessionId, {
      id: uuid,
      role: 'assistant',
      text,
      timestamp: new Date().toISOString(),
      sortMs: Date.now(),
      blocks,
      usage: usageFromMessage(isRecord(message) ? message : undefined),
      source: 'bridge',
    })
    trim(deviceId, localSessionId)
  }
}

/**
 * Pull billed usage off a live bridge assistant message (same extraction
 * rules as the spawner's transcript reader and the terminal's hydration).
 */
function usageFromMessage(
  message: Record<string, unknown> | undefined,
): TranscriptUsage | undefined {
  if (!message || message.model === '<synthetic>') return undefined
  const usage = message.usage
  if (!usage || typeof usage !== 'object' || Array.isArray(usage))
    return undefined
  const u = usage as Record<string, unknown>
  const result = {
    inputTokens: Number(u.input_tokens) || 0,
    outputTokens: Number(u.output_tokens) || 0,
    cacheReadInputTokens: Number(u.cache_read_input_tokens) || 0,
    cacheCreationInputTokens: Number(u.cache_creation_input_tokens) || 0,
  }
  if (
    result.inputTokens === 0 &&
    result.outputTokens === 0 &&
    result.cacheReadInputTokens === 0 &&
    result.cacheCreationInputTokens === 0
  ) {
    return undefined
  }
  return result
}

function trim(deviceId: string, localSessionId: string): void {
  try {
    trimStmt!.run(deviceId, localSessionId, MAX_STORED_PER_SESSION)
  } catch {
    // Best-effort; the read path still works without trimming.
  }
}

type Cursor = { sortMs: number; seq: number }

function parseCursor(before?: string): Cursor | undefined {
  if (!before) return undefined
  // Backward compatibility with the first cursor format (a bare seq).
  if (/^\d+$/.test(before))
    return { sortMs: Number.MAX_SAFE_INTEGER, seq: Number(before) }
  const split = before.lastIndexOf('|')
  if (split <= 0) return undefined
  const sortMs = Number(before.slice(0, split))
  const seq = Number(before.slice(split + 1))
  if (!Number.isFinite(sortMs) || !Number.isFinite(seq)) return undefined
  return { sortMs, seq }
}

export function listStoredMessages(
  deviceId: string,
  localSessionId: string,
  limit: number,
  before?: string,
): StoredPage {
  ensureDb()
  const bareSeq = before && /^\d+$/.test(before) ? Number(before) : undefined
  const cursor = bareSeq === undefined ? parseCursor(before) : undefined
  // Fetch one extra row to know whether another page exists without guessing
  // from a full page (which produced a phantom "load more").
  const fetchLimit = limit + 1
  const rows = (
    bareSeq !== undefined
      ? listSeqBeforeStmt!.all(deviceId, localSessionId, bareSeq, fetchLimit)
      : cursor
        ? listBeforeStmt!.all(
            deviceId,
            localSessionId,
            cursor.sortMs,
            cursor.sortMs,
            cursor.seq,
            fetchLimit,
          )
        : listStmt!.all(deviceId, localSessionId, fetchLimit)
  ) as Row[]
  const hasMore = rows.length > limit
  if (hasMore) rows.pop()

  const total =
    (countStmt!.get(deviceId, localSessionId) as { n: number } | null)?.n ?? 0

  // Rows come newest-first for a cheap tail query; the UI wants oldest-first.
  const ordered = [...rows].reverse()
  const messages: StoredMessage[] = ordered.map(row => {
    let blocks: TranscriptBlock[] = []
    try {
      const parsed = JSON.parse(row.blocks)
      if (Array.isArray(parsed)) blocks = parsed as TranscriptBlock[]
    } catch {
      blocks = []
    }
    let usage: TranscriptUsage | undefined
    if (row.usage) {
      try {
        const parsed = JSON.parse(row.usage)
        if (parsed && typeof parsed === 'object') {
          usage = parsed as TranscriptUsage
        }
      } catch {
        // A corrupt usage payload must not take the page down with it.
      }
    }
    return {
      id: row.message_id,
      role: row.role === 'assistant' ? 'assistant' : 'user',
      text: row.text,
      ...(row.timestamp ? { timestamp: row.timestamp } : {}),
      ...(usage ? { usage } : {}),
      blocks,
      source: row.source as StoredMessage['source'],
    }
  })

  const oldest = rows[rows.length - 1]

  return {
    messages,
    ...(hasMore && oldest ? { before: `${oldest.sort_ms}|${oldest.seq}` } : {}),
    hasMore,
    total,
  }
}

/** Oldest cached transcript id, used as the `before` cursor for backfill. */
export function oldestTranscriptMessageId(
  deviceId: string,
  localSessionId: string,
): string | undefined {
  ensureDb()
  const row = oldestStmt!.get(deviceId, localSessionId) as {
    message_id: string
  } | null
  return row?.message_id
}

export function hasStoredMessages(
  deviceId: string,
  localSessionId: string,
): boolean {
  ensureDb()
  const row = countStmt!.get(deviceId, localSessionId) as { n: number } | null
  return (row?.n ?? 0) > 0
}

/**
 * True when the session's cache is pinned at the trim cap: older pages fetched
 * past this point are deleted as fast as they arrive, so backfilling further
 * only burns device round-trips. Callers treat this as "history is as complete
 * as the cache allows".
 */
export function isStoreAtCapacity(
  deviceId: string,
  localSessionId: string,
): boolean {
  ensureDb()
  const row = countStmt!.get(deviceId, localSessionId) as { n: number } | null
  return (row?.n ?? 0) >= MAX_STORED_PER_SESSION
}

export function isBackfillComplete(
  deviceId: string,
  localSessionId: string,
): boolean {
  ensureDb()
  return Boolean(
    backfillDoneStmt!.get(deviceId, localSessionId) as { ok: number } | null,
  )
}

export function markBackfillComplete(
  deviceId: string,
  localSessionId: string,
): void {
  ensureDb()
  markBackfillStmt!.run(deviceId, localSessionId, Date.now())
}

/** Test seam. */
export function resetConversationStoreForTest(): void {
  if (db) {
    try {
      db.close()
    } catch {
      // ignore
    }
  }
  db = undefined
  insertStmt = undefined
  insertIfAbsentStmt = undefined
  listStmt = undefined
  listBeforeStmt = undefined
  countStmt = undefined
  minSortStmt = undefined
  maxSortStmt = undefined
  listSeqBeforeStmt = undefined
  backfillDoneStmt = undefined
  markBackfillStmt = undefined
  oldestStmt = undefined
  trimStmt = undefined
}
