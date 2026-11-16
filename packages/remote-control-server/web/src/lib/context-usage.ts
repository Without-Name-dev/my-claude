import type { SessionUsage } from './rcs-chat-adapter'

function formatTokens(value: number): string {
  if (value >= 1_000_000) return `${Number((value / 1_000_000).toFixed(1))}M`
  if (value >= 1_000) return `${Number((value / 1_000).toFixed(1))}K`
  return String(value)
}

/** Show the latest prompt's context, never the session's cumulative usage. */
export function formatContextUsage(
  usage: SessionUsage | null,
  contextWindow: number | undefined,
): string | null {
  const limit = contextWindow && contextWindow > 0 ? contextWindow : null
  const current = usage?.current
  const estimated = usage?.estimated || usage?.contextEstimated ? '约 ' : ''
  let context: string | null = limit ? formatTokens(limit) : null
  if (current) {
    const used = current.input + current.cacheCreation + current.cacheRead
    context = `${estimated}${formatTokens(used)} / ${limit ? formatTokens(limit) : '上限未知'}`
  }
  if (!context) return null
  const cache = usage?.cacheHitRate
  if (
    typeof cache !== 'number' ||
    !Number.isFinite(cache) ||
    cache < 0 ||
    cache > 1
  )
    return context
  return `${context} · ${usage?.estimated ? '约 ' : ''}${Math.round(cache * 100)}%`
}
