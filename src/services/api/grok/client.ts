import OpenAI from 'openai'
import { getProxyFetchOptions } from 'src/utils/proxy.js'
import { getServerProvider } from 'src/utils/model/serverProviders.js'

/**
 * Environment variables:
 *
 * GROK_API_KEY (or XAI_API_KEY): Required. API key for the xAI Grok endpoint.
 * GROK_BASE_URL: Optional. Defaults to https://api.x.ai/v1.
 */

const DEFAULT_BASE_URL = 'https://api.x.ai/v1'

let cachedClient: OpenAI | null = null

export function getGrokClient(options?: {
  maxRetries?: number
  fetchOverride?: typeof fetch
  source?: string
}): OpenAI {
  const remote = getServerProvider()
  if (remote) {
    if (remote.protocol !== 'grok') throw new Error('不匹配的供应商协议')
    return new OpenAI({
      apiKey: remote.apiKey,
      baseURL: remote.baseUrl,
      maxRetries: options?.maxRetries ?? 0,
      timeout: parseInt(process.env.API_TIMEOUT_MS || String(600 * 1000), 10),
      dangerouslyAllowBrowser: true,
      fetchOptions: getProxyFetchOptions({
        forAnthropicAPI: false,
        url: remote.baseUrl,
      }),
      ...(options?.fetchOverride && { fetch: options.fetchOverride }),
    })
  }
  if (cachedClient) return cachedClient

  const apiKey = process.env.GROK_API_KEY || process.env.XAI_API_KEY || ''
  const baseURL = process.env.GROK_BASE_URL || DEFAULT_BASE_URL

  const client = new OpenAI({
    apiKey,
    baseURL,
    maxRetries: options?.maxRetries ?? 0,
    timeout: parseInt(process.env.API_TIMEOUT_MS || String(600 * 1000), 10),
    dangerouslyAllowBrowser: true,
    fetchOptions: getProxyFetchOptions({ forAnthropicAPI: false }),
    ...(options?.fetchOverride && { fetch: options.fetchOverride }),
  })

  if (!options?.fetchOverride) {
    cachedClient = client
  }

  return client
}

export function clearGrokClientCache(): void {
  cachedClient = null
}
