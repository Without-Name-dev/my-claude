import { AsyncLocalStorage } from 'node:async_hooks'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { isModelAllowed } from './modelAllowlist.js'
import {
  saveSessionProvider,
  readSessionProvider,
  readRecentSessionProviders,
} from './serverProviderSession.js'

export type ServerProvider = {
  id: string
  name: string
  protocol: 'anthropic' | 'openai' | 'gemini' | 'grok'
  baseUrl: string
  authMode?: 'api-key' | 'bearer'
  apiKey: string
  models: string[]
  modelDetails?: { model: string; displayName: string; context1m: boolean }[]
  revision: number
  model: string
}
export type ServerProviderInfo = Omit<ServerProvider, 'apiKey' | 'model'> & {
  hasApiKey: boolean
}

// A running turn gets an immutable snapshot. Parallel subagents inherit that
// snapshot even if the interactive window changes its next selection.
const activeContext = new AsyncLocalStorage<ServerProvider | null>()
let selected: ServerProvider | null = null
let selectionEpoch = 0
const selectionListeners = new Set<() => void>()

export function getServerProvider(): ServerProvider | null {
  const scoped = activeContext.getStore()
  return scoped === undefined ? selected : scoped
}
export function setServerProvider(value: ServerProvider | null): void {
  selectionEpoch++
  selected = value
  for (const listener of selectionListeners) listener()
}
export function serverProviderSelection(
  value: Pick<ServerProvider, 'id' | 'model'>,
): string {
  return `rcs-provider:${value.id}:${encodeURIComponent(value.model)}`
}
export function subscribeServerProvider(listener: () => void): () => void {
  selectionListeners.add(listener)
  return () => {
    selectionListeners.delete(listener)
  }
}
/** Internal tier aliases fall back to this profile's selected model. */
export function resolveServerModel(model: string): string {
  const provider = getServerProvider()
  if (!provider) return model
  const actualModel = model.replace(/\[1m\]$/i, '')
  return provider.models.includes(actualModel) ? actualModel : provider.model
}
/** Keep the opt-in local to this window; the suffix is never a provider model ID. */
export function serverProviderRuntimeModel(provider: ServerProvider): string {
  const detail = provider.modelDetails?.find(
    item => item.model === provider.model,
  )
  return detail?.context1m ? `${provider.model}[1m]` : provider.model
}
export function runWithServerProvider<T>(
  provider: ServerProvider | null,
  callback: () => T,
): T {
  return activeContext.run(provider, callback)
}

function serverSettings(): { base: string; token: string } | undefined {
  const base = process.env.CLAUDE_BRIDGE_BASE_URL
  const token = process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
  return base && token ? { base: base.replace(/\/+$/, ''), token } : undefined
}
export function serverProvidersAvailable(): boolean {
  return Boolean(serverSettings())
}

export type ServerSessionSettingsValues = {
  model?: string
  effort?: 'auto' | 'low' | 'medium' | 'high' | 'xhigh' | 'max'
  ultracode?: boolean
}

/** Read this device's settings before /resume switches the running REPL. */
export async function loadServerSessionSettings(
  sessionId: string,
): Promise<ServerSessionSettingsValues | null> {
  const config = serverSettings()
  if (!config) return null
  let deviceId = process.env.CLAUDE_BRIDGE_DEVICE_ID
  if (!deviceId && process.env.LOCALAPPDATA) {
    try {
      const local = JSON.parse(
        readFileSync(
          join(process.env.LOCALAPPDATA, 'CCRemote', 'spawner.json'),
          'utf8',
        ),
      ) as Record<string, unknown>
      if (
        typeof local.serverUrl === 'string' &&
        local.serverUrl.replace(/\/+$/, '') === config.base &&
        typeof local.deviceId === 'string'
      )
        deviceId = local.deviceId
    } catch {
      /* A CLI without the connector can still restore its local selection. */
    }
  }
  if (!deviceId) return null
  let result: { settings: { values?: unknown } | null }
  try {
    result = await serverRequest(
      `/web/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(sessionId)}/settings`,
    )
  } catch (error) {
    if (error instanceof ServerRequestError && error.status === 404) return null // older server
    throw error
  }
  if (!result.settings) return null
  const values = result.settings.values
  if (!values || typeof values !== 'object' || Array.isArray(values))
    throw new Error('会话设置无效')
  const row = values as Record<string, unknown>
  if (row.model !== undefined && typeof row.model !== 'string')
    throw new Error('会话模型无效')
  if (
    row.effort !== undefined &&
    !['auto', 'low', 'medium', 'high', 'xhigh', 'max'].includes(
      String(row.effort),
    )
  )
    throw new Error('会话思考强度无效')
  if (row.ultracode !== undefined && typeof row.ultracode !== 'boolean')
    throw new Error('会话 Ultracode 设置无效')
  return row as ServerSessionSettingsValues
}

class ServerRequestError extends Error {
  constructor(readonly status: number) {
    super(`获取供应商配置失败（${status}）`)
  }
}

