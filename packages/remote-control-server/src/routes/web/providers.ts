import { Hono } from 'hono'
import { extractBearerToken, uuidAuth } from '../../auth/middleware'
import { validateApiKey } from '../../auth/api-key'
import {
  authenticateDeviceToken,
  lastKnownProviderSelections,
} from '../../device/state'
import { config } from '../../config'
import { fetchProviderQuota } from '../../device/provider-quota'
import {
  deleteProvider,
  listProviders,
  readProvider,
  readLastProviderSelection,
  saveLastProviderSelection,
  saveProvider,
  type ProviderInput,
  type ProviderProtocol,
} from '../../device/provider-store'

const app = new Hono()

function deviceAuthorized(
  c: Parameters<typeof extractBearerToken>[0],
): boolean {
  const token = extractBearerToken(c)
  return !!token && (validateApiKey(token) || !!authenticateDeviceToken(token))
}

// The last selection is shared, but each session snapshots it independently.
app.get('/provider-preference', c => {
  if (!deviceAuthorized(c))
    return c.json({ error: { message: '设备凭据无效' } }, 401)
  let selection = readLastProviderSelection()
  if (!selection) {
    for (const value of lastKnownProviderSelections()) {
      const match = /^rcs-provider:([^:]+):(.+)$/.exec(value)
      if (!match) continue
      try {
        const model = decodeURIComponent(match[2]!)
        if (readProvider(match[1]!)?.models.includes(model)) {
          selection = { id: match[1]!, model }
          saveLastProviderSelection(selection.id, selection.model)
          break
        }
      } catch {
        /* Ignore malformed old snapshots. */
      }
    }
  }
  c.header('Cache-Control', 'no-store')
  return c.json({ selection })
})
app.put('/provider-preference', async c => {
  if (!deviceAuthorized(c))
    return c.json({ error: { message: '设备凭据无效' } }, 401)
  const body: unknown = await c.req.json().catch(() => null)
  if (!body || typeof body !== 'object' || Array.isArray(body))
    return c.json({ error: { message: '供应商选择无效' } }, 400)
  const { id, model } = body as Record<string, unknown>
  if (
    typeof id !== 'string' ||
    typeof model !== 'string' ||
    !readProvider(id)?.models.includes(model)
  )
    return c.json({ error: { message: '供应商或模型不存在' } }, 400)
  saveLastProviderSelection(id, model)
  return c.json({ ok: true })
})
const protocols: ProviderProtocol[] = ['anthropic', 'openai', 'gemini', 'grok']
const quotaCache = new Map<
  string,
  {
    revision: number
    until: number
    promise: ReturnType<typeof fetchProviderQuota>
  }
>()

function parseProvider(value: unknown): ProviderInput | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value))
    return undefined
  const body = value as Record<string, unknown>
  if (
    typeof body.name !== 'string' ||
    !body.name.trim() ||
    body.name.length > 120 ||
    typeof body.protocol !== 'string' ||
    !protocols.includes(body.protocol as ProviderProtocol) ||
    typeof body.baseUrl !== 'string' ||
    body.baseUrl.length > 2048 ||
    (body.authMode !== undefined &&
      body.authMode !== 'api-key' &&
      body.authMode !== 'bearer') ||
    !Array.isArray(body.models) ||
    body.models.length === 0 ||
    body.models.length > 100 ||
    !body.models.every(
      model => typeof model === 'string' && model.trim() && model.length <= 300,
    ) ||
    (body.modelDetails !== undefined &&
      (!Array.isArray(body.modelDetails) ||
        body.modelDetails.length > 100 ||
        !body.modelDetails.every(detail => {
          if (!detail || typeof detail !== 'object' || Array.isArray(detail))
            return false
          const d = detail as Record<string, unknown>
          return (
            typeof d.model === 'string' &&
            (body.models as string[]).includes(d.model) &&
            typeof d.displayName === 'string' &&
            !!d.displayName.trim() &&
            d.displayName.length <= 300 &&
            typeof d.context1m === 'boolean'
          )
        }) ||
        new Set(
          body.modelDetails.map(detail => (detail as { model: string }).model),
        ).size !== body.modelDetails.length)) ||
    (body.apiKey !== undefined &&
      (typeof body.apiKey !== 'string' || body.apiKey.length > 8192))
  )
    return undefined
  try {
    const url = new URL(body.baseUrl)
    if (
      !['http:', 'https:'].includes(url.protocol) ||
      url.username ||
      url.password ||
      url.search ||
      url.hash
    )
      return undefined
  } catch {
    return undefined
  }
  return {
    name: body.name.trim(),
    protocol: body.protocol as ProviderProtocol,
    baseUrl: body.baseUrl.replace(/\/+$/, ''),
    authMode: body.authMode === 'bearer' ? 'bearer' : 'api-key',
    models: [...new Set((body.models as string[]).map(model => model.trim()))],
    ...(Array.isArray(body.modelDetails)
      ? {
          modelDetails: body.modelDetails.map(detail => ({
            model: detail.model,
            displayName: detail.displayName.trim(),
            context1m: detail.context1m,
          })),
        }
      : {}),
    ...(typeof body.apiKey === 'string' ? { apiKey: body.apiKey } : {}),
  }
}

