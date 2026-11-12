import type {
  Session,
  Environment,
  ControlResponse,
  SessionEvent,
} from '../types'
import { generateMessageUuid } from '../lib/utils'
import { debugLog } from '../lib/debug-log'

const BASE = ''

/** Same ceiling as the device API: a hung request must surface, not spin. */
const REQUEST_TIMEOUT_MS = 30_000

export function getUuid(): string {
  let uuid = localStorage.getItem('rcs_uuid')
  if (!uuid) {
    uuid = generateMessageUuid()
    localStorage.setItem('rcs_uuid', uuid)
  }
  return uuid
}

export function setUuid(uuid: string): void {
  localStorage.setItem('rcs_uuid', uuid)
}

/** Active API token for Authorization header (set by useTokens) */
let _activeToken: string | null = null

export function setActiveApiToken(token: string | null): void {
  _activeToken = token
}

export function getActiveApiToken(): string | null {
  return _activeToken
}

/**
 * Server access key (RCS_WEB_ACCESS_KEY), entered once when the server asks
 * for it. Kept in localStorage for fetches and mirrored into a cookie so the
 * SSE EventSource — which cannot send headers — passes too.
 */
const ACCESS_KEY_STORAGE = 'rcs.access_key'

// Mirrors the stored key for this page lifetime. localStorage can throw or be
// cleared (private mode, storage-blocking extensions); without this fallback
// the gate would accept the key, reload, and then send no Authorization header
// at all — an endless 401 loop that looks like "login is broken".
let _memoryAccessKey: string | null = null

export function getAccessKey(): string | null {
  try {
    return localStorage.getItem(ACCESS_KEY_STORAGE) || _memoryAccessKey
  } catch {
    return _memoryAccessKey
  }
}

export function setAccessKey(key: string): void {
  _memoryAccessKey = key
  try {
    localStorage.setItem(ACCESS_KEY_STORAGE, key)
  } catch {
    // Private mode: the cookie below still authenticates this page lifetime.
  }
  document.cookie = `rcs_access_key=${encodeURIComponent(key)}; path=/; max-age=31536000; samesite=strict`
}

export function clearAccessKey(): void {
  _memoryAccessKey = null
  try {
    localStorage.removeItem(ACCESS_KEY_STORAGE)
  } catch {
    // ignore
  }
  document.cookie = 'rcs_access_key=; path=/; max-age=0; samesite=strict'
}

async function api<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }

  const token = _activeToken ?? getAccessKey()
  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }

  const uuid = getUuid()
  const sep = path.includes('?') ? '&' : '?'
  const url = `${BASE}${path}${sep}uuid=${encodeURIComponent(uuid)}`
  const opts: RequestInit = { method, headers }
  if (body !== undefined) opts.body = JSON.stringify(body)

  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS)
  let res: Response
  try {
    res = await fetch(url, { ...opts, signal: controller.signal })
  } catch (error) {
    const aborted = error instanceof Error && error.name === 'AbortError'
    debugLog(
      'api',
      `${method} ${path} → ${aborted ? 'timeout' : 'network error'}`,
    )
    throw new Error(
      aborted ? '请求超时，请检查网络后重试' : '网络异常，请检查连接',
    )
  } finally {
    clearTimeout(timer)
  }
  let raw = ''
  let data: unknown = null
  if (typeof res.text === 'function') {
    raw = await res.text()
    try {
      data = raw ? JSON.parse(raw) : null
    } catch {
      // Non-JSON error page (proxy, HTML 502): fall through to status text.
    }
  } else {
    // Test doubles / older fetch implementations may only provide .json().
    data = await res.json()
  }
  if (!res.ok) {
    // A 401 from the access-key gate surfaces the pairing prompt.
    if (res.status === 401 && typeof window !== 'undefined') {
      window.dispatchEvent(new Event('rcs:unauthorized'))
    }
    const record =
      data && typeof data === 'object' && !Array.isArray(data)
        ? (data as Record<string, unknown>)
        : {}
    const err =
      record.error && typeof record.error === 'object'
        ? (record.error as Record<string, unknown>)
        : {}
    const message =
      typeof err.message === 'string'
        ? err.message
        : res.status >= 500 || /^\s*</.test(raw)
          ? res.status === 404
            ? '找不到这个会话，终端可能已关闭'
            : '服务器暂时不可用，请稍后重试'
          : raw.trim().slice(0, 200) || res.statusText || '请求失败'
    throw new Error(message)
  }
  return data as T
}

export function apiBind(sessionId: string) {
  return api<void>('POST', '/web/bind', { sessionId })
}

export function apiFetchSessions() {
  return api<Session[]>('GET', '/web/sessions')
}

export function apiFetchAllSessions() {
  return api<Session[]>('GET', '/web/sessions/all')
}

export function apiFetchSession(id: string) {
  return api<Session>('GET', `/web/sessions/${id}`)
}

export function apiFetchSessionHistory(id: string) {
  return api<{ events: SessionEvent[] }>('GET', `/web/sessions/${id}/history`)
}

export function apiFetchEnvironments() {
  return api<Environment[]>('GET', '/web/environments')
}

export function apiSendEvent(sessionId: string, body: Record<string, unknown>) {
  return api<void>('POST', `/web/sessions/${sessionId}/events`, body)
}

export function apiSendControl(sessionId: string, body: ControlResponse) {
  return api<void>('POST', `/web/sessions/${sessionId}/control`, body)
}

export function apiInterrupt(sessionId: string) {
  return api<void>('POST', `/web/sessions/${sessionId}/interrupt`)
}

export function apiCreateSession(body: {
  title?: string
  environment_id?: string
}) {
  return api<Session>('POST', '/web/sessions', body)
}
