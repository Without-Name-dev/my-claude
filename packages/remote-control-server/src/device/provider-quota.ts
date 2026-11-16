/** Supported quota providers are fixed origins; never forward a key to a supplied URL. */
export type QuotaWindow = { label: string; remaining: number; resetAt?: number }

export function quotaEndpoint(baseUrl: string): string | undefined {
  let url: URL
  try {
    url = new URL(baseUrl)
  } catch {
    return undefined
  }
  if (url.protocol !== 'https:' || url.port || url.username || url.password)
    return undefined
  if (
    !['open.bigmodel.cn', 'dev.bigmodel.cn', 'api.z.ai'].includes(url.hostname)
  )
    return undefined
  return `https://${url.hostname}/api/monitor/usage/quota/limit`
}

export function parseQuota(data: unknown): QuotaWindow[] {
  if (!data || typeof data !== 'object') return []
  const body = data as Record<string, unknown>
  if (body.success === false) return []
  const value =
    body.data && typeof body.data === 'object'
      ? (body.data as Record<string, unknown>)
      : body
  if (!Array.isArray(value.limits)) return []
  const tokenLimits = value.limits.filter(
    (v: unknown) =>
      v &&
      typeof v === 'object' &&
      (v as Record<string, unknown>).type === 'TOKENS_LIMIT',
  ) as Record<string, unknown>[]
  return tokenLimits.flatMap(limit => {
    const percentage = limit.percentage
    if (
      typeof percentage !== 'number' ||
      !Number.isFinite(percentage) ||
      percentage < 0 ||
      percentage > 100
    )
      return []
    const label =
      limit.unit === 3 && limit.number === 5
        ? '5 小时'
        : limit.unit === 6
          ? '每周'
          : tokenLimits.length === 1 && limit.unit === undefined
            ? '额度'
            : undefined
    if (!label) return []
    const reset =
      typeof limit.nextResetTime === 'number' && limit.nextResetTime > 0
        ? limit.nextResetTime
        : undefined
    return [
      {
        label,
        remaining: Math.round(100 - percentage),
        ...(reset ? { resetAt: reset } : {}),
      },
    ]
  })
}

export async function fetchProviderQuota(
  baseUrl: string,
  apiKey: string,
): Promise<QuotaWindow[]> {
  const endpoint = quotaEndpoint(baseUrl)
  if (!endpoint || !apiKey) return []
  const response = await fetch(endpoint, {
    headers: { Authorization: apiKey, Accept: 'application/json' },
    redirect: 'error',
    signal: AbortSignal.timeout(5000),
  })
  if (!response.ok) return []
  return parseQuota(await response.json())
}
