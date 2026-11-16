import { log } from '../logger'

/**
 * In-memory store for client (web/iOS) debug-log uploads.
 *
 * The web app keeps a local ring buffer of tagged events (SSE lifecycle, API
 * failures, visibility changes). POST /web/client-logs ships batches here so
 * problems on a phone — where ?debug=1 is unreachable — can be read from the
 * server journal or the retrieval endpoint. Everything is best-effort
 * diagnostics: bounded memory, rate-limited, never persisted to disk.
 */

export type ClientLogEntry = {
  seq: number
  at: number
  tag: string
  message: string
}

const MAX_ENTRIES_PER_UUID = 1000
const MAX_UUIDS = 50
/** Per-POST cap — mirrors the client-side MAX_BATCH so one payload fits. */
const MAX_BATCH = 200
const MAX_MESSAGE_CHARS = 2000
const MAX_TAG_CHARS = 32
const RATE_WINDOW_MS = 60_000
const RATE_MAX_UPLOADS = 20

/**
 * Read at call time (not module load) so tests and ops can flip it without
 * reload ordering surprises. Default on; RCS_CLIENT_LOG_UPLOAD=0 turns both
 * ingestion and retrieval off.
 */
export function isClientLogUploadEnabled(): boolean {
  return process.env.RCS_CLIENT_LOG_UPLOAD !== '0'
}

const buffers = new Map<string, ClientLogEntry[]>()
const lastSeqByUuid = new Map<string, number>()
const uploadTimes = new Map<string, number[]>()

export type RecordResult = {
  stored: number
  dropped: number
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function sanitizeEntry(raw: unknown, uuid: string): ClientLogEntry | null {
  if (!isRecord(raw)) return null
  const seq = raw.seq
  const at = raw.at
  const tag = typeof raw.tag === 'string' ? raw.tag : ''
  const message = typeof raw.message === 'string' ? raw.message : ''
  if (typeof seq !== 'number' || !Number.isInteger(seq) || seq <= 0) return null
  if (typeof at !== 'number' || !Number.isFinite(at)) return null
  // Replayed batches (sendBeacon retried, page reload) must not duplicate.
  const lastSeq = lastSeqByUuid.get(uuid) ?? 0
  if (seq <= lastSeq) return null
  const clampedAt =
    at < 0 ? 0 : at > Date.now() + 300_000 ? Date.now() : Math.round(at)
  return {
    seq,
    at: clampedAt,
    tag: tag.slice(0, MAX_TAG_CHARS),
    message: message.slice(0, MAX_MESSAGE_CHARS),
  }
}

export function recordClientLogs(uuid: string, raw: unknown): RecordResult {
  if (!Array.isArray(raw)) return { stored: 0, dropped: 0 }
  const buffer = buffers.get(uuid) ?? []
  buffers.set(uuid, buffer)
  let stored = 0
  let dropped = 0
  for (const item of raw.slice(0, MAX_BATCH)) {
    const entry = sanitizeEntry(item, uuid)
    if (!entry) {
      dropped++
      continue
    }
    buffer.push(entry)
    lastSeqByUuid.set(uuid, entry.seq)
    stored++
    // Mirror into the journal so `journalctl -u cc-rcs -f` shows phone
    // activity live — that is the whole point of the feature.
    log(`[client-log] ${uuid} #${entry.seq} [${entry.tag}] ${entry.message}`)
  }
  if (buffer.length > MAX_ENTRIES_PER_UUID) {
    dropped += buffer.length - MAX_ENTRIES_PER_UUID
    buffer.splice(0, buffer.length - MAX_ENTRIES_PER_UUID)
  }
  // Simple LRU across uuids: a Map preserves insertion order, so re-setting
  // on activity keeps the least-recently-used entry first for eviction.
  buffers.delete(uuid)
  buffers.set(uuid, buffer)
  while (buffers.size > MAX_UUIDS) {
    const oldest = buffers.keys().next().value
    if (oldest === undefined) break
    buffers.delete(oldest)
    lastSeqByUuid.delete(oldest)
    uploadTimes.delete(oldest)
  }
  return { stored, dropped }
}

/** Sliding-window rate limit per uuid; call before recordClientLogs. */
export function isClientLogRateLimited(uuid: string): boolean {
  const now = Date.now()
  const times = (uploadTimes.get(uuid) ?? []).filter(
    t => now - t < RATE_WINDOW_MS,
  )
  if (times.length >= RATE_MAX_UPLOADS) {
    uploadTimes.set(uuid, times)
    return true
  }
  times.push(now)
  uploadTimes.set(uuid, times)
  return false
}

export function listClientLogs(uuid: string, limit = 200): ClientLogEntry[] {
  const buffer = buffers.get(uuid)
  if (!buffer) return []
  const max = Math.min(Math.max(limit, 1), MAX_ENTRIES_PER_UUID)
  return buffer.slice(-max)
}

export function resetClientLogStoreForTest(): void {
  buffers.clear()
  lastSeqByUuid.clear()
  uploadTimes.clear()
}
