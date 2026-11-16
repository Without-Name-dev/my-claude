import { expect, test } from 'bun:test'
import { parseQuota, quotaEndpoint } from '../device/provider-quota'

test('allows only known HTTPS quota origins', () => {
  expect(quotaEndpoint('https://open.bigmodel.cn/api/anthropic')).toBe(
    'https://open.bigmodel.cn/api/monitor/usage/quota/limit',
  )
  expect(
    quotaEndpoint('https://open.bigmodel.cn.attacker.test/api'),
  ).toBeUndefined()
  expect(quotaEndpoint('http://open.bigmodel.cn/api')).toBeUndefined()
  expect(quotaEndpoint('https://open.bigmodel.cn:444/api')).toBeUndefined()
})

test('shows only validated token windows, never MCP or unknown limits', () => {
  expect(
    parseQuota({
      data: {
        limits: [
          {
            type: 'TOKENS_LIMIT',
            unit: 3,
            number: 5,
            percentage: 25,
            nextResetTime: 2000000000000,
          },
          { type: 'TOKENS_LIMIT', unit: 6, number: 1, percentage: 66 },
          { type: 'TIME_LIMIT', unit: 3, number: 5, percentage: 99 },
          { type: 'TOKENS_LIMIT', unit: 3, number: 5, percentage: 120 },
        ],
      },
    }),
  ).toEqual([
    { label: '5 小时', remaining: 75, resetAt: 2000000000000 },
    { label: '每周', remaining: 34 },
  ])
  expect(parseQuota({ success: false, data: { limits: [] } })).toEqual([])
})
