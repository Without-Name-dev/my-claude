/**
 * Device Spawner APIs for the chat-first workspace.
 *
 * These calls describe evidence, not guesses: `cached` and `updatedAt` on a
 * catalog response mean the device could not be reached and the UI is looking
 * at the last known snapshot.
 */

import { getActiveApiToken, getUuid } from './client'
import { debugLog } from '../lib/debug-log'

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

export type TranscriptBlock =
  | { type: 'text'; text: string }
  | { type: 'thinking'; text: string }
  | { type: 'tool_use'; id?: string; name: string; input?: unknown }
  | { type: 'tool_result'; toolUseId?: string; text: string; isError?: boolean }
  | { type: 'image' }

export type TranscriptMessage = {
  id: string
  role: 'user' | 'assistant'
  text: string
  timestamp?: string
  blocks: TranscriptBlock[]
  isMeta?: boolean
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

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const token = getActiveApiToken()
  if (token) headers.Authorization = `Bearer ${token}`
  const uuid = getUuid()
  const sep = path.includes('?') ? '&' : '?'
  const res = await fetch(`${path}${sep}uuid=${encodeURIComponent(uuid)}`, {
    method,
    headers,
    ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
  })
  const data: unknown = await res.json().catch(() => null)
  if (!res.ok) {
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
        : res.statusText || '请求失败'
    debugLog('api', `${method} ${path} → ${res.status} ${code}: ${message}`)
    throw new DeviceApiError(res.status, code, message)
  }
  return data as T
}

export function fetchDevices(): Promise<{ devices: DeviceInfo[] }> {
  return request('GET', '/web/devices')
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
): Promise<{
  sessions: DeviceSessionSummary[]
  cached?: boolean
  updatedAt?: number
  warning?: string
}> {
  const query = new URLSearchParams({ limit: String(limit) })
  if (projectId) query.set('projectId', projectId)
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

export function fetchOperation(operationId: string): Promise<OpenOperation> {
  return request('GET', `/web/operations/${encodeURIComponent(operationId)}`)
}

export function sendDeviceMessage(
  deviceId: string,
  localSessionId: string,
  text: string,
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
    { text },
  )
}
