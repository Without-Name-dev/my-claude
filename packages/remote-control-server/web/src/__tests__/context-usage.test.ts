import { describe, expect, test } from 'bun:test'
import { formatContextUsage } from '../lib/context-usage'
import type { SessionUsage } from '../lib/rcs-chat-adapter'

const usage: SessionUsage = {
  current: {
    input: 18_000,
    output: 1_000,
    cacheCreation: 20_000,
    cacheRead: 100_000,
  },
  totals: { input: 7_100_000, output: 20_000 },
  cacheHitRate: 0.99,
  estimated: false,
}

describe('current context display', () => {
  test('shows current prompt including cache, without cumulative tokens', () => {
    expect(formatContextUsage(usage, 200_000)).toBe('138K / 200K · 99%')
  })

  test('labels estimated history and handles missing information', () => {
    expect(formatContextUsage({ ...usage, estimated: true }, 200_000)).toBe(
      '约 138K / 200K · 约 99%',
    )
    expect(formatContextUsage(null, 200_000)).toBe('200K')
    expect(formatContextUsage(usage, undefined)).toBe('138K / 上限未知 · 99%')
    expect(formatContextUsage(null, undefined)).toBeNull()
    expect(
      formatContextUsage({ ...usage, contextEstimated: true }, 200_000),
    ).toBe('约 138K / 200K · 99%')
  })

  test('shows zero cache hit rate, and does not invent a missing rate', () => {
    expect(formatContextUsage({ ...usage, cacheHitRate: 0 }, 200_000)).toBe(
      '138K / 200K · 0%',
    )
    expect(formatContextUsage({ ...usage, cacheHitRate: null }, 200_000)).toBe(
      '138K / 200K',
    )
    expect(formatContextUsage({ ...usage, current: null }, 200_000)).toBe(
      '200K · 99%',
    )
  })
})
