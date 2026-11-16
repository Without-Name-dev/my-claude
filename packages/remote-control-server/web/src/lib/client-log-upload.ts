/**
 * Uploads the client debug ring buffer to the RCS server so problems on a
 * phone — where ?debug=1 is unreachable — can be diagnosed from the server
 * journal (`journalctl -u cc-rcs -f`, lines prefixed [client-log]).
 *
 * Configurable end to end:
 *  - Server: RCS_CLIENT_LOG_UPLOAD=0 rejects uploads entirely
 *    (GET /web/client-logs/config reports enabled:false).
 *  - Client: one-click switch in 设置 → 诊断日志 (and the debug panel).
 *    Unset follows the server; '1'/'0' overrides it locally.
 *
 * The uploader never logs its own HTTP failures into the ring buffer — that
 * would self-trigger the error flush and loop.
 */

import { getAccessKey, getActiveApiToken, getUuid } from '../api/client'
import {
  debugLog,
  getDebugEntries,
  getDebugEntriesSince,
  type DebugEntry,
  subscribeDebug,
} from './debug-log'

const FLUSH_INTERVAL_MS = 30_000
const CONFIG_REFRESH_MS = 300_000
/** Matches the server's per-POST cap. */
const MAX_BATCH = 200
const ERROR_FLUSH_DELAY_MS = 2_000
const STORAGE_KEY = 'rcs.client_log_upload'
const ERROR_TAGS = new Set(['window', 'promise', 'console'])

let serverEnabled: boolean | null = null
let localOverride: boolean | null = null
let lastShippedSeq = 0
let inFlight = false
let started = false
let errorFlushTimer: ReturnType<typeof setTimeout> | null = null
let shippedTotal = 0
const settingListeners = new Set<() => void>()

function readOverride(): boolean | null {
  try {
    const value = localStorage.getItem(STORAGE_KEY)
    return value === '1' ? true : value === '0' ? false : null
  } catch {
    return null
  }
}

localOverride = readOverride()

function notifySetting(): void {
  for (const listener of settingListeners) listener()
}

/** Effective upload switch: local override wins, else server config, else on. */
export function isUploadEnabled(): boolean {
  return localOverride ?? serverEnabled ?? true
}

export function getUploadState(): {
  serverEnabled: boolean | null
  localOverride: boolean | null
  enabled: boolean
  lastShippedSeq: number
  shippedTotal: number
} {
  return {
    serverEnabled,
    localOverride,
    enabled: isUploadEnabled(),
    lastShippedSeq,
    shippedTotal,
  }
}

export function subscribeUploadSetting(listener: () => void): () => void {
  settingListeners.add(listener)
  return () => settingListeners.delete(listener)
}

/** null clears the local override → follow the server setting again. */
export function setUploadOverride(value: boolean | null): void {
  localOverride = value
  try {
    if (value === null) localStorage.removeItem(STORAGE_KEY)
    else localStorage.setItem(STORAGE_KEY, value ? '1' : '0')
  } catch {
    // Private mode: the in-memory copy still governs this page lifetime.
  }
  notifySetting()
  if (value !== false) void flushClientLogs('setting')
}

function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const token = getActiveApiToken() ?? getAccessKey()
  if (token) headers.Authorization = `Bearer ${token}`
  return headers
}

export function buildUploadBody(entries: DebugEntry[]): string {
  return JSON.stringify({
    uuid: getUuid(),
    entries: entries.slice(0, MAX_BATCH).map(entry => ({
      seq: entry.seq,
      at: entry.at,
      tag: entry.tag,
      message: entry.message,
    })),
  })
}

/** Re-read the server's on/off setting; also used by tests to reset state. */
export async function refreshUploadConfig(): Promise<void> {
  try {
    const res = await fetch(
      `/web/client-logs/config?uuid=${encodeURIComponent(getUuid())}`,
      { headers: authHeaders() },
    )
    if (!res.ok) return
    const data = (await res.json().catch(() => null)) as {
      enabled?: unknown
    } | null
    if (
      data &&
      typeof data.enabled === 'boolean' &&
      data.enabled !== serverEnabled
    ) {
      serverEnabled = data.enabled
      notifySetting()
    }
  } catch {
    // Offline: keep the previous setting; the next refresh retries.
  }
}

/** Flush pending entries now. Uses fetch (auth header) — safe on live pages. */
export async function flushClientLogs(reason: string): Promise<void> {
  if (!isUploadEnabled() || inFlight) return
  const pending = getDebugEntriesSince(lastShippedSeq)
  if (pending.length === 0) return
  const batch = pending.slice(0, MAX_BATCH)
  inFlight = true
  try {
    const res = await fetch(
      `/web/client-logs?uuid=${encodeURIComponent(getUuid())}`,
      { method: 'POST', headers: authHeaders(), body: buildUploadBody(batch) },
    )
    if (res.status === 403) {
      // client_logs_disabled — stop until the server says otherwise.
      serverEnabled = false
      notifySetting()
      return
    }
    if (res.ok) {
      lastShippedSeq = Math.max(lastShippedSeq, batch[batch.length - 1].seq)
      shippedTotal += batch.length
      notifySetting()
    }
    // 429/5xx: keep entries; the next flush retries with backoff from rate limit.
  } catch {
    // Offline: entries stay; timer/visibility triggers retry.
  } finally {
    inFlight = false
  }
}

/**
 * Best-effort ship on tab hide — iOS suspends the WebView shortly after
 * visibilitychange→hidden, so this is the last reliable moment. sendBeacon
 * authenticates via the rcs_access_key cookie (uuidAuth accepts it).
 */
export function flushClientLogsBeacon(): void {
  if (!isUploadEnabled()) return
  const pending = getDebugEntriesSince(lastShippedSeq)
  if (pending.length === 0) return
  const batch = pending.slice(0, MAX_BATCH)
  const url = `/web/client-logs?uuid=${encodeURIComponent(getUuid())}`
  try {
    if (
      typeof navigator !== 'undefined' &&
      navigator.sendBeacon(
        url,
        new Blob([buildUploadBody(batch)], { type: 'application/json' }),
      )
    ) {
      lastShippedSeq = Math.max(lastShippedSeq, batch[batch.length - 1].seq)
      shippedTotal += batch.length
    }
  } catch {
    // Old WebViews without Blob-typed beacons: the next foreground flush covers it.
  }
}

export function startClientLogUpload(): void {
  if (started || typeof window === 'undefined') return
  started = true

  debugLog(
    'lifecycle',
    `boot ${window.location.pathname} — ${navigator.userAgent}`,
  )

  void refreshUploadConfig()
  window.setInterval(() => void flushClientLogs('timer'), FLUSH_INTERVAL_MS)
  window.setInterval(() => void refreshUploadConfig(), CONFIG_REFRESH_MS)

  window.addEventListener('visibilitychange', () => {
    const hidden = document.visibilityState === 'hidden'
    debugLog('lifecycle', hidden ? '转入后台' : '回到前台')
    if (hidden) flushClientLogsBeacon()
  })
  window.addEventListener('pagehide', flushClientLogsBeacon)

  // Crash triage: a window/promise/console error entry schedules a near-term
  // flush so the batch reaches the server before the page is closed.
  subscribeDebug(() => {
    const last = getDebugEntries().at(-1)
    if (!last || !ERROR_TAGS.has(last.tag) || errorFlushTimer !== null) return
    errorFlushTimer = setTimeout(() => {
      errorFlushTimer = null
      void flushClientLogs('error')
    }, ERROR_FLUSH_DELAY_MS)
  })
}