app.get('/providers', uuidAuth, c => c.json({ providers: listProviders() }))
app.get('/providers/:id/quota', uuidAuth, async c => {
  const provider = readProvider(c.req.param('id')!)
  if (!provider) return c.json({ error: { message: '供应商不存在' } }, 404)
  c.header('Cache-Control', 'no-store')
  try {
    let cached = quotaCache.get(provider.id)
    if (
      !cached ||
      cached.revision !== provider.revision ||
      cached.until <= Date.now()
    ) {
      cached = {
        revision: provider.revision,
        until: Date.now() + 30_000,
        promise: fetchProviderQuota(provider.baseUrl, provider.apiKey),
      }
      quotaCache.set(provider.id, cached)
    }
    return c.json({ windows: await cached.promise })
  } catch {
    return c.json({ windows: [] })
  }
})
// A self-declared UUID is not sufficient to disclose a provider secret.
// Require a configured web access key (validated by uuidAuth), or an API key.
app.get('/providers/:id/secret', uuidAuth, c => {
  if (!config.webAccessKey && !validateApiKey(extractBearerToken(c))) {
    return c.json({ error: { message: '查看密钥需要配置 Web 访问密钥' } }, 403)
  }
  const provider = readProvider(c.req.param('id')!)
  if (!provider) return c.json({ error: { message: '供应商不存在' } }, 404)
  c.header('Cache-Control', 'no-store')
  return c.json({ apiKey: provider.apiKey })
})
app.post('/providers', uuidAuth, async c => {
  const body = parseProvider(await c.req.json().catch(() => null))
  if (!body || !body.apiKey?.trim())
    return c.json(
      { error: { message: '请填写有效的供应商配置和 API Key' } },
      400,
    )
  return c.json({ provider: saveProvider(body) }, 201)
})
app.put('/providers/:id', uuidAuth, async c => {
  const id = c.req.param('id')!
  const body = parseProvider(await c.req.json().catch(() => null))
  if (!body || (body.apiKey !== undefined && !body.apiKey.trim()))
    return c.json({ error: { message: '供应商配置无效' } }, 400)
  const previous = readProvider(id)
  if (!previous) return c.json({ error: { message: '供应商不存在' } }, 404)
  // Never send a retained secret to a newly supplied endpoint without the
  // operator explicitly providing that secret again.
  if (
    !body.apiKey &&
    (body.baseUrl !== previous.baseUrl || body.protocol !== previous.protocol)
  )
    return c.json(
      { error: { message: '修改接口地址或协议时请重新填写 API Key' } },
      400,
    )
  return c.json({ provider: saveProvider(body, id) })
})
app.delete('/providers/:id', uuidAuth, c => {
  if (!deleteProvider(c.req.param('id')!))
    return c.json({ error: { message: '供应商不存在' } }, 404)
  return c.json({ ok: true })
})

// Only the CLI/device credential may read a key. The browser's UUID is not
// authentication for this endpoint, even when the web UI has no login.
app.get('/provider-credentials/:id', c => {
  const token = extractBearerToken(c)
  if (!token || (!validateApiKey(token) && !authenticateDeviceToken(token))) {
    return c.json({ error: { message: '设备凭据无效' } }, 401)
  }
  const provider = readProvider(c.req.param('id')!)
  if (!provider) return c.json({ error: { message: '供应商不存在' } }, 404)
  c.header('Cache-Control', 'no-store')
  return c.json({ provider })
})

export default app
