/**
 * Device Spawner APIs for the chat-first workspace.
 *
 * These calls describe evidence, not guesses: `cached` and `updatedAt` on a
 * catalog response mean the device could not be reached and the UI is looking
 * at the last known snapshot.
 */

import { getAccessKey, getActiveApiToken, getUuid } from './client'
import { debugLog } from '../lib/debug-log'
import type { UserMessageImage } from '../lib/types'

export type DeviceInfo = {
  id: string
  name: string
  platform: string
  configDir: string
  capabilities: string[]
  online: boolean
  lastSeenAt: number | null
  catalogUpdatedAt: number | null
}

export type DeviceProject = {
  projectId: string
  path: string
  name: string
  sessionCount: number
  lastActivityAt: number | null
}

export type DeviceSessionSummary = {
  localSessionId: string
  projectId: string
  cwd: string
  title: string
  summary: string
  updatedAt: number
  createdAt?: number
  fileSize?: number
  gitBranch?: string
}

/** Terminal presence for one local session; cheap enough to poll. */
export type TerminalStatus = {
  localSessionId: string
  bridgeSessionId: string
  online: boolean
}

export type TranscriptBlock =
  | { type: 'text'; text: string }
  | { type: 'thinking'; text: string }
  | { type: 'tool_use'; id?: string; name: string; input?: unknown }
  | { type: 'tool_result'; toolUseId?: string; text: string; isError?: boolean }
  | { type: 'image'; mimeType?: string; data?: string }

/** Billed usage of one assistant message, when the source carries it. */
export type TranscriptUsage = {
  inputTokens: number
  outputTokens: number
  cacheReadInputTokens: number
  cacheCreationInputTokens: number
}

export type TranscriptMessage = {
  id: string
  role: 'user' | 'assistant'
  text: string
  timestamp?: string
  blocks: TranscriptBlock[]
  isMeta?: boolean
  usage?: TranscriptUsage
}

/** One row in the directory browser; files are listed but not selectable
 *  when the picker's job is choosing a working directory. */
export type DirectoryEntry = {
  name: string
  path: string
  type: 'dir' | 'file'
  size?: number
  modifiedAt?: number
}

export type DirectoryRoot = {
  name: string
  path: string
  kind: 'drive' | 'home' | 'volume'
}

export type DirectoryListing = {
  path?: string
  parent?: string
  entries: DirectoryEntry[]
  roots?: DirectoryRoot[]
  truncated?: boolean
}

export type UploadFileInput = { name: string; data: string }

export type UploadResult = {
  root: string
  files: { name: string; path: string; bytes: number }[]
}

export type SessionRuntime = {
  localSessionId: string
  cwd: string
  processAlive: boolean
  pid?: number
  bridgeSessionId?: string
  verifiedAt: number
  reportedAt?: number
  source: string
}

export type OpenOperation = {
  id: string
  deviceId: string
  localSessionId: string
  cwd: string
  state: string
  detail?: string
  failureCode?: string
  bridgeSessionId?: string
  pid?: number
  createdAt: number
  updatedAt: number
  /** open_new_session: the session id the CLI minted, once known. */
  resolvedLocalSessionId?: string
}

export type DeviceSessionDetail = {
  deviceId: string
  online: boolean
  session: DeviceSessionSummary
  bridgeSessionId: string | null
  /** True only while a bridge transport is attached right now. */
  bridgeConnected: boolean
  runtime: SessionRuntime | null
  operation: OpenOperation | null
  /** Messages that might have been delivered before an RCS restart. */
  deliveryReviewRequired?: number
}

export class DeviceApiError extends Error {
  status: number
  code: string
  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'DeviceApiError'
    this.status = status
    this.code = code
  }
}

/** Generous enough for a cold terminal scan, short enough to fail visibly. */
const REQUEST_TIMEOUT_MS = 30_000

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  timeoutMs = REQUEST_TIMEOUT_MS,
): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const token = getActiveApiToken() ?? getAccessKey()
  if (token) headers.Authorization = `Bearer ${token}`
  const uuid = getUuid()
  const sep = path.includes('?') ? '&' : '?'
  // Without a timeout a stalled request hangs the spinner forever (the
  // terminal poll, the history load, and send all share this path).
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  let res: Response
  try {
    res = await fetch(`${path}${sep}uuid=${encodeURIComponent(uuid)}`, {
      method,
      headers,
      signal: controller.signal,
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    })
  } catch (err) {
    const aborted = err instanceof Error && err.name === 'AbortError'
    debugLog(
      'api',
      `${method} ${path} → ${aborted ? 'timeout' : 'network error'}`,
    )
    throw new DeviceApiError(
      0,
      aborted ? 'timeout' : 'network_error',
      aborted ? '请求超时，请检查网络后重试' : '网络异常，请检查连接',
    )
  } finally {
    clearTimeout(timer)
  }
  const text = await res.text().catch(() => '')
  let data: unknown = null
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    // Proxy / nginx error pages are HTML; never render that markup to a user.
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
    const error =
      record.error &&
      typeof record.error === 'object' &&
      !Array.isArray(record.error)
        ? (record.error as Record<string, unknown>)
        : {}
    const code = typeof error.type === 'string' ? error.type : 'request_failed'
    const message =
      typeof error.message === 'string'
        ? error.message
        : res.status >= 500
          ? '服务器暂时不可用，请稍后重试'
          : res.status === 404
            ? '找不到这个会话或设备'
            : res.statusText || '请求失败'
    debugLog('api', `${method} ${path} → ${res.status} ${code}: ${message}`)
    throw new DeviceApiError(res.status, code, message)
  }
  return data as T
}

