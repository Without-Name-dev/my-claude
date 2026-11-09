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

async function api<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }

  if (_activeToken) {
    headers['Authorization'] = `Bearer ${_activeToken}`
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