async function serverRequest<T>(path: string, body?: unknown): Promise<T> {
  const config = serverSettings()
  if (!config) throw new Error('未配置远程服务地址或设备凭据')
  const url = `${config.base}${path}`
  const request = (direct: boolean) =>
    fetch(url, {
      method: body === undefined ? 'GET' : 'PUT',
      headers: {
        Authorization: `Bearer ${config.token}`,
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      signal: AbortSignal.timeout(5_000),
      // Bun honours system proxy variables by default. If that proxy cannot
      // reach the self-hosted RCS, retry this read-only request directly.
      ...(direct ? { proxy: false } : {}),
    })
  let response: Response
  let retriedDirectly = false
  try {
    response = await request(false)
  } catch (error) {
    if (body !== undefined) throw error
    retriedDirectly = true
    response = await request(true)
  }
  if (
    body === undefined &&
    !retriedDirectly &&
    [502, 503, 504].includes(response.status)
  ) {
    response = await request(true)
  }
  if (!response.ok) throw new ServerRequestError(response.status)
  return (await response.json()) as T
}

/** Record an explicit choice; loading or restoring a session is not a choice. */
export async function rememberServerProvider(
  provider: Pick<ServerProvider, 'id' | 'model'>,
): Promise<void> {
  await serverRequest('/web/provider-preference', {
    id: provider.id,
    model: provider.model,
  })
}

/** Snapshot the shared preference for a brand-new session, without changing it. */
export async function initializeNewSessionProvider(
  sessionId: string,
): Promise<ServerProvider | null> {
  let result: { selection: { id: string; model: string } | null }
  try {
    result = await serverRequest('/web/provider-preference')
  } catch (error) {
    // The preference endpoint was introduced after provider management. Older
    // servers still supply credentials; recover the last locally selected
    // identity only after verifying it exists on this particular server.
    if (!(error instanceof ServerRequestError) || error.status !== 404)
      throw error
    const providers = await fetchServerProviders()
    const selected = readRecentSessionProviders().find(item =>
      providers.some(
        provider =>
          provider.id === item.id && provider.models.includes(item.model),
      ),
    )
    const choices = providers.flatMap(provider =>
      provider.models.map(model => ({ id: provider.id, model })),
    )
    if (!selected && choices.length > 1) {
      throw new Error(
        '服务器版本较旧，请先在已有会话中选择供应商模型，或更新服务器',
      )
    }
    result = { selection: selected ?? choices[0] ?? null }
  }
  if (!result.selection) return null
  if (
    typeof result.selection.id !== 'string' ||
    typeof result.selection.model !== 'string'
  )
    throw new Error('服务器模型选择无效')
  if (!isModelAllowed(result.selection.model))
    throw new Error('服务器模型不在允许列表中')
  return selectServerProvider(
    result.selection.id,
    result.selection.model,
    sessionId,
  )
}
export async function fetchServerProviders(): Promise<ServerProviderInfo[]> {
  const result = await serverRequest<{ providers: ServerProviderInfo[] }>(
    '/web/providers',
  )
  if (!Array.isArray(result.providers)) throw new Error('服务器供应商列表无效')
  return result.providers
}
export async function fetchServerProvider(
  id: string,
  model: string,
): Promise<ServerProvider> {
  const result = await serverRequest<{
    provider: Omit<ServerProvider, 'model'>
  }>(`/web/provider-credentials/${encodeURIComponent(id)}`)
  const value = result.provider
  if (
    !value ||
    value.id !== id ||
    !Array.isArray(value.models) ||
    !value.models.includes(model) ||
    !['anthropic', 'openai', 'gemini', 'grok'].includes(value.protocol) ||
    typeof value.apiKey !== 'string' ||
    !value.apiKey ||
    typeof value.baseUrl !== 'string'
  ) {
    throw new Error('供应商或模型配置已变更，请重新选择')
  }
  const url = new URL(value.baseUrl)
  if (
    !['http:', 'https:'].includes(url.protocol) ||
    url.username ||
    url.password
  )
    throw new Error('供应商地址无效')
  return { ...value, model }
}

/** Only the most recently requested selection may replace the active profile. */
export async function selectServerProvider(
  id: string,
  model: string,
  sessionId?: string,
): Promise<ServerProvider> {
  const epoch = ++selectionEpoch
  const value = await fetchServerProvider(id, model)
  if (epoch !== selectionEpoch) throw new Error('已有更新的模型选择')
  if (sessionId) saveSessionProvider(sessionId, { id, model })
  setServerProvider(value)
  return value
}

/** Fail closed on a saved selection: do not silently route a resumed session elsewhere. */
export async function loadSessionProvider(
  sessionId: string,
): Promise<ServerProvider | null> {
  const selection = readSessionProvider(sessionId)
  return selection ? fetchServerProvider(selection.id, selection.model) : null
}

export async function restoreSessionProvider(
  sessionId: string,
): Promise<ServerProvider | null> {
  const value = await loadSessionProvider(sessionId)
  setServerProvider(value)
  return value
}