export function fetchDevices(): Promise<{ devices: DeviceInfo[] }> {
  return request('GET', '/web/devices')
}

export function reviewUnconfirmedDelivery(
  deviceId: string,
  localSessionId: string,
): Promise<{ reviewed: number }> {
  return request(
    'POST',
    `/web/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(localSessionId)}/review-delivery`,
  )
}

/**
 * Terminal presence per session, read from the server's in-memory state.
 * Polling this does not touch the PC, unlike the session catalog.
 */
export function fetchTerminals(
  deviceId: string,
): Promise<{ terminals: TerminalStatus[] }> {
  return request(
    'GET',
    `/web/devices/${encodeURIComponent(deviceId)}/terminals`,
  )
}

/**
 * List one directory on the operator machine. An empty path asks for the
 * starting points (home directory and drives) instead of a listing.
 */
export function fetchDirectory(
  deviceId: string,
  path?: string,
): Promise<DirectoryListing> {
  const query = path ? `?path=${encodeURIComponent(path)}` : ''
  return request(
    'GET',
    `/web/devices/${encodeURIComponent(deviceId)}/directory${query}`,
  )
}

/**
 * Upload files into the session's working directory. Uploads are slow on a
 * phone network, so this call gets a longer timeout than the rest.
 */
export function uploadToDevice(
  deviceId: string,
  cwd: string,
  files: UploadFileInput[],
  timeoutMs = 180_000,
): Promise<UploadResult> {
  return request(
    'POST',
    `/web/devices/${encodeURIComponent(deviceId)}/uploads`,
    { cwd, files },
    timeoutMs,
  )
}

export type CopiedEntry = {
  source: string
  path: string
  type: 'dir' | 'file'
  copied: boolean
}

/**
 * Bring files or directories that already exist on the PC into the session
 * directory. Sources already inside it are returned untouched.
 */
export function copyToDevice(
  deviceId: string,
  cwd: string,
  paths: string[],
  timeoutMs = 180_000,
): Promise<{ entries: CopiedEntry[] }> {
  return request(
    'POST',
    `/web/devices/${encodeURIComponent(deviceId)}/copy`,
    { cwd, paths },
    timeoutMs,
  )
}

export function fetchProjects(deviceId: string): Promise<{
  projects: DeviceProject[]
  cached?: boolean
  updatedAt?: number
  warning?: string
}> {
  return request('GET', `/web/devices/${encodeURIComponent(deviceId)}/projects`)
}

export function fetchSessions(
  deviceId: string,
  projectId?: string,
  limit = 200,
  offset = 0,
): Promise<{
  sessions: DeviceSessionSummary[]
  hasMore?: boolean
  cached?: boolean
  updatedAt?: number
  warning?: string
}> {
  const query = new URLSearchParams({ limit: String(limit) })
  if (projectId) query.set('projectId', projectId)
  if (offset > 0) query.set('offset', String(offset))
  return request(
    'GET',
    `/web/devices/${encodeURIComponent(deviceId)}/sessions?${query.toString()}`,
  )
}

export function fetchSession(
  deviceId: string,
  localSessionId: string,
): Promise<DeviceSessionDetail> {
  return request(
    'GET',
    `/web/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(localSessionId)}`,
  )
}

export function fetchHistory(
  deviceId: string,
  localSessionId: string,
  options: { limit?: number; before?: string } = {},
): Promise<{
  messages: TranscriptMessage[]
  before?: string
  hasMore: boolean
  total: number
  /** True when the device could not be reached and this is the server cache. */
  cached?: boolean
  warning?: string
}> {
  const query = new URLSearchParams({ limit: String(options.limit ?? 100) })
  if (options.before) query.set('before', options.before)
  return request(
    'GET',
    `/web/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(localSessionId)}/history?${query.toString()}`,
  )
}

export function openSession(
  deviceId: string,
  localSessionId: string,
  force = false,
): Promise<{
  state: string
  operationId: string | null
  bridgeSessionId?: string | null
  detail?: string | null
}> {
  return request(
    'POST',
    `/web/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(localSessionId)}/open`,
    force ? { force: true } : {},
  )
}

/** Open a brand-new terminal in a project cwd; follow the operation to learn
 *  the minted session id (resolvedLocalSessionId on the record). */
export function openNewSession(
  deviceId: string,
  cwd: string,
): Promise<{
  state: string
  operationId: string | null
  detail?: string | null
}> {
  return request(
    'POST',
    `/web/devices/${encodeURIComponent(deviceId)}/sessions/new`,
    { cwd },
  )
}

/** Stop the terminal of one local session (destructive; ask the user first). */
export function closeSession(
  deviceId: string,
  localSessionId: string,
): Promise<{ closed: boolean; detail?: string }> {
  return request(
    'POST',
    `/web/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(localSessionId)}/close`,
  )
}

export function fetchOperation(operationId: string): Promise<OpenOperation> {
  return request('GET', `/web/operations/${encodeURIComponent(operationId)}`)
}

export function sendDeviceMessage(
  deviceId: string,
  localSessionId: string,
  text: string,
  images?: UserMessageImage[],
): Promise<{
  state: 'sent' | 'unconfirmed' | 'queued'
  messageId: string
  bridgeSessionId?: string
  operationId?: string | null
  detail?: string
}> {
  return request(
    'POST',
    `/web/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(localSessionId)}/messages`,
    { text, ...(images && images.length > 0 ? { images } : {}) },
  )
}
